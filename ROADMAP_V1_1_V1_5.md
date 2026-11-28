# NexusFlow v1.1.0 – v1.5.0 企业级能力演进方案（ROADMAP_V1_1_V1_5）

> **定位**：本文件是面向 v1.1.0 至 v1.5.0 的能力演进方案，共 5 个 Milestone、11 个结构化任务；已全部交付。
> **2026-11-27 增补**：Agent Hub × Workflow × MCP × HITL 已按规划文档 `.trae/documents/agent-hub-workflow-mcp-hitl.md` 交付；Task 3.2 完成（运行状态持久化由 Task 3.3 补齐），Task 5.2 的 MCP Server 部分提前落地，其余任务状态不变。
> **v1.2.0 已交付**：自定义 SubAgent（存储 + 多租户注册表 + SandboxAgentExecutor + 动态技能路由 + `/subagents` 控制台）已按 [ROADMAP_V1_2.md](ROADMAP_V1_2.md) Milestone 1–3 完成。本文件 Milestone 2（多模态、Connector）未能进入该版本，顺延为待做项，其原 v1.2.0 版本标签不再使用。
> **与 `ROADMAP_V1_1.md` 的关系**：`ROADMAP_V1_1.md` 是上一轮已执行完毕的交付清单（知识库实体、级联删除、配额计量、K8s HPA，对应提交 c880e64 / 44b9284 / 9bc2c01 / a244bd7）。本文件在此基础上重新规划更完整的企业级能力，Milestone 1 为增量升级。
> **执行约定**：按 Milestone 顺序逐个执行，禁止跨任务并发；每个任务验证通过后提交独立 Commit，并将任务状态更新为 `[x]`；接口变更优先更新 proto 契约，存储扩展保留 Memory/Mock 降级路径。
> **数据契约**：涉及数据库表结构新增或变更时，必须严格对齐 [docs/schema_enterprise_v1_1_to_1_5.sql](docs/schema_enterprise_v1_1_to_1_5.sql) 中的表名、字段名与状态枚举值，并在 Postgres 初始化逻辑中保证幂等建表。

## 当前待做总览

Milestone 1–5 的 11 个任务已全部交付，无待做项。

---

## Milestone 1: 知识库生命周期管理与精细化权限（v1.1.0）

* **[x] Task 1.1: 实现 kb_id 维度的独立生命周期与多维度 RBAC 权限隔离（kb:admin / kb:editor / kb:viewer），改造网关鉴权与前端切换器**
  * 历史现状：基础版 `kb_id` 隔离与知识库 CRUD 已交付（c880e64），此前仅有 `kb:read` / `kb:write` 两档权限。
  * 交付情况：已新增 `knowledge_bases` / `kb_permissions`（旧 `identity_knowledge_bases` 自动迁移），支持按 `kb_id` 授予 `kb:admin` / `kb:editor` / `kb:viewer` 并按用户或角色绑定；网关在文件上传/删除/重建、检索问答与知识库管理入口按角色校验，前端切换器展示当前库角色并限制操作。

* **[x] Task 1.2: 文档全生命周期治理，支持分布式级联物理删除（Qdrant 向量点、LightRAG 子图、对象存储源文件）及无感重新索引（Reindex）**
  * 现状：级联删除已交付（44b9284），覆盖 Qdrant 分片、图谱缓存失效与对象文件清理；Reindex 已交付：解析产物持久化、REINDEXING 状态、稳定分片 ID 与先写后清理。
  * 范围调整（已确认）：LightRAG 独立持久化图库暂不做，当前以“删除向量 + 失效图缓存”等效子图清理，后续单独立项。

---

## Milestone 2: 多模态与外部数据源连接器（已全部交付）

* **[x] Task 2.1: 释放 MinerU 图文多模态解析能力，在 RAG 问答中支持图文联合检索、图表缩略图渲染与点击放大**
  * 解析层保留 MinerU 输出的图片与版面信息，切块时建立图-文关联；检索结果返回图表资源引用，问答链路注入图片摘要；前端支持缩略图展示与点击放大。
  * 交付情况：MinerU 结果 ZIP 中的图片由 `mineru.ParseDocument` 提取（单图 8MB、总量 32MB、最多 64 张），Worker 落盘到 `<tenant>/<kb>/parsed/<task>/images/` 并把 Markdown 图片链接改写为 `/v1/media?key=...`；Gateway 新增 `GET /v1/media`（`search:use`，仅允许本租户 `parsed/*/images` 下的图片扩展名）；LlamaIndex 切块把 `image_urls` 写入节点 metadata 并随检索结果返回；Chat 上下文按 chunk 注入“本段配图”摘要；控制台检索结果与问答引用展示缩略图，点击全屏放大。
  * 边界：本期只复用 MinerU 的 Markdown 版面结构，未接入独立 layout JSON；图片以鉴权接口读取（前端 fetch 带 Bearer 转 blob），不走公开对象存储。

