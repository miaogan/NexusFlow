-- ============================================================================
-- NexusFlow 企业级演进增量数据库 Schema (v1.1.0 - v1.5.0)
-- 目标数据库: PostgreSQL 16+ (已启用 pgvector 扩展)
--
-- 设计约定:
--   1. 所有业务表强制包含 tenant_id 字段实现多租户逻辑隔离
--   2. 严格遵循幂等性 (IF NOT EXISTS)，支持服务启动时自动迁移执行
--   3. 敏感凭据字段 (_encrypted / _hash) 严禁明文存储
--
-- 与当前实现的衔接:
--   * knowledge_bases / kb_permissions 已落地：identity 包启动时以 advisory lock 幂等建表，
--     并将旧 identity_knowledge_bases 数据一次性迁入 knowledge_bases（旧表仅保留读取兼容）。
--   * 租户配额目前仍由 internal/metering 在 Redis / 内存中计量，tenant_quotas 是目标持久化
--     模型，尚未落地；接入时需明确 Postgres 与 Redis 的主从关系，避免计数漂移。
--   * custom_agents（v1.2.0）已落地：Agent Hub 在 AGENT_HUB_REGISTRY_DRIVER=postgres 时
--     启动幂等建表，用于租户级自定义 SubAgent；内置 Agent 仅存于进程内注册表。
--   * workflow_executions / workflow_approvals 已落地：Go workflow-engine 在
--     WORKFLOW_RUN_STORE_DRIVER=postgres 时启动幂等建表；挂起快照写入 context_snapshot，
--     审批单以 (execution_id, node_id) 派生的稳定 ID 幂等 upsert，审批并发通过行锁 +
--     状态 CAS 保证只有一次决策生效。Run 状态取值为 RUNNING / WAITING_APPROVAL /
--     COMPLETED / FAILED / REJECTED。
--   * audit_logs 已落地：Agent Hub 在 AGENT_HUB_AUDIT_DRIVER=postgres 时启动幂等建表，
--     新表按 created_at 月分区（audit_logs_pYYYYMM），启动与每小时维护会预建当月与次月
--     分区，并按 AGENT_HUB_AUDIT_RETENTION_DAYS（默认 180，0 表示永久保留）整月 DROP；
--     记录工具调用与审批事件；Agent Hub 的 HITL 工具审批使用本文件末尾的
--     agent_approvals（工作流审批仍走 workflow_approvals），两者不要混用。
--   * data_connectors 已落地：ingestion 在 CONNECTOR_DRIVER=postgres 时启动幂等建表，
--     配置以 AES-256-GCM 加密后写入 config_encrypted（CONNECTOR_ENCRYPTION_KEY 为空时
--     以 plain: 前缀存明文，仅限开发），sync_cursor 保存增量水位线。
--   * document_media_assets 等其余表仍为规划草案，代码中尚未建表。
--   * 所有状态/枚举字段以字符串存储，应用层常量必须与本文件的取值保持一致。
--   * 已存在的非分区 audit_logs 不会被自动改写：需手工迁移为分区表后，保留策略才会
--     按整月 DROP；在此之前退化为 DELETE 过期行。
-- ============================================================================

-- ----------------------------------------------------------------------------
-- Milestone 1: 知识库生命周期管理与精细化权限 (v1.1.0)
-- ----------------------------------------------------------------------------

-- 1. 知识库实体表
CREATE TABLE IF NOT EXISTS knowledge_bases (
    id            VARCHAR(64) PRIMARY KEY,
    tenant_id     VARCHAR(64) NOT NULL,
    name          VARCHAR(128) NOT NULL,
    description   TEXT DEFAULT '',
    embedding_model VARCHAR(128) DEFAULT 'default',
    chunk_strategy JSONB DEFAULT '{"type": "semantic", "chunk_size": 512, "overlap": 64}'::jsonb,
    status        VARCHAR(32) NOT NULL DEFAULT 'ACTIVE', -- ACTIVE, ARCHIVED, DELETING
    created_by    VARCHAR(64) NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, name)
);

CREATE INDEX IF NOT EXISTS idx_kb_tenant_status
    ON knowledge_bases (tenant_id, status);

-- 2. 知识库细粒度权限绑定表 (支持对用户或角色授权)
CREATE TABLE IF NOT EXISTS kb_permissions (
    id            VARCHAR(64) PRIMARY KEY,
    tenant_id     VARCHAR(64) NOT NULL,
    kb_id         VARCHAR(64) NOT NULL REFERENCES knowledge_bases(id) ON DELETE CASCADE,
    subject_type  VARCHAR(16) NOT NULL, -- 'user' 或 'role'
    subject_id    VARCHAR(64) NOT NULL, -- user_id 或 role_name
    permission    VARCHAR(32) NOT NULL, -- 'kb:admin', 'kb:editor', 'kb:viewer'
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (kb_id, subject_type, subject_id)
);