* **[x] Task 2.2: 内置统一 Connector 框架与开箱即用连接器，支持 web_crawler、s3_bucket 及 db_query 的增量同步与去重**
  * 抽象统一的 Connector 接口（发现 → 拉取 → 解析 → 入库 → 游标持久化），内置网页抓取、S3 桶与数据库查询三类连接器；以内容哈希与外部主键去重，按游标增量同步，失败可断点续传。
  * 交付情况：新增 `internal/connector`（`Source` 接口 + `web_crawler` / `s3_bucket` / `db_query` 实现），游标分别使用已抓 URL 集合、S3 对象 key 与 SQL OFFSET；`data_connectors` 支持 memory / Postgres 持久化，配置以 AES-256-GCM 加密，同步状态记录 `IDLE / SYNCING / ERROR` 与 `last_error`。同步时通过 ingestion 的 `RegisterContent` 写入对象存储并创建普通任务，沿用 MinerU → LlamaIndex 链路；内容 SHA-256 作为幂等键，未变化文档自动跳过。Gateway 新增 `GET/POST /v1/connectors`、`GET/DELETE /v1/connectors/{id}`、`POST /v1/connectors/{id}/sync`（`kb:write`）。
  * 边界：`db_query` 为只读 SELECT + OFFSET 分页，要求稳定 `ORDER BY`；`web_crawler` 默认拒绝私网/回环地址（`allow_private` 可放开），单页上限 5MB、最多 100 页；S3 单对象上限 20MB。`schedule_cron` 为 5 字段标准 cron，由 ingestion 内置调度器执行（`CONNECTOR_SCHEDULER_*`），多副本通过 `connector_schedule_claims` 按分钟抢占去重。

---

## Milestone 3: 高级工作流控制流与人工介入（已全部交付）

* **[x] Task 3.1: 升级 DAG 引擎，支持基于上下文的条件分支（condition）节点与并发迭代批处理（loop）节点**
  * 条件节点按上游上下文结果选择分支；loop 节点对集合元素并发执行子图并聚合输出；保持现有拓扑校验、层级并发与失败语义不回退。
  * 交付情况：新增 `condition` 节点（`field` 支持 `${node.field}` / `node.field` / 字面量，运算符 `equals` / `not_equals` / `in` / `contains`，缺省按真值判断），输出 `branch` 并按配置的 `then` / `else` 目标裁剪未选分支；引擎以“从被丢弃目标可达且从选中目标不可达”计算跳过集合，汇聚节点仍会执行，结果新增 `skipped` 字段并随 `RunState` 持久化。新增 `loop` 节点（`items` / `items_from`、`max_concurrency` 默认 4 上限 16、`continue_on_error`），对每个元素以 `item` / `index`（map 元素同时展开字段）并发执行 `sub_dag`，聚合 `count` / `failed` / `results`；子图校验复用 `Engine.Validate`，禁止嵌套 loop。
  * 边界：`condition` 的 `then` / `else` 必须是该节点下游（`/v1/workflows/validate` 校验），未选分支节点不再出现在 `results` 而进入 `skipped`；`loop` 子图运行不单独持久化，父 run 失败即整体失败（除非 `continue_on_error`）。控制流节点已注册到独立 workflow-engine 与 dev-all，`/v1/nodes` 可见。

* **[x] Task 3.2: 引入工作流人工介入机制（Human-in-the-Loop），支持 human_approval 审批挂起、上下文快照持久化及审批恢复**
  * 审批节点触发后持久化执行上下文并挂起实例；提供查询待办、批准/驳回接口；审批通过后从快照恢复继续执行，驳回则按失败或指定分支收尾。
  * 交付情况：新增 `human_approval` 节点、`MemoryRunStore` 检查点、`GET /v1/workflows/runs`、`POST /v1/workflows/runs/{id}/{approve,resume}`；状态对齐 schema 的 `COMPLETED / FAILED / REJECTED / WAITING_APPROVAL`，重复审批返回 409。同时交付 Hub 侧高风险工具审批（202 + `approval_id`、`/v1/approvals/{id}/decide`）。
  * 边界：检查点与审批记录当前为进程内存储，跨副本/重启恢复由 Task 3.3 承接。