CREATE INDEX IF NOT EXISTS idx_kb_perm_lookup
    ON kb_permissions (tenant_id, subject_type, subject_id);

-- ----------------------------------------------------------------------------
-- Milestone 2: 多模态与外部数据源连接器 (v1.2.0)
-- ----------------------------------------------------------------------------

-- 3. 外部数据源连接器配置表（web_crawler / s3_bucket / db_query 增量同步）
CREATE TABLE IF NOT EXISTS data_connectors (
    id               VARCHAR(64) PRIMARY KEY,
    tenant_id        VARCHAR(64) NOT NULL,
    kb_id            VARCHAR(64) NOT NULL REFERENCES knowledge_bases(id) ON DELETE CASCADE,
    name             VARCHAR(128) NOT NULL,
    connector_type   VARCHAR(32) NOT NULL, -- 'web_crawler', 's3_bucket', 'db_query'
    config_encrypted TEXT NOT NULL,        -- AES-GCM 加密后的连接配置 JSON (含 URL/凭证/SQL)
    schedule_cron    VARCHAR(64) DEFAULT '',
    sync_cursor      JSONB DEFAULT '{}'::jsonb, -- 增量同步水位线 (如 last_modified / max_id)
    status           VARCHAR(32) NOT NULL DEFAULT 'IDLE', -- IDLE, SYNCING, ERROR, DISABLED
    last_sync_at     TIMESTAMPTZ,
    last_error       TEXT DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_connectors_tenant_kb
    ON data_connectors (tenant_id, kb_id);

-- 3.1 连接器定时 run 抢占表（多副本调度去重，按「连接器 + 分钟」唯一）
CREATE TABLE IF NOT EXISTS connector_schedule_claims (
    connector_id VARCHAR(64) NOT NULL,
    run_minute   TIMESTAMPTZ NOT NULL,
    claimed_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (connector_id, run_minute)
);

CREATE INDEX IF NOT EXISTS idx_connector_claims_minute
    ON connector_schedule_claims (run_minute);

-- 4. 文档多模态媒体资产映射表 (存储 MinerU 提取的图表/截图元数据)
CREATE TABLE IF NOT EXISTS document_media_assets (
    id          VARCHAR(64) PRIMARY KEY,
    tenant_id   VARCHAR(64) NOT NULL,
    kb_id       VARCHAR(64) NOT NULL,
    doc_id      VARCHAR(64) NOT NULL,
    chunk_id    VARCHAR(64) NOT NULL,
    media_type  VARCHAR(16) NOT NULL, -- 'image', 'table_snapshot'
    object_key  VARCHAR(256) NOT NULL, -- MinIO 对象路径
    caption     TEXT DEFAULT '',       -- 图表标题或 VLM 生成的描述
    page_number INT DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_media_assets_chunk
    ON document_media_assets (tenant_id, chunk_id);

CREATE INDEX IF NOT EXISTS idx_media_assets_doc
    ON document_media_assets (tenant_id, doc_id);

-- ----------------------------------------------------------------------------
-- Milestone 3: 高级工作流控制流与人工介入 (v1.3.0)
-- ----------------------------------------------------------------------------

-- 5. 工作流执行实例快照表 (支持人工审批挂起与恢复)
CREATE TABLE IF NOT EXISTS workflow_executions (
    execution_id     VARCHAR(64) PRIMARY KEY,
    tenant_id        VARCHAR(64) NOT NULL,
    workflow_id      VARCHAR(64) NOT NULL,
    status           VARCHAR(32) NOT NULL, -- RUNNING, WAITING_APPROVAL, COMPLETED, FAILED, REJECTED
    current_node_id  VARCHAR(64) DEFAULT '',
    context_snapshot JSONB NOT NULL DEFAULT '{}'::jsonb, -- 节点输出与变量状态快照
    triggered_by     VARCHAR(64) NOT NULL,
    started_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at      TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_wf_exec_tenant_status
    ON workflow_executions (tenant_id, status);

-- 6. 人工审批任务单表 (Human-in-the-Loop)
CREATE TABLE IF NOT EXISTS workflow_approvals (
    id              VARCHAR(64) PRIMARY KEY,
    tenant_id       VARCHAR(64) NOT NULL,
    execution_id    VARCHAR(64) NOT NULL REFERENCES workflow_executions(execution_id) ON DELETE CASCADE,
    node_id         VARCHAR(64) NOT NULL,
    assignee_role   VARCHAR(64) DEFAULT '', -- 指定审批角色 (如 admin / editor)
    prompt_message  TEXT DEFAULT '',        -- 展示给审批人的提示与上下文摘要
    payload_preview JSONB DEFAULT '{}'::jsonb,
    decision        VARCHAR(32) NOT NULL DEFAULT 'PENDING', -- PENDING, APPROVED, REJECTED
    reviewer_id     VARCHAR(64) DEFAULT '',
    review_comment  TEXT DEFAULT '',
    expires_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at     TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_wf_approvals_pending
    ON workflow_approvals (tenant_id, decision, created_at DESC);

-- ----------------------------------------------------------------------------
-- Milestone 4: 企业级安全合规与大模型网关治理 (v1.4.0)
-- ----------------------------------------------------------------------------

-- 7. 企业级合规审计日志表 (建议生产环境按月做分区)
CREATE TABLE IF NOT EXISTS audit_logs (
    id               VARCHAR(64) NOT NULL,
    tenant_id        VARCHAR(64) NOT NULL,
    user_id          VARCHAR(64) NOT NULL,
    action           VARCHAR(64) NOT NULL, -- 如 'file.delete', 'kb.permission.update', 'chat.query'
    resource_type    VARCHAR(32) NOT NULL, -- 'knowledge_base', 'document', 'user', 'api_key'
    resource_id      VARCHAR(64) DEFAULT '',
    client_ip        VARCHAR(64) DEFAULT '',
    trace_id         VARCHAR(64) DEFAULT '',
    pii_masked_count INT DEFAULT 0,        -- 本次请求触发的 PII 脱敏命中次数
    status_code      INT NOT NULL,
    metadata         JSONB DEFAULT '{}'::jsonb,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (id, created_at)           -- 分区表主键必须包含分区键
) PARTITION BY RANGE (created_at);

-- 分区由 Agent Hub 启动/维护时自动创建（以下为当前月示例，YYYYMM 按需替换）
-- CREATE TABLE IF NOT EXISTS audit_logs_p202611 PARTITION OF audit_logs
--     FOR VALUES FROM ('2026-11-01T00:00:00Z') TO ('2026-12-01T00:00:00Z');

CREATE INDEX IF NOT EXISTS idx_audit_logs_tenant_time
    ON audit_logs (tenant_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_audit_logs_action
    ON audit_logs (tenant_id, action);

-- 8. 租户级资源配额与用量统计表
CREATE TABLE IF NOT EXISTS tenant_quotas (
    tenant_id           VARCHAR(64) PRIMARY KEY,
    max_storage_bytes   BIGINT NOT NULL DEFAULT 10737418240, -- 默认 10 GiB
    used_storage_bytes  BIGINT NOT NULL DEFAULT 0,
    daily_token_limit   BIGINT NOT NULL DEFAULT 1000000,    -- 默认每日 100 万 Token
    daily_tokens_used   BIGINT NOT NULL DEFAULT 0,
    quota_reset_at      TIMESTAMPTZ NOT NULL DEFAULT (DATE_TRUNC('day', NOW()) + INTERVAL '1 day'),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ----------------------------------------------------------------------------
-- Milestone 5: 生态集成与对外服务能力 (v1.5.0)
-- ----------------------------------------------------------------------------

-- 9. 对外开放平台 API Key 表 (绑定特定租户与允许访问的知识库白名单)
CREATE TABLE IF NOT EXISTS api_keys (
    id             VARCHAR(64) PRIMARY KEY,
    tenant_id      VARCHAR(64) NOT NULL,
    name           VARCHAR(128) NOT NULL,
    key_prefix     VARCHAR(16) NOT NULL,  -- 用于前端展示前缀，如 'nf-sk-8a9b...'
    key_hash       VARCHAR(128) NOT NULL UNIQUE, -- SHA-256 哈希值
    allowed_kb_ids JSONB DEFAULT '[]'::jsonb,    -- 允许检索的 kb_id 列表，空数组代表租户内全量
    scopes         JSONB DEFAULT '["chat:completions", "search:use"]'::jsonb,
    rate_limit_rpm INT NOT NULL DEFAULT 60,
    status         VARCHAR(16) NOT NULL DEFAULT 'ACTIVE', -- ACTIVE, REVOKED
    expires_at     TIMESTAMPTZ,
    last_used_at   TIMESTAMPTZ,
    created_by     VARCHAR(64) NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_api_keys_tenant
    ON api_keys (tenant_id, status);

-- 10. 租户级事件 Webhook 订阅表
CREATE TABLE IF NOT EXISTS webhooks (
    id               VARCHAR(64) PRIMARY KEY,
    tenant_id        VARCHAR(64) NOT NULL,
    name             VARCHAR(128) NOT NULL,
    target_url       TEXT NOT NULL,
    secret_encrypted TEXT NOT NULL, -- 用于计算 X-NexusFlow-Signature (HMAC-SHA256)
    events           JSONB NOT NULL DEFAULT '["task.completed", "task.failed"]'::jsonb,
    is_active        BOOLEAN NOT NULL DEFAULT TRUE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_webhooks_tenant_active
    ON webhooks (tenant_id, is_active);

-- 11. Webhook 投递死信与重试日志表
CREATE TABLE IF NOT EXISTS webhook_deliveries (
    id            VARCHAR(64) PRIMARY KEY,
    tenant_id     VARCHAR(64) NOT NULL,
    webhook_id    VARCHAR(64) NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
    event_type    VARCHAR(64) NOT NULL,
    payload       JSONB NOT NULL,
    attempt_count INT NOT NULL DEFAULT 1,
    http_status   INT DEFAULT 0,
    response_body TEXT DEFAULT '',
    status        VARCHAR(16) NOT NULL DEFAULT 'PENDING', -- PENDING, SUCCESS, FAILED, REPLAYED
    next_retry_at TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_retry
    ON webhook_deliveries (status, next_retry_at)
    WHERE status = 'PENDING';

CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_status
    ON webhook_deliveries (tenant_id, webhook_id, status, created_at DESC);

-- ----------------------------------------------------------------------------
-- v1.2.0: 自定义 SubAgent 注册表 (Agent Hub)
-- ----------------------------------------------------------------------------

-- 12. 租户级自定义 SubAgent（HTTP / Sandbox spoke）
CREATE TABLE IF NOT EXISTS custom_agents (
    tenant_id      VARCHAR(64) NOT NULL,
    name           VARCHAR(64) NOT NULL,
    capabilities   JSONB NOT NULL DEFAULT '[]'::jsonb,
    kind           VARCHAR(16) NOT NULL DEFAULT 'http',
    endpoint       VARCHAR(512) NOT NULL DEFAULT '',
    timeout_ms     INTEGER NOT NULL DEFAULT 5000,
    priority       INTEGER NOT NULL DEFAULT 100,
    ttl_seconds    INTEGER NOT NULL DEFAULT 0,
    version        VARCHAR(32) NOT NULL DEFAULT '1.0',
    input_schema   JSONB NOT NULL DEFAULT '{}'::jsonb,
    output_schema  JSONB NOT NULL DEFAULT '{}'::jsonb,
    permissions    JSONB NOT NULL DEFAULT '[]'::jsonb,
    tenant_scope   VARCHAR(128) NOT NULL DEFAULT '',
    tools          JSONB NOT NULL DEFAULT '[]'::jsonb,
    skills         JSONB NOT NULL DEFAULT '[]'::jsonb, -- 动态技能路由名，如 ["expense.approval"]
    sandbox        JSONB NOT NULL DEFAULT '{}'::jsonb, -- {code, runtime, timeout_seconds, packages, env_vars}
    model          VARCHAR(128) NOT NULL DEFAULT '',
    max_turns      INTEGER NOT NULL DEFAULT 0,
    token_budget   INTEGER NOT NULL DEFAULT 0,
    risk           VARCHAR(16) NOT NULL DEFAULT 'read',
    created_by     VARCHAR(64) NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, name)
);

CREATE INDEX IF NOT EXISTS custom_agents_tenant_idx
    ON custom_agents (tenant_id, updated_at DESC);

-- 13. Agent Hub 工具调用审批单（Human-in-the-Loop）
-- 与 workflow_approvals 的区别：本表服务于工具/SubAgent 的高风险调用，
-- 不绑定工作流执行实例；状态取值为 pending / approved / rejected。
CREATE TABLE IF NOT EXISTS agent_approvals (
    id          VARCHAR(64) PRIMARY KEY,
    tenant_id   VARCHAR(64) NOT NULL,
    tool        VARCHAR(128) NOT NULL,
    args        JSONB NOT NULL DEFAULT '{}'::jsonb,
    risk        VARCHAR(16) NOT NULL DEFAULT 'read',
    user_id     VARCHAR(64) NOT NULL DEFAULT '',
    trace_id    VARCHAR(64) NOT NULL DEFAULT '',
    status      VARCHAR(16) NOT NULL DEFAULT 'pending',
    reason      TEXT NOT NULL DEFAULT '',
    decided_by  VARCHAR(64) NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    decided_at  TIMESTAMPTZ,
    expires_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS agent_approvals_pending_idx
    ON agent_approvals (tenant_id, status, created_at DESC);