* **[x] Task 3.3: 将工作流运行、审批与审计状态接入 Postgres，支持多副本恢复**
  * 按 `docs/schema_enterprise_v1_1_to_1_5.sql` 落库 `workflow_executions` / `workflow_approvals` / `audit_logs`，以 `RunStore` / `ApprovalService` 接口替换内存实现；明确同一 run 的副本粘性或乐观锁，保证审批并发与幂等；审计按月分区并支持保留策略。
  * 交付情况：Go 新增 `PostgresRunStore`（`WORKFLOW_RUN_STORE_DRIVER=postgres`），幂等建表并持久化挂起快照与审批单；审批通过 `SELECT ... FOR UPDATE` + 状态 CAS 原子抢占，跨副本并发只有一次决策生效，其余返回 409；运行快照额外记录 `approved_by` / `approval_reason`，审批单以 `(execution_id, node_id)` 稳定 ID 幂等 upsert，run 结束后自动置为 APPROVED / REJECTED。Hub 新增 `PostgresApprovalService`（`agent_approvals`）与 `PostgresAuditLog`（`audit_logs`），由 `AGENT_HUB_APPROVAL_DRIVER` / `AGENT_HUB_AUDIT_DRIVER` + `AGENT_HUB_STATE_DATABASE_URL` 开启；数据库不可用时回退内存并告警。
  * 验证：`go test ./internal/workflow`（含 `POSTGRES_TEST_DSN` 集成用例）与 `pytest tests/test_postgres_state.py`（`AGENT_HUB_TEST_DATABASE_URL`）均通过。
  * 边界：`audit_logs` 仍为单表，按月分区与保留策略留给运维脚本；Hub 审计写入为尽力而为（失败只告警），不阻断业务请求。

---

## Milestone 4: 企业级安全合规与大模型网关治理（已全部交付）

* **[x] Task 4.1: 实现 PII 敏感信息双向脱敏引擎与结构化合规审计日志中间件，支持安全追溯**
  * 入口对提示词与上传文本做 PII 识别与脱敏，出口对模型回复做还原或再次脱敏；审计日志记录操作者、资源、动作与脱敏命中项，支持按租户与时间检索。
  * 交付情况：新增 `core/pii.py`（邮箱 / 手机号 / 身份证 / 银行卡 / IPv4），按请求维护稳定占位符（`{{EMAIL_1}}` 等）与唯一命中计数；Chat 同步与 SSE 路径在模型边界脱敏提示词、历史、检索片段、摘要、记忆与纠正答案，检索仍使用原始 query；回复默认保持脱敏并二次扫描模型新生成的 PII，`RAG_PII_RESTORE_OUTPUT=true` 可还原。命中写入 `pii.mask` 审计事件（操作者、资源、`detail.types/count`），`GET /v1/audit` 新增 `since` / `until`（RFC3339 或 unix 秒）并支持 memory / Postgres 两种存储。
  * 边界：上传文档与对话历史保持原文存储，脱敏发生在进入模型的边界；SSE 逐 token 事件为实时输出，最终 `done.answer` 与持久化内容采用脱敏结果；`audit_logs` 按月分区与保留策略仍由运维脚本负责。

* **[x] Task 4.2: 建设多模型路由网关（主备自动故障转移）与租户级用量计量/配额管控（防 429 溢出）**
  * 统一 LLM 调用入口，按模型与租户路由至主/备通道，失败自动切换并熔断；复用并扩展已有计量能力，按租户累计 token 与调用量，超限前置拦截并避免上游 429 溢出。
  * 交付情况：新增独立 `cmd/llm-gateway`（`internal/llmgateway`），提供 OpenAI 兼容 `/v1/chat/completions`（含 SSE 透传与 usage 解析）与 `/v1/embeddings`；按 `LLM_PRIMARY_*` → `LLM_BACKUP_*` 顺序失败切换，网络错误 / 429 / 5xx 计入熔断（`LLM_BREAKER_THRESHOLD` / `LLM_BREAKER_COOLDOWN`）。按 `x-tenant-id` + UTC 日原子预留 token（提示词估算 + max_tokens），超限前置返回 `429 quota_exceeded`，响应后按实际 usage 修正；计数支持 `LLM_QUOTA_DRIVER=memory|redis`。Agent Hub 的 RAG 生成在 `generate` / `astream` / intent 路径透传 `x-tenant-id`。
  * 边界：模型路由以通道顺序为准（`LLM_*_MODEL` 可按通道覆盖模型名）；Intent/Summary/Memory 独立 Agent 若直连上游则不带租户头；Embedding 用量按请求体估算；配额按 UTC 自然日重置。

---

## Milestone 5: 生态集成与对外服务能力（v1.5.0）

* **[x] Task 5.1: 支持独立 API Key 鉴权体系，并提供兼容 OpenAI 协议的 /v1/openai/chat/completions 对外接入端点**
  * 新增租户级 API Key 的签发、吊销与作用域控制（可绑定知识库），与现有 JWT 鉴权并存；实现 OpenAI Chat Completions 兼容端点，支持流式与非流式响应、引用透传。
  * 交付情况：新增 `backend/internal/apikey`（`api_keys` 表，SHA-256 存储、展示前缀、`allowed_kb_ids` / `scopes` / `rate_limit_rpm` / 过期时间，memory 与 postgres 双实现）；网关鉴权同时接受 `nf-sk-*` 与 JWT，按 Key 独立限流；`/v1/api-keys` 提供签发/列表/吊销（`apikey:write`）；`/v1/openai/chat/completions` 支持流式 `chat.completion.chunk` + `[DONE]` 与非流式 `chat.completion`，引用在 message 与顶层透传，并透传 `system` / `temperature` / `max_tokens`；Hub 新增 `stateless` + `history`，外部请求不写会话库。
  * 边界：API Key 为只读集成凭证（`chat:completions` / `search:use`，前者隐含后者以支持检索），不可获得管理权限；OpenAI `system` / `developer` 消息拼接为 `system_prompt` 注入生成，`temperature`（0–2）与 `max_tokens`（兼容 `max_completion_tokens`，上限 32768）透传给上游模型；`usage` 优先采用上游真实 token 用量，未配置 LLM 时回退为字符估算（system prompt 计入 `prompt_tokens`），流式需显式开启 `stream_options.include_usage`。

* **[x] Task 5.2: 支持租户级异步事件 Webhook 订阅（HMAC 签名与指数退避重试）以及标准 MCP Server 适配模块**
  * 提供事件订阅管理（任务完成、索引完成、审批结果等），投递时携带 HMAC 签名，失败按指数退避重试并记录投递日志；实现标准 MCP Server，暴露检索、问答与工作流能力供外部 Agent 调用。
  * 进展：MCP Server 已落地（Agent Hub `POST /mcp` / `GET /mcp/sse` / stdio，统一支持 `initialize`、`tools/*`、`resources/*`、`prompts/*`；工具源为 `ToolRegistry`，资源暴露 agents / skills / tools 只读视图，提示词内置 `rag_answer` / `summarize` / `intent_classify`）。
  * 交付情况：新增 `backend/internal/webhook`（`webhooks` / `webhook_deliveries` 表，memory 与 postgres 双实现），订阅管理 `GET/POST /v1/webhooks`、`DELETE /v1/webhooks/{id}` 与投递日志 `GET /v1/webhooks/{id}/deliveries`（`webhook:write`）；投递使用 `X-NexusFlow-Signature: sha256=<HMAC>`，非 2xx 按指数退避重试（默认 5 次、基数 1s、上限 60s）并逐次落日志；提供测试投递、密钥轮换与历史载荷重放接口。事件源：网关审批决策产生 `approval.decided`；processing-worker 产生 `task.completed` / `task.failed` / `index.completed`；workflow-engine 产生 `workflow.completed` / `workflow.failed` 与节点级 `workflow.node.*`，后两者经 `GATEWAY_URL` + `WEBHOOK_EVENTS_TOKEN` 调用 `POST /v1/internal/events` 统一分发。
  * 边界：目标 URL 仅支持 `http(s)`；默认拒绝回环/私网/链路本地地址并在每次投递时重新解析（`WEBHOOK_ALLOW_PRIVATE` 可放宽），未做域名白名单；`WEBHOOK_EVENTS_TOKEN` 为空时内部事件接入禁用；Webhook 采用 HTTP 即时分发，未引入独立消息队列与死信重放。
