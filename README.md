# NexusFlow

分布式文件向量化工作流：**Go 编排调度 + MinerU 解析 + LlamaIndex 数据加工 + Agent Hub 多智能体 + OpenSandbox 执行**。

将 PDF / Word / TXT 等异构文件加工为可检索的向量与索引，提供多路召回、RRF 融合、Cross-Encoder 重排与自定义代码节点能力，服务下游 RAG 与语义检索场景。

完整设计见 [docs/architecture.md](docs/architecture.md)（系统架构设计文档 V4.0）。

**文档导航**：[用户使用指南](docs/user-guide.md) · [部署与运维](docs/deployment.md) · [系统架构](docs/architecture.md)

> 说明：设计文档标题为 V4.0，正文部分沿用了 V2.0 的演进叙述（“V2.0 新增 LlamaIndex 服务”等），属于保留的历史上下文，不影响当前架构与代码结构。

## 架构总览

```mermaid
flowchart TB
    subgraph A[接入层]
        GW[API Gateway<br/>JWT 校验 · 限流 · 权限路由 · trace 透传]
        AUTH[Auth / User Service<br/>JWT 签发 · 用户/角色 · kb RBAC]
        MET[Metering<br/>用量计量与配额]
    end

    subgraph B[编排调度层]
        WF[Workflow Engine<br/>DAG 校验 · 节点调度 · agent / human_approval]
        ING[Ingestion Service<br/>元数据 · 任务 · 删除 · Reindex]
    end

    subgraph C[执行加工层]
        PRC[Processing Worker]
        MINERU[MinerU<br/>结构化 Markdown]
        LIDX[LlamaIndex Service<br/>切块 · Embedding · 索引]
        SBX[Sandbox Execution]
    end

    subgraph D[检索问答层]
        QS[Query Service<br/>多路召回 · RRF · Rerank]
        AGH[Agent Hub<br/>星型调度 · 工具/推理 · MCP · HITL]
        INT[Intent Agent]
        SUM[Summary Agent]
        MEM[Memory Agent]
        EXT[MCP Server<br/>外部工具 / Agent]
    end

    subgraph E[存储层]
        MQ[(RabbitMQ / Kafka<br/>任务与事件 · DLQ)]
        OBJ[(MinIO / S3<br/>原文件 · parsed/)]
        VS[(Qdrant<br/>KB: dense+sparse<br/>memory: dense)]
        PG[(PostgreSQL<br/>身份 · 反馈 · 会话 · 记忆正文)]
        RD[(Redis<br/>任务状态 · 吊销 · 计量)]
    end

    UI[可视化画布 / 客户端] -->|HTTP / gRPC| GW
    GW --> AUTH
    GW --> MET
    GW --> WF
    GW --> ING
    GW --> QS
    GW --> AGH
    AGH -->|intent.classify| INT
    AGH -->|summary.compact| SUM
    AGH -->|memory.recall/extract| MEM
    AGH <-->|/mcp · tools/call| EXT
    WF -->|agent 节点 · /v1/call| AGH
    AGH -->|workflow.run| WF
    AUTH --> RD
    AUTH -->|身份/角色| PG
    MET --> RD
    WF -->|解析节点| PRC
    WF -->|代码节点| SBX
    ING -->|任务事件| MQ
    ING -->|原文件| OBJ
    ING -->|任务状态| RD
    MQ -->|消费| PRC
    PRC -->|解析| MINERU
    PRC -->|parsed/*.md| OBJ
    PRC -->|索引| LIDX
    LIDX -->|dense+sparse| VS
    QS -->|多路召回| LIDX
    QS -->|负反馈计数| PG
    AGH -. 复用 /v1/search .-> GW
    AGH -->|会话/记忆正文| PG
    AGH -. 记忆向量 .-> VS
```

分层职责：

- **接入层**：Gateway 统一鉴权（JWT / API Key → tenant_id + 权限点）、租户级限流、trace 透传；Auth / User 服务负责签发令牌、管理账号与全局角色，并按知识库解析 `kb:admin` / `kb:editor` / `kb:viewer` 权限；Metering 负责租户用量与配额。
- **编排调度层**：Workflow Engine 解析 DAG、按依赖并发调度；Ingestion 接收上传、连接器增量同步、级联删除并投递任务，Reindex 复用已持久化的解析产物。
- **执行加工层**：Processing Worker 调用 MinerU 解析并把 Markdown 持久化为 `tenant/kb/parsed/<task>.md`；LlamaIndex Service 负责语义/层级切块、索引与 Embedding；Sandbox Execution 经 OpenSandbox 执行自定义代码。
- **检索问答层**：Query Service 并行多路召回，RRF 融合，Cross-Encoder 重排，任一非核心路失败 Fail-open；Agent Hub 作为星型调度中心，按 capability 路由到 intent / summary / memory，经 Gateway 复用统一检索链路。
- **存储层**：RabbitMQ/Kafka 消息与 DLQ、MinIO 原文件与解析产物、Qdrant 文档向量（dense/sparse）与记忆向量（dense）、PostgreSQL 身份/反馈/会话/记忆正文、Redis 任务状态、计量与令牌吊销。

## 服务与目录

| 服务 | 语言 | 目录 | 端口 | 职责 |
| --- | --- | --- | --- | --- |
| API Gateway | Go | `backend/cmd/gateway` | 8080 | 鉴权（JWT / API Key）、限流、上传与检索入口、OpenAI 兼容端点、Webhook 分发 |
| Auth Service | Go | `backend/cmd/auth-service` | 9096 | 注册 / 登录 / 刷新 / 校验，签发 JWT |
| User Service | Go | `backend/cmd/user-service` | 9097 | 用户 CRUD、角色与权限（RBAC）管理 |
| Workflow Engine | Go | `backend/cmd/workflow-engine` | 8081 | DAG 校验与调度执行 |
| Ingestion Service | Go | `backend/cmd/ingestion` | 9095（Connector API 9098） | 上传登记、连接器同步、状态查询、任务投递 |
| Processing Worker | Go | `backend/cmd/processing-worker` | 9092 | 消费任务、调用 MinerU、发布解析结果 |
| Sandbox Execution | Go | `backend/cmd/sandbox-execution` | 9093 | 自定义代码节点强隔离执行 |
| LLM Gateway | Go | `backend/cmd/llm-gateway` | 8082 | OpenAI 兼容路由：主备切换、熔断、租户 token 配额 |
| Query Service | Go | `backend/cmd/query-service` | 9094 | 多路召回、RRF、重排、流式回答 |
| LlamaIndex Service | Python | `services/llamaindex` | 8000 | 切块、索引、Embedding、检索策略 |
| Agent Hub | Python | `services/agent-hub` | 8001（拆分 8002–8004） | 星型调度 + RAG 多轮问答；默认单容器内置 intent / summary / memory，可用 `ROLE` 拆分 |
| Web Console | React | `web` | 5173 / 8088 | 控制台：知识库切换与授权 / 文件 / 任务 / 检索 / 问答 / 沙箱 / DAG / 审批 / 审计 / API Key / Webhook |

```text
.
├── proto/vectorization/v1/        # Protobuf 契约（跨语言唯一事实来源）
├── backend/
│   ├── cmd/                       # 9 个可执行入口（含单进程 dev-all）
│   ├── internal/
│   │   ├── gateway/               # 鉴权、限流、HTTP 路由
│   │   ├── workflow/              # DAG 校验、拓扑调度、节点注册
│   │   ├── ingestion/             # 上传登记与任务投递
│   │   ├── processing/            # MinerU 解析 Worker
│   │   ├── sandbox/               # Local / OpenSandbox 双后端
│   │   ├── query/                 # 召回、RRF、Rerank
│   │   ├── identity/              # 用户/角色、知识库与 kb 级权限
│   │   ├── metering/              # 租户用量计量与配额（memory / Redis）
│   │   ├── state/                 # 状态机（memory / Redis）
│   │   ├── queue/                 # 消息总线（memory / RabbitMQ）
│   │   ├── llmgateway/            # OpenAI 兼容路由：主备切换、熔断、token 配额
│   │   └── platform/              # 日志、指标、HTTP/gRPC 服务封装
│   └── pb/                        # protoc 生成代码
├── services/llamaindex/           # FastAPI + LlamaIndex 数据加工服务
├── services/agent-hub/            # 单容器多 Agent：RAG + intent/summary/memory + 星型调度
│   └── src/nexusflow_agents/      # agents/*（业务）、core/{registry,hub,executor}（调度）
├── web/                           # React + TS 控制台（路由 + 多页面 + React Flow）
├── deploy/
│   ├── docker-compose.yml         # 完整本地编排
│   ├── k8s/nexusflow.yaml         # K8s 部署 + HPA 自定义指标
│   ├── opensandbox/               # OpenSandbox server（Docker 模式）示例配置
│   └── prometheus/                # 指标抓取配置
├── docs/architecture.md           # 架构设计文档（V4.0）
├── docs/deployment.md             # Kubernetes 生产部署指南
├── docs/schema_enterprise_v1_1_to_1_5.sql  # 企业版增量 DDL
└── Makefile
```

## 快速开始

### 1. 单进程跑通全链路

无需任何外部依赖，单进程即启动 Gateway（:8080）、Workflow Engine（:8081）与全部 gRPC 服务，使用内存消息总线 + Mock MinerU：

```bash
make run-dev-all
```

然后：

```bash
# 1) 上传真实文件（multipart）——文件写入对象存储并登记任务
curl -s http://127.0.0.1:8080/v1/files -F "file=@./README.md"

# 或仅登记元数据（文件由客户端直传对象存储时使用）
curl -s http://127.0.0.1:8080/v1/files \
  -H 'Content-Type: application/json' \
  -d '{"file_name":"demo.pdf","file_type":"pdf","file_size":1024}'

# 2) 任务列表（按状态过滤 / 分页）
curl -s "http://127.0.0.1:8080/v1/tasks?limit=20"

# 3) 单个任务状态（task_id 换成上一步返回值）
curl -s http://127.0.0.1:8080/v1/tasks/<task_id>

# 4) 检索（内置演示语料）
curl -s http://127.0.0.1:8080/v1/search \
  -H 'Content-Type: application/json' \
  -d '{"query":"三路召回如何融合？","routes":[{"type":"vector","top_k":20},{"type":"bm25","top_k":20}],"final_top_n":5}'

# 5) 沙箱执行自定义代码（输入/输出走 /tmp/input.json、/tmp/output.json）
curl -s http://127.0.0.1:8080/v1/sandbox/execute \
  -H 'Content-Type: application/json' \
  -d '{"code":"print(1+2)","runtime":"python","timeout_seconds":10}'
```

DAG 校验与试运行：

```bash
curl -s http://127.0.0.1:8081/v1/workflows/validate \
  -H 'Content-Type: application/json' \
  -d '{"dag":{"name":"demo","nodes":[{"id":"ingest","type":"ingest"},{"id":"parse","type":"parse"}],"edges":[{"from":"ingest","to":"parse"}]}}'
```

### 2. 前端控制台

```bash
make web-install
make web-dev        # http://localhost:5173
```

控制台通过 Vite 代理连接 Gateway（`/api`）与 Workflow Engine（`/wf`），包含以下页面：

| 页面 | 路径 | 功能 |
| --- | --- | --- |
| 登录 / 注册 | `/login` | 账号密码登录、自助注册、匿名浏览 |
| 概览 | `/` | 任务统计、租户存储/文件/问答配额卡片、最近任务 |
| 文件管理 | `/files` | 按知识库上传、查看状态、级联删除与重建索引（viewer 只读） |
| 任务中心 | `/tasks` | 任务列表、状态过滤、自动轮询、详情 |
| 检索测试 | `/search` | 多路召回检索、赞/踩反馈与链路分解 |
| 智能问答 | `/chat` | 多轮 RAG 问答、过程节点、检索知识、正文引用一键跳转 |
| SubAgent 管理 | `/subagents` | 自定义 HTTP / Sandbox Agent 的注册、技能声明、在线调试（需 `user:read`，写操作需 `user:write`） |
| 用户管理 | `/users` | 用户 CRUD、角色与权限点管理（需 `user:read`） |
| 沙箱 Playground | `/sandbox` | 自定义代码执行与输入/输出合约调试 |
| 工作流编排 | `/workflow` | 拖拽节点、连线、DAG 校验与试运行 |
| 审批中心 | `/approvals` | 工具审批收件箱与工作流运行列表 / 节点时间线 / 批准恢复（需 `workflow:run`） |
| 审计日志 | `/audit` | 按动作、决策与时间范围查询工具调用、审批与 PII 脱敏记录（需 `trace:read`） |
| API Key | `/api-keys` | 签发、吊销、作用域与知识库绑定，明文仅创建时展示（需 `apikey:write`） |
| Webhook | `/webhooks` | 事件订阅、签名密钥一次性展示、投递日志与载荷查看（需 `webhook:write`） |

访问控制台会跳转 `/login`：用种子管理员 `admin / admin123` 登录，或自助注册（默认 viewer 角色），也可点"以匿名身份浏览"（需 `ALLOW_ANONYMOUS=true`）。登录后右上角显示当前身份、知识库切换器与当前库角色（管理员/编辑者/只读），导航按权限动态显示：拥有 `user:read` 可见"SubAgent 管理"与"用户管理"，`workflow:run` 可见"审批中心"，`trace:read` 可见"审计日志"，`apikey:write` / `webhook:write` 分别可见"API Key"与"Webhook"。SubAgent 页面写操作需 `user:write`，且 Hub 需开启 `AGENT_HUB_DYNAMIC_REGISTRATION`。知识库管理员可在"管理"弹窗中改名、删除与授权成员。

首次体验可一键导入示例数据（知识库、三篇文档、演示账号、API Key 与示例工作流）：

```bash
python scripts/seed_demo_data.py
```

详见 [用户使用指南](docs/user-guide.md)。

### 3. 完整容器编排

```bash
cp .env.example .env
make up
```

`make up` 会自动加载根目录 `.env`（gitignored）。端口冲突时可在 `.env` 覆盖 `GATEWAY_PUBLISH_PORT`（默认 8080）与 `POSTGRES_PUBLISH_PORT`（默认 5432）；MinIO / Prometheus / Grafana 属于可选 `extras` profile，需要时用 `docker compose --profile extras ...` 启用。

| 入口 | 地址 |
| --- | --- |
| Web 画布 | http://localhost:8088 |
| Gateway API | http://localhost:8080 |
| Workflow Engine | http://localhost:8081 |
| LlamaIndex Service | http://localhost:8000 |
| MinerU API（独立容器） | http://localhost:18000 |
| Agent Hub | http://localhost:8001 |
| RabbitMQ 控制台 | http://localhost:15672 |
| Prometheus / Grafana | http://localhost:9090 / http://localhost:3000 |

容器编排使用 `STATE_DRIVER=redis`、`QUEUE_DRIVER=rabbitmq`，服务间通过 gRPC 与消息队列解耦。每个业务容器均提供 `/healthz`（Go 服务在 `METRICS_ADDR=:9100` 上同时暴露 `/metrics`，gateway/workflow/web 用各自 HTTP 端口），`docker compose` 依据健康检查状态控制启动顺序（如 gateway 等待 auth/user/ingestion/query/sandbox/agent-hub 健康后再启动）。

生产集群部署（PVC、HPA 自定义指标与配额）见 [docs/deployment.md](docs/deployment.md)。

### 4. 真实 MinerU 4.x 与真机冒烟

真实解析使用 **MinerU 4.x 官方 V1 Tasks API**。按[官方 Docker 部署文档](https://opendatalab.github.io/MinerU/zh/quick_start/docker_deployment/)构建镜像时，建议固定 `mineru[torch]==4.2.3`，不要依赖可变的 `>=4.0,<5` 安装区间。当前 `vLLM 0.21.0` 基础镜像还需将 `transformers` 固定为 `>=5.10.1,<5.23.0`；本次验证版本为 `5.22.2`。镜像内应预下载 standard 的 Torch 小模型与 vLLM 权重。

已有 `mineru:4.2.3` 镜像后，启动独立 API 容器（宿主端口避开 LlamaIndex 的 8000）：

```bash
docker run -d --name mineru-api \
  --network nexusflow_default \
  -p 18000:8000 \
  --gpus all --ipc=host --shm-size=8g \
  --restart unless-stopped \
  mineru:4.2.3 \
  mineru-kit api-server --host 0.0.0.0 --port 8000 --tier standard
```

在根目录 `.env` 配置并重建 Worker：

```bash
MINERU_TARGET=http://mineru-api:8000
MINERU_API_KEY=
MINERU_TIER=standard
```

```bash
docker compose --env-file .env -f deploy/docker-compose.yml up -d --build processing-worker
```

本地 API 未启用认证时 `MINERU_API_KEY` 留空；PDF/图片档位可设为 `flash/basic/standard/advanced`，取决于服务端开放能力。Worker 从与 Gateway 共享的 `OBJECT_STORE_DIR` 流式上传文件，不要求 MinerU 能访问 `OBJECT_STORE_BASE` 中的 MinIO 地址。TXT/Markdown 直接保留原文，PDF/DOCX/HTML 交给 MinerU；V1 结果 ZIP 中的 Markdown 再进入 LlamaIndex 切块与索引。

运行真机验收：

```bash
python scripts/smoke_test.py --require-mineru
# Linux/macOS 也可使用：./scripts/smoke_test.sh --require-mineru
```

默认不加 `--require-mineru` 时允许 Mock 解析并将真实 MinerU 检查记为 SKIP；严格模式会验证真实 PDF 上传、GPU 解析、Qdrant 向量与内容标记、LlamaIndex 检索及 Gateway 混合检索，同时检查 Postgres 身份初始化和 Redis 跨进程令牌吊销。2026-11-27 本机验证结果为 **29 passed / 0 failed / 0 skipped**。

## 核心机制

**任务状态机**（`backend/internal/state`）：

```text
PENDING → FETCHED → PARSING → PARSED → CHUNKED → EMBEDDED → INDEXED → COMPLETED
                                   ↘ FAILED（重试耗尽进 DLQ）

原地重建：COMPLETED → REINDEXING → CHUNKED → EMBEDDED → INDEXED → COMPLETED
自定义节点：SANDBOX_CREATING → SANDBOX_RUNNING → SANDBOX_DONE → 并入主链路
```

消费端以 `task_id + content_hash` 为幂等键，避免重复解析与重复向量化。`processing-worker` 在解析完成后消费 `file.parsed`，通过 LlamaIndex `/v1/index` 完成切块、Embedding 与 Qdrant 写入（未配置 `LLAMAINDEX_URL` 时跳过索引并告警），并把解析产物保存为 `tenant/kb/parsed/<task>.md` 作为重建索引的输入。

**文档治理**：`DELETE /v1/files/{id}` 级联清理对象存储、Qdrant 分片与图谱缓存，并回退租户计量；索引清理失败时保留任务记录以便重试。`POST /v1/files/{id}/reindex` 复用已持久化的 Markdown 原地重建：分片使用确定性 UUID 覆盖写，新点写入后再删除旧点，检索侧无空窗；升级前上传的历史文档没有解析产物，需重新上传后才能重建。

**外部连接器**（`backend/internal/connector`）：统一 `Source` 契约（发现 → 拉取 → 入库 → 游标），内置 `web_crawler`（同站链接、页数/大小上限、默认拒绝私网）、`s3_bucket`（S3 兼容 endpoint，按对象 key 续传）与 `db_query`（只读 SELECT + OFFSET 分页，行转 Markdown）。同步经 ingestion `RegisterContent` 落对象存储并创建普通任务，内容 SHA-256 幂等去重；配置与游标存入 `data_connectors`（`CONNECTOR_DRIVER=postgres`），配置以 AES-256-GCM 加密，HTTP 响应中凭据统一脱敏。`schedule_cron` 为 5 字段标准 cron（分/时/日/月/周），由内置调度器按 `CONNECTOR_SCHEDULER_INTERVAL` 评估；ingestion 多副本通过 `connector_schedule_claims` 抢占同一分钟的 run，避免重复同步。

**DAG 引擎**（`backend/internal/workflow`）：校验节点类型、引用完整性与环路，按拓扑层级并发执行就绪节点，上游输出写入共享 `ExecutionContext`。

**控制流节点**：`condition` 按 `field`（`${node.field}` / `node.field` / 字面量）与 `equals` / `not_equals` / `in` / `contains`（缺省真值判断）选择 `then` / `else` 分支；引擎跳过“从被丢弃目标可达且从选中目标不可达”的节点，汇聚节点仍会执行，结果返回 `skipped` 列表。`loop` 对 `items`（或 `items_from`）逐元素并发执行 `sub_dag`（`max_concurrency` 默认 4、上限 16），每项输入为 `item` / `index`，聚合 `count` / `failed` / `results`；默认任一元素失败即整体失败，`continue_on_error=true` 可收集部分结果。配置 RunStore 时每次迭代会作为子运行持久化（`parent_run_id` / `parent_node_id` / `parent_index`），可经 `GET /v1/workflows/runs/{id}/children` 查看，且不进入顶层运行列表、不重复发 Webhook。`then` / `else` 必须是 condition 的下游，嵌套 loop 在 `/v1/workflows/validate` 阶段拒绝。

**NL2SQL 节点**（`type: "nl2sql"`）：配置 `sql` 直接执行，或用 `sql_from` 引用上游输出（如 `query.sql`）；配置 `question` / `question_from` 且启用 `NL2SQL_LLM_*` 时由 LLM 生成 SQL。内置只读护栏：只允许单条 `SELECT` / `WITH ... SELECT`，剔除注释与尾分号，拒绝堆叠语句、DDL/DML 关键字、`SELECT INTO` 与 `FOR UPDATE`；执行时统一包裹 `LIMIT`（`NL2SQL_MAX_ROWS`，默认 200）并受 `NL2SQL_TIMEOUT` 约束。数据库连接由 `NL2SQL_DATABASE_URL` 注入，生产应使用 SELECT-only 角色。

**检索链路**（`backend/internal/query`）：多路召回并行执行（`vector` / `bm25` / `graph`，生产均由 LlamaIndex 检索真实 Qdrant 索引）→ RRF 融合（k=60）→ 去重 → Cross-Encoder 重排 → 负反馈降权（`FeedbackPenalty`：同一租户内某 chunk 累计 ≥3 条负反馈时得分 ×0.5，再排序）→ 取 top-N；单路失败、重排超时或反馈存储不可用时均自动降级（Fail-open）。`LlamaIndexRetriever` 统一封装三路请求并从 gRPC 元数据注入 `tenant_id` 过滤；配置 `LLAMAINDEX_URL` 时 query-service 使用真实索引，未配置时回退到内存演示语料（dev-all 始终使用内存实现）。`graph` 路由默认使用自研的 LightRAG 风格实体图（无 LLM 时退化为共现词图）；图按「集合 + 租户」隔离，`LLAMAINDEX_GRAPH_STORE_DRIVER=postgres` 时快照写入 `rag_graph_snapshots`，重启/多副本可直接复用，索引与删除会按集合前缀失效；BM25 回退与图构建的 Qdrant scroll 也按租户过滤，避免共享 KB 集合串租户。设置 `LLAMAINDEX_LIGHTRAG_ENABLED=true` 且配置 `LLAMAINDEX_LLM_*` 后可切换为 `lightrag-hku`（>=1.5.8）官方实现：按「租户 + KB」独立 workspace，索引异步 `ainsert`、查询 `aquery_data` 取结构化 chunk，存储支持 `file` 或 `postgres`（PGKV/PGVector/PGDocStatus，图默认 `PGTableGraphStorage` 纯 PG 表、无需 Apache AGE；AGE 环境可切 `PGGraphStorage`）。

**稠密 + 稀疏混合召回**：`bm25` 路由优先使用 Qdrant 原生 sparse（每 KB 伴生集合 `<collection>__sparse`，`modifier=idf`），写入时与稠密向量同 ID；sparse 未覆盖、集合缺失或调用失败时自动回退到原来的 scroll + BM25。`LLAMAINDEX_SPARSE_ENABLED=false` 可强制使用旧实现。升级前已索引的文档没有 sparse 向量，建议通过 `POST /v1/files/{id}/reindex` 补齐；命中不足 `top_k` 时会混合旧 BM25 结果兜底，但旧文档的纯词法召回会弱于已补齐的文档。

**Rerank / Embedding 协议**：`RERANK_PROTOCOL=generic|dashscope`。`generic` 为 bge/Cohere 风格 `{model,query,documents,top_n}`；`dashscope` 使用阿里云原生 `{model,input:{query,documents},parameters:{top_n}}` 并以 `RERANK_API_KEY` 作 Bearer 认证，`RERANK_POOL` 限制送排候选数、`RERANK_TIMEOUT` 控制超时；LlamaIndex 侧对应 `LLAMAINDEX_RERANK_*`。Embedding 批次由 `LLAMAINDEX_EMBEDDING_BATCH_SIZE` 控制，`MEMORY_EMBEDDING_DIM` 必须与模型输出维度一致（如 DashScope 1024）。

**沙箱护栏**（`backend/internal/sandbox`）：强制 TTL、`defer` 无条件销毁、输入/输出走约定文件、出口白名单与凭据不落盘。`local` 后端仅用于本机调试；生产必须使用 `remote`——已接入官方 OpenSandbox Go SDK（创建/等待就绪、execd 执行与文件传输、强制删除），配置方式见 `docs/deployment.md` 第 8 节。

## 用户与认证（RBAC）

认证与账号管理拆成两个独立 gRPC 服务（`identity.proto` 契约）：

- **Auth Service（:9096）**：`Register` / `Login` / `Refresh` / `Logout` / `Validate`，负责密码校验与 JWT 签发。
- **User Service（:9097）**：租户隔离的用户 CRUD，以及全局角色、权限点的管理。

**令牌模型**：HS256 JWT，claims 携带 `tenant_id` / `user_id` / `roles` / `permissions` / `token_type` / `jti`。access token 默认 15 分钟（`ACCESS_TOKEN_TTL`），refresh token 默认 168 小时（`REFRESH_TOKEN_TTL`）；`token_type` 区分用途，refresh token 不能当 access 用。权限点在登录时由角色展开并写入 token，网关据此本地判定权限，无需每次回调鉴权服务；仅额外查询一次吊销名单以拦截已登出的 `jti`。控制台在 access token 过期（401）时用 refresh token 自动换新并重试原请求，刷新失败则清理登录态并回到 `/login`。

**存储**：`IDENTITY_DRIVER=memory`（默认，dev-all 单进程）、`redis` 或 `postgres`（独立部署时 Auth / User 服务共享同一份用户与角色数据；postgres 使用 `IDENTITY_DATABASE_URL`，启动时自动建 `identity_users` / `identity_roles` / `knowledge_bases` / `kb_permissions` 表，并把旧 `identity_knowledge_bases` 幂等迁入）。

**权限模型**：固定权限点目录（19 个 + 通配 `*`，含 `kb:read` / `kb:write`、`usage:read`、`apikey:write`、`webhook:write`）→ 角色组合 → 用户绑定角色。内置三个角色：

| 角色 | 权限 | 说明 |
| --- | --- | --- |
| `admin` | `*` | 全量权限 |
| `editor` | 文件/任务读写、知识库读写、检索、沙箱、工作流、trace、user:read、usage:read | 内容编辑者 |
| `viewer` | 文件/任务与知识库只读、检索、trace、usage:read | 只读访客 |

内置角色不可修改删除；自定义角色可自由组合权限点。

**知识库级权限**：除全局角色外，`kb_permissions` 按知识库授予 `kb:admin` / `kb:editor` / `kb:viewer`。显式用户绑定优先于角色绑定，租户 `admin` 兜底，`default` 知识库对成员默认 viewer；无绑定且非 default 的知识库对该用户不可见。viewer 只能检索与查看，editor 可上传、删除与重建索引，admin 才能改名、删除知识库和授权。表结构见 `docs/schema_enterprise_v1_1_to_1_5.sql`。

**HTTP 端点**（Gateway 暴露）：

| 方法 | 路径 | 鉴权 | 说明 |
| --- | --- | --- | --- |
| POST | `/v1/auth/register` | 公开 | 自助注册（`ALLOW_SELF_REGISTER` 可关），默认分配 viewer |
| POST | `/v1/auth/login` | 公开 | 用户名 + 密码 → 令牌对 + 用户信息 |
| POST | `/v1/auth/refresh` | 公开 | refresh token 换新令牌对 |
| POST | `/v1/auth/logout` | 需登录 | 吊销当前 access token 与请求体中的 refresh token（`TOKEN_BLACKLIST_DRIVER`：memory/redis） |
| GET/POST | `/v1/users` | `user:read` / `user:write` | 列表 / 创建 |
| GET/PATCH/DELETE | `/v1/users/:id` | `user:read` / `user:write` / `user:delete` | 详情 / 更新 / 删除 |
| GET/POST | `/v1/roles` | `role:read` / `role:write` | 角色列表 / 新建或更新 |
| DELETE | `/v1/roles/:name` | `role:write` | 删除自定义角色 |
| GET | `/v1/permissions` | `role:read` | 权限点目录 |
| GET/POST | `/v1/knowledge-bases` | `kb:read` / `kb:write` | 知识库列表 / 创建 |
| PATCH/DELETE | `/v1/knowledge-bases/:id` | `kb:read` + 服务端校验 `kb:admin` | 改名 / 删除知识库 |
| GET/POST/DELETE | `/v1/knowledge-bases/:id/permissions` | `kb:read` + 服务端校验 `kb:admin` | 列出 / 授权 / 撤销 |
| GET | `/v1/usage` | `usage:read` | 租户用量、配额与余量 |
| DELETE | `/v1/files/:id` | `file:delete` + `kb:editor` | 级联删除对象、向量与任务 |
| POST | `/v1/files/:id/reindex` | `task:write` + `kb:editor` | 从已解析内容重建索引 |
| GET/POST | `/v1/connectors` | `kb:write` | 外部数据源连接器列表 / 创建（配置加密存储） |
| GET/DELETE | `/v1/connectors/:id` | `kb:write` | 连接器详情（密钥脱敏） / 删除 |
| POST | `/v1/connectors/:id/sync` | `kb:write` | 手动增量同步，返回创建/跳过/失败计数 |
| GET/POST | `/v1/api-keys` | `apikey:write` | 列出 / 签发租户 API Key（明文仅创建时返回） |
| DELETE | `/v1/api-keys/:id` | `apikey:write` | 吊销 API Key |
| GET/POST | `/v1/webhooks` | `webhook:write` | 租户 Webhook 订阅列表 / 创建（密钥仅创建时返回） |
| DELETE | `/v1/webhooks/:id` | `webhook:write` | 删除订阅（级联删除投递日志） |
| GET | `/v1/webhooks/:id/deliveries` | `webhook:write` | 投递日志与重试状态（`?status=FAILED` 过滤死信） |
| POST | `/v1/webhooks/:id/test` | `webhook:write` | 测试投递（`webhook.test`） |
| POST | `/v1/webhooks/:id/rotate-secret` | `webhook:write` | 轮换签名密钥 |
| POST | `/v1/webhooks/:id/deliveries/:deliveryId/replay` | `webhook:write` | 重放单条历史投递 |
| POST | `/v1/webhooks/:id/dead-letters/replay` | `webhook:write` | 批量重放 FAILED 投递（标记为 REPLAYED） |
| POST | `/v1/openai/chat/completions` | API Key `chat:completions` 或 JWT `search:use` | OpenAI 兼容问答（流式 / 非流式，引用透传；支持 `system`、`temperature`、`max_tokens`） |

首次启动会自动种子管理员：`admin / admin123`（可用 `SEED_ADMIN_*` 覆盖）。控制台 `/login` 登录后，`admin` 可在"用户管理"页创建账号、分配角色、启停用户，并新建自定义角色。未登录可点"以匿名身份浏览"（需 `ALLOW_ANONYMOUS=true`），匿名身份仅有 `search:use`，可使用检索与智能问答，但无任何管理权限。

## Agent Hub（RAG + 多 Agent 星型调度）

`services/agent-hub` 默认单容器运行：中心 Hub 以星型拓扑连接 intent、summary、memory 三个 Agent，对外仍提供原 `/v1/chat` 与 `/v1/conversations` 契约。

```mermaid
flowchart LR
    GW[API Gateway] --> HUB[Agent Hub<br/>capability router]
    HUB -. registry · priority · health · TTL .-> REG[(Agent Registry)]

    HUB -->|intent.classify| INT[Intent Agent<br/>local :8001 / remote :8002]
    HUB -->|summary.compact| SUM[Summary Agent<br/>local :8001 / remote :8003]
    HUB -->|memory.recall / extract| MEM[Memory Agent<br/>local :8001 / remote :8004]

    HUB -->|/v1/search| GW
    HUB -->|会话 / 记忆正文| PG[(PostgreSQL)]
    HUB -. 记忆向量 .-> QD[(Qdrant)]
```

- **星型调度**：Hub 按 capability（`intent.classify` / `summary.compact` / `memory.recall` / `memory.extract`）从注册表选择 Agent；支持 `local` 进程内执行与 `http` 远程 worker、优先级、超时、健康摘除、故障转移和运行时注册。`AGENT_HUB_MODE=local|remote|hybrid` 控制拓扑。
- **自定义 SubAgent**：`POST /v1/agents` 注册租户级 Agent（需 `AGENT_HUB_DYNAMIC_REGISTRATION=true`），支持 `kind=http` 与 `kind=sandbox`；注册表以 `(tenant_id, name)` 隔离，内置 Agent 为全局共享；`AGENT_HUB_REGISTRY_DRIVER=postgres` 时持久化到 `custom_agents` 并在启动时自动加载，`GET /v1/agents` / `GET|DELETE /v1/agents/{name}` 均按 `x-tenant-id` 限定。
- **Sandbox SubAgent**：`kind=sandbox` 将 `{code,runtime,timeout_seconds,packages}` 存入注册表，由 `SandboxAgentExecutor` 经 Gateway `/v1/sandbox/execute` 执行；调用入口 `POST /v1/agents/{name}/call` 透传用户 Bearer，实际运行仍要求 `sandbox:execute`，代码上限 64KB、超时上限 60s，列表只返回脱敏元数据。
- **动态技能路由**：自定义 Agent 可声明 `skills`（如 `expense.approval`）；Intent 节点接收该租户的可用技能（`GET /v1/skills`），返回 `intent=skill` 与技能名；Chat 直接调用对应 SubAgent，失败/技能不存在时自动回退 RAG。SSE 增加 `intent.skill` 与 `stage(skill)` 事件。
- **身份联动**：网关校验 JWT 后将 `x-tenant-id` / `x-user-id` / `Authorization` 透传给 agent-hub；会话与消息按「租户 + 用户」隔离，匿名调用者落在 `default/anonymous`。
- **意图路由**：Hub 调用 `intent.classify` 做 `knowledge_qa | chitchat | clarify` 分类；`chitchat` / `clarify` 跳过检索直接回答，Agent 不可用时降级为知识问答。
- **上下文压缩**：会话超出 `RAG_MAX_HISTORY` 时，Hub 调用 `summary.compact` 生成滚动摘要并写入 `rag_conversations.summary`；失败时改回原文窗口，仅损失上下文、不影响正确性。
- **长期记忆**：Hub 调用 `memory.recall` 按「租户 + 用户」召回 top-k 注入 prompt；回答落库后通过 Hub 有界后台池提交 `memory.extract`。记忆正文在 Postgres，向量在 Qdrant。
- **检索复用**：agent-hub 通过 `RAG_GATEWAY_URL` 调用网关 `/v1/search`，复用统一的多路召回 + RRF + 重排链路与 trace。
- **LangGraph 生成**：`retrieve → generate` 两节点状态图；配置 `RAG_LLM_BASE_URL` + `RAG_LLM_API_KEY` 时调用大模型，未配置时降级为抽取式片段摘要。
- **SSE 契约兼容**：`/v1/chat/stream` 仍推送 `stage(intent) → intent → stage(retrieve) → sources → stage(generate) → token* → done`；命中技能时插入 `stage(skill)`，`intent` 事件携带 `skill`；开启推理后新增 `stage(reasoning)` / `tool_call` / `tool_result` / `approval_required`，旧事件语义不变。
- **会话存储**：`RAG_DATABASE_URL` 指向 Postgres 时持久化到 `rag_conversations` / `rag_messages`；为空则使用进程内存储。

### 工具调用、双向联动、MCP 与人机协同

四项能力均由开关控制，默认关闭；关闭即回滚到原有 `retrieve → generate` 与 `/v1/workflows/run` 路径。

- **工具契约与通用调用**（`AGENT_HUB_TOOLS_ENABLED`）：`ToolSpec{input/output schema, risk, timeout, requires_approval}` 与 `CallEnvelope{tenant,user,trace,request,approval,deadline,budget}` 统一；`POST /v1/call` 的 agent 分支始终可用，tool 分支需开启该开关；`GET /v1/tools` 列出工具。
- **多轮自助推理**（`AGENT_HUB_REASONING_ENABLED`）：LLM `bind_tools` 后循环 `plan → tool_call → observe → answer`；受 `MAX_TURNS` / 工具调用数 / Token / 总时限预算约束，重复 `tool+args` 自动抑制，工具失败交回模型决策，预算耗尽回退原生成。
- **DAG 双向联动**（`AGENT_HUB_WORKFLOW_ENABLED` / `WORKFLOW_AGENT_NODE`）：DAG 新增 `agent` 节点调用 Hub `/v1/call`；Hub 的 `workflow.run` 工具经网关（携带用户身份）或服务令牌触发 DAG。`x-tenant-id` / `x-trace-id` / deadline 全程透传；网关 `/v1/workflows/*` 统一要求 `workflow:run`。
- **MCP**（`AGENT_HUB_MCP_ENABLED`）：`POST /mcp` 为 Streamable HTTP（JSON 或 `Accept: text/event-stream` 单次 SSE 响应），`initialize` 返回 `Mcp-Session-Id`；`GET /mcp` 复用该会话接收服务端通知，`DELETE /mcp` 结束会话。另保留 `GET /mcp/sse` + `POST /mcp/messages?session_id=...` 旧版 HTTP+SSE 传输，本地 stdio 通过 `python -m nexusflow_agents.mcp.stdio` 使用（newline-delimited JSON-RPC，日志走 stderr）。所有传输统一支持 `initialize` / `tools/*` / `resources/*` / `prompts/*`：资源暴露 `nexusflow://agents`、`nexusflow://skills`、`nexusflow://tools`（含单条模板），`resources/subscribe` 后 `GET /mcp` 会在快照变化时推送 `notifications/resources/updated`；提示词提供 `rag_answer`、`summarize`、`intent_classify`。`AGENT_HUB_MCP_SERVERS=name=url,...` 接入外部 MCP，工具注册为 `mcp.<server>.<tool>`。外部工具默认需审批，sandbox 默认不对外暴露。
- **人机协同**（`AGENT_HUB_HITL_ENABLED` / `HITL_ENABLED`）：高风险工具不执行，返回 202 + `approval_id`，由 `POST /v1/approvals/{id}/decide` 批准/拒绝后重试；DAG 的 `human_approval` 节点使 run 进入 `WAITING_APPROVAL` 并落盘检查点，经 `/v1/workflows/runs/{id}/approve|resume` 恢复，重复审批返回 409。`WORKFLOW_RUN_STORE_DRIVER=postgres` 时运行与审批写入 `workflow_executions` / `workflow_approvals`，跨副本共享且审批只生效一次。
- **安全护栏**：工具风险分级（`read/write/execute/external`）、入参 JSON Schema、输出截断、预算超限 429、工具与审批审计（`GET /v1/audit`），工具结果以不可信数据包裹以防提示注入；网关侧 `/v1/sandbox/execute` 要求 `sandbox:execute`。`AGENT_HUB_AUDIT_DRIVER=postgres` 时审计写入 `audit_logs`，新表按月分区、启动与每小时维护预建次月分区，并按 `AGENT_HUB_AUDIT_RETENTION_DAYS`（默认 180，0 永久保留）整月清理过期分区。
- **PII 脱敏**（`RAG_PII_ENABLED`）：提示词、历史、检索片段、摘要、记忆与纠正答案在进入模型前替换为稳定占位符（`{{EMAIL_1}}` / `{{PHONE_1}}` 等，支持邮箱、手机号、身份证、银行卡与 IPv4）；检索仍使用原始 query，保证按手机号/邮箱检索可用。回复默认保持脱敏并对模型新生成的 PII 再次脱敏，`RAG_PII_RESTORE_OUTPUT=true` 可还原。每次命中会写入 `pii.mask` 审计事件（类型与数量），可按租户与时间检索。
- **LLM 网关**（`backend/internal/llmgateway`）：OpenAI 兼容 `POST /v1/chat/completions`（含 SSE）与 `/v1/embeddings`，按主/备通道自动切换，连续失败触发熔断（阈值/冷却可配）；按 `x-tenant-id` 累计每日 token，超限前置返回 `429 quota_exceeded`，预留额度原子扣减并在响应后按实际 usage 修正。把 `RAG_LLM_BASE_URL` 等指向 `http://llm-gateway:8082/v1` 即可复用；RAG 生成、Intent / Summary / Memory 的独立 Agent 与 HTTP 客户端都会透传 `x-tenant-id`。
- **开放 API / OpenAI 兼容**（`API_KEY_DRIVER=memory|postgres`）：管理员通过 `POST /v1/api-keys` 签发 `nf-sk-*` 密钥（明文仅返回一次，库中只存 SHA-256），可限定 `allowed_kb_ids`、`scopes`（`chat:completions` / `search:use`）、`rate_limit_rpm` 与 `expires_in_days`；`DELETE /v1/api-keys/{id}` 吊销，密钥与 JWT 并存。`POST /v1/openai/chat/completions` 兼容 OpenAI Chat Completions：非流式返回 `chat.completion`，流式逐 token 返回 `chat.completion.chunk` 并以 `data: [DONE]` 结束；`messages` 转为 Hub 无状态调用（历史随请求传入、不写会话库），引用在 `choices[0].message.citations` 与顶层 `citations` 透传，`kb_id` 可放在请求体、`metadata.kb_id` 或 `X-KB-ID` 头。`system` / `temperature` / `max_tokens` 透传；`usage` 优先使用上游真实 token 用量，未配置模型时回退字符估算；流式请求 `stream_options.include_usage=true` 时会在 `[DONE]` 前追加 usage chunk。
- **事件 Webhook**（`WEBHOOK_DRIVER=memory|postgres`）：`POST /v1/webhooks` 按租户订阅 `task.completed` / `task.failed` / `index.completed` / `workflow.completed` / `workflow.failed` / `workflow.node.*` / `approval.decided`（或 `*`），可指定目标 URL 与签名密钥（未提供时自动生成并仅返回一次）。目标 URL 默认拒绝回环/私网/链路本地地址，并在每次投递时重新解析以防 DNS rebinding（`WEBHOOK_ALLOW_PRIVATE` 可放宽，仅建议本地开发使用）；`WEBHOOK_ALLOWED_HOSTS` 可进一步限定精确域名或 `*.suffix` 通配。投递携带 `X-NexusFlow-Signature: sha256=<HMAC>`、事件、投递 ID 与时间戳头；失败按指数退避重试（`WEBHOOK_MAX_ATTEMPTS` / `WEBHOOK_RETRY_BASE`），每次尝试写入 `webhook_deliveries`，可经 `GET /v1/webhooks/{id}/deliveries?status=FAILED` 查看死信。支持 `POST /v1/webhooks/{id}/test` 测试投递、`POST .../rotate-secret` 轮换密钥（新密钥仅返回一次）、`POST .../deliveries/{deliveryId}/replay` 单条重放与 `POST .../dead-letters/replay` 批量重放 FAILED 投递（重放后原记录标记 `REPLAYED`，避免重复发送）。processing-worker 与 workflow-engine 通过 `GATEWAY_URL` + `WEBHOOK_EVENTS_TOKEN` 回调 `POST /v1/internal/events`，由网关统一分发；审批结果由网关直接产生。Workflow 引擎的节点级 `EventSink` 会发出 `workflow.node.started` / `completed` / `failed` / `waiting_approval` / `skipped`，循环子运行不重复上报。
- **图文多模态**：MinerU 结果 ZIP 中的图片由 Worker 提取并落盘到 `<tenant>/<kb>/parsed/<task>/images/`，Markdown 图片链接改写为 `/v1/media`；切块把 `image_urls` 写入节点 metadata，检索结果与问答引用返回配图，控制台展示缩略图并支持点击放大。结果 ZIP 中的 `content_list.json`（同名 `*_content_list.json` 优先）会原样保存为 `<tenant>/<kb>/parsed/<task>/layout.json` 并记录到任务 `layout_key`，供版面感知的下游消费；`/v1/media?key=...` 同时支持读取该文件（JSON，仍受租户与 API Key 知识库白名单约束）。

新增 / 变更接口：

| 方法与路径 | 权限 / 开关 | 说明 |
| --- | --- | --- |
| `GET /v1/media?key=` | `search:use` | 读取解析图片或 `parsed/<task>/layout.json`（仅本租户） |
| `POST /v1/workflows/{validate,run}` | `workflow:run` | DAG 校验与执行（经 Gateway 代理） |
| `GET /v1/workflows/runs`、`/v1/workflows/runs/{id}` | `workflow:run` | 运行列表（仅顶层）/ 详情 |
| `GET /v1/workflows/runs/{id}/children` | `workflow:run` | loop 节点各次迭代的持久化子运行 |
| `POST /v1/workflows/runs/{id}/{approve,resume}` | `workflow:run` | 审批并恢复挂起的 run，重复审批 409 |
| `GET /v1/approvals`、`/v1/approvals/{id}` | `workflow:run` | 工具审批待办 / 详情 |
| `POST /v1/approvals/{id}/decide` | `workflow:run` | 批准 / 拒绝高风险工具 |
| `GET /v1/audit` | `trace:read` | 工具 / 审批 / PII 审计查询，支持 `since` / `until`（RFC3339 或 unix 秒） |
| `POST /v1/call` | agent 分支恒可用；tool 分支需 `AGENT_HUB_TOOLS_ENABLED` 或 `AGENT_HUB_WORKFLOW_ENABLED` | Hub 内部通用调用（供 DAG `agent` 节点与 Sandbox SubAgent 使用） |
| `GET /v1/tools` | `AGENT_HUB_TOOLS_ENABLED` 或 `AGENT_HUB_WORKFLOW_ENABLED` | 工具契约列表 |
| `POST /v1/agents/{name}/call` | `search:use`；sandbox agent 另需 `sandbox:execute` | Gateway 调用自定义 SubAgent |
| `GET /v1/skills` | Hub 内部 | 租户可见技能（供 Intent/Router 使用） |
| `POST /mcp` | `AGENT_HUB_MCP_ENABLED` | MCP Server（Streamable HTTP）：initialize / tools / resources / prompts，可返回 JSON 或 SSE |
| `GET /mcp`、`DELETE /mcp` | `AGENT_HUB_MCP_ENABLED` | Streamable HTTP 通知流（`resources/subscribe` 更新） / 结束会话 |
| `GET /mcp/sse`、`POST /mcp/messages?session_id=` | `AGENT_HUB_MCP_ENABLED` | MCP Server（旧版 HTTP+SSE 传输） |
| `python -m nexusflow_agents.mcp.stdio` | `AGENT_HUB_MCP_ENABLED` | MCP Server（stdio 传输，本地进程） |
| `GET/POST /v1/api-keys`、`DELETE /v1/api-keys/{id}` | `apikey:write` | 签发 / 列出 / 吊销租户 API Key（明文仅创建时返回） |
| `POST /v1/openai/chat/completions` | API Key `chat:completions` 或 JWT `search:use` | OpenAI 兼容问答（流式 / 非流式，引用透传；支持 `system`、`temperature`、`max_tokens`） |
| `GET/POST /v1/webhooks`、`DELETE /v1/webhooks/{id}` | `webhook:write` | Webhook 订阅列表 / 创建 / 删除 |
| `GET /v1/webhooks/{id}/deliveries` | `webhook:write` | 投递日志（状态、尝试次数、HTTP 状态与响应；`?status=FAILED` 查看死信） |
| `POST /v1/webhooks/{id}/test` | `webhook:write` | 发送 `webhook.test` 测试投递 |
| `POST /v1/webhooks/{id}/rotate-secret` | `webhook:write` | 轮换签名密钥（新密钥仅返回一次） |
| `POST /v1/webhooks/{id}/deliveries/{deliveryId}/replay` | `webhook:write` | 重放单条历史投递载荷 |
| `POST /v1/webhooks/{id}/dead-letters/replay` | `webhook:write` | 批量重放 FAILED 投递（`limit` 可配，成功后标记 REPLAYED） |
| `POST /v1/internal/events` | `WEBHOOK_EVENTS_TOKEN` | 服务间事件接入（processing-worker / workflow-engine） |
| `GET/POST /v1/agents`、`GET/DELETE /v1/agents/{name}` | Hub 内部；写操作需 `AGENT_HUB_DYNAMIC_REGISTRATION` | 租户级自定义 SubAgent CRUD |

> `/v1/call` 的 agent 分支始终可用，tool 分支需 `AGENT_HUB_TOOLS_ENABLED`；`/mcp`、`/v1/call`、`/v1/tools` 为 Hub 内部入口，外部经 Gateway 的 `/v1/agents/{name}/call` 调用自定义 SubAgent。

本地运行（单容器，先启动 dev-all 提供网关与检索）：

```bash
make run-dev-all

cd services/agent-hub
pip install -e ".[dev]"
uvicorn nexusflow_agents.main:app --port 8001
```

可选拆分部署（同一镜像，通过 `ROLE` 启动独立 worker）：

```bash
AGENT_HUB_ROLE=intent  AGENT_HUB_PORT=8002 uvicorn nexusflow_agents.main:app --port 8002
AGENT_HUB_ROLE=summary AGENT_HUB_PORT=8003 uvicorn nexusflow_agents.main:app --port 8003
AGENT_HUB_ROLE=memory  AGENT_HUB_PORT=8004 uvicorn nexusflow_agents.main:app --port 8004

# 主入口改为远程/混合模式
AGENT_HUB_MODE=remote \
RAG_INTENT_URL=http://127.0.0.1:8002 \
RAG_SUMMARY_URL=http://127.0.0.1:8003 \
RAG_MEMORY_URL=http://127.0.0.1:8004 \
uvicorn nexusflow_agents.main:app --port 8001
```

也可用 `docker compose --profile split up` 启动拆分拓扑。

接入 DeepSeek（OpenAI 兼容，各类 Agent 可用同一套 Key）：

```bash
RAG_LLM_BASE_URL=https://api.deepseek.com/v1
RAG_LLM_API_KEY=sk-...
RAG_LLM_MODEL=deepseek-chat
INTENT_LLM_BASE_URL=https://api.deepseek.com/v1
INTENT_LLM_API_KEY=sk-...
INTENT_LLM_MODEL=deepseek-chat
SUMMARY_LLM_BASE_URL=https://api.deepseek.com/v1
SUMMARY_LLM_API_KEY=sk-...
SUMMARY_LLM_MODEL=deepseek-chat
# 记忆抽取复用 LLM；向量召回需额外的 embeddings（可选）
MEMORY_LLM_BASE_URL=https://api.deepseek.com/v1
MEMORY_LLM_API_KEY=sk-...
MEMORY_LLM_MODEL=deepseek-chat
MEMORY_EMBEDDING_BASE_URL=https://api.openai.com/v1
MEMORY_EMBEDDING_API_KEY=sk-...
MEMORY_EMBEDDING_MODEL=text-embedding-3-small
```

网关端点（`RAG_AGENT_URL` 为空会关闭代理）：

| 方法 | 路径 | 鉴权 | 说明 |
| --- | --- | --- | --- |
| POST | `/v1/chat` | `search:use` | 提问 `{message, conversation_id?, top_k?, kb_id?}`，返回答案 + 引用 |
| POST | `/v1/chat/stream` | `search:use` | 同上，SSE 流式返回过程事件与回答 |
| GET | `/v1/conversations` | `search:use` | 当前用户的会话列表（分页） |
| GET | `/v1/conversations/:id` | `search:use` | 会话详情（消息 + 引用） |

### 多智能体路线

| 阶段 | Agent | 内容 | 状态 |
| --- | --- | --- | --- |
| P1 | `intent`（默认 in-process；拆分 :8002） | 意图识别与路由（`knowledge_qa / chitchat / clarify`），LLM + 规则降级 | 已完成 |
| P2 | `summary`（默认 in-process；拆分 :8003） | 上下文总结、自动压缩（滚动摘要 + 最近 N 轮），按字符预算裁剪 | 已完成 |
| P3 | `memory`（默认 in-process；拆分 :8004） | 长期记忆抽取、去重合并、召回注入；Postgres 正文 + Qdrant 向量 | 已完成 |

约定：

- 默认 `AGENT_HUB_ROLE=all`、`AGENT_HUB_MODE=local`，三个 Agent 在进程内执行；`MODE=remote|hybrid` 时通过 `RAG_INTENT_URL` / `RAG_SUMMARY_URL` / `RAG_MEMORY_URL` 调用拆分 worker，失败自动降级。
- 摘要写入 `rag_conversations.summary`（`summary_count` 记录已折叠消息数），prompt 使用「摘要 + 最近 N 条原文」；引用与全文不因压缩丢失。
- 记忆正文存 `rag_memories`（Postgres，事实源），向量存 Qdrant `nexusflow_memories`；召回先按 `tenant_id + user_id` 过滤取 top-k ID，再回 Postgres 取正文，Qdrant 不可用或未命中时自动退回词面召回。`MEMORY_DRIVER=memory|postgres|qdrant`，为空时自动选择（DB + Qdrant → qdrant，仅 DB → postgres，否则 memory）。
- 记忆抽取在有 `MEMORY_LLM_*` 时用 LLM，否则走规则；去重按归一化文本 + 词面相似度合并，更新而非新增。
- 历史 pgvector 向量可用 `cd services/agent-hub && python -m nexusflow_agents.agents.memory.backfill` 一次性回填到 Qdrant；`MEMORY_DRIVER=postgres` 保留旧 pgvector 实现作为回滚路径。
- 运行时注册接口 `POST /v1/agents` 默认关闭；开启需 `AGENT_HUB_DYNAMIC_REGISTRATION=true` 与 `AGENT_HUB_ADMIN_TOKEN`。
- 所有 Agent 统一使用 OpenAI 兼容配置，DeepSeek 可直接接入。

## 反馈与评审

- **问答页反馈**：每条回答支持「有帮助 / 没帮助」，提交时携带 `trace_id`、`conversation_id`、`message_id`、query、answer、引用 chunk 与意图标签；点「没帮助」后可补充纠正答案（`corrected_answer`，覆盖更新同一评分）；点击正文 `[n]` 引用同时上报 `user.citation_click` 隐式正反馈。
- **检索页反馈**：保留赞/踩，落库的同时旁路写入 trace score（Langfuse 可用时）。
- **持久化**：`FEEDBACK_DRIVER=memory|postgres`。Postgres 启动时自动建 `rag_feedback` 表，按 `tenant_id + user_id + subject_id + name` 覆盖更新，同一消息只保留最新评分；生产 compose 默认 `postgres`，即使 tracing 为 Noop 也不会丢数据。
- **评审查询**：`GET /v1/feedback?rating=-1&limit=20`（需 `user:read`，按租户隔离），返回 query/answer/引用/纠正答案等字段。
- **低分归因**：`eval/analyze_feedback.py` 拉取负反馈，按 `generation_wrong / retrieval_miss / citation_missing / citation_out_of_range / unclassified` 归因，输出 `eval/feedback_report.json` 与可回放的 `eval/regressions.jsonl`。
- **闭环回灌**：`GET /v1/corrected-answers?query=...`（需 `search:use`）按问题相似度返回高分历史纠正答案；agent-hub 在检索前置步骤调用该接口，把命中的纠正答案注入 Prompt（`RAG_CORRECTED_TOP_K`，失败自动降级为普通 RAG）。检索侧的负反馈在线降权见上文 `FeedbackPenalty`。

## RAG 离线评测

`eval/rag_eval.py` 仅依赖 Python 标准库，用 golden 集调用网关 `/v1/search`（可选 `/v1/chat`）并输出指标；低于 `eval/thresholds.json` 时进程退出码为 1，可直接接入 CI。

| 指标 | 含义 |
| --- | --- |
| `recall_at_k` | top-k 命中的相关条目数 / 标注相关数 |
| `mrr` | 首个相关结果排名的倒数均值 |
| `ndcg_at_k` | 二值相关性 nDCG@k |
| `citation_validity` | 引用中 `chunk_id` 非空的比例（`--with-chat`） |
| `citation_hit_rate` | 至少命中一个标注相关条目的回答占比（`--with-chat`） |
| `inline_marker_rate` | 有引用的回答中正文包含 `[n]` 的比例（`--with-chat`） |

```bash
# 先启动 dev-all；基于 3 条种子文档的示例集默认 k=2
make eval-rag

# 追加问答引用指标（需启动 agent-hub）
make eval-rag-chat
```

用例格式（`eval/golden.jsonl`，每行一个）：

```json
{"query": "多路召回如何做 RRF 融合和精排？", "relevant_ids": ["doc-2"], "kb_id": null}
```

`relevant_ids` 可匹配 `chunk_id` 或 `meta.doc_id / document_id / file_name`。真实语料库建议 `--k 5` 并同步调整阈值；报告写入 `eval/report.json`（已忽略提交）。生成侧的语义评分见下节。

## LLM-as-judge 语义评分

`eval/judge.py` 对 golden 集逐条调用 `/v1/chat` 拿到答案与引用片段，再用 OpenAI 兼容模型按三个维度打分（0–1）：`faithfulness`（是否只依据引用）、`relevancy`（是否切题）、`completeness`（是否覆盖关键信息）。仅依赖标准库。

```bash
export RAG_LLM_BASE_URL=https://api.deepseek.com/v1
export RAG_LLM_API_KEY=sk-...
export RAG_LLM_MODEL=deepseek-chat

make judge-rag      # 写 eval/judge_report.json；低于 eval/judge_thresholds.json 时退出码 1
```

- 未配置 LLM 时脚本以退出码 2 提示需要 Key，不会静默跳过。
- 单条用例失败（网关或模型异常）会记入 `errors` 并继续评测其余用例；全部失败才返回 2。
- 阈值默认 `faithfulness/relevancy >= 0.7`、`completeness >= 0.6`，可按语料在 `eval/judge_thresholds.json` 调整。

## 反馈归因与回归集

`eval/analyze_feedback.py` 只读调用 `GET /v1/feedback?rating=-1`，把每条负反馈归因到最可能的失败环节，并导出可回放的回归样本；仅依赖标准库。

| 归因 | 触发条件（按优先级判断） |
| --- | --- |
| `generation_wrong` | 用户提交了 `corrected_answer` |
| `retrieval_miss` | 无引用，或答案包含“未在知识库 / 资料不足 / 没有找到 / 无法回答 / 没有相关内容” |
| `citation_missing` | 有引用但正文没有 `[n]` 角标 |
| `citation_out_of_range` | 角标最大序号超过引用条数 |
| `unclassified` | 以上都不命中（如有引用且角标合法仍被踩） |

```bash
# 先启动 dev-all 并积累若干负反馈
make analyze-feedback   # 默认 limit=200，写 eval/feedback_report.json 与 eval/regressions.jsonl
make test-eval          # 归因与输出的标准库单测
```

报告按 `attribution` 与 `intent`（取自反馈 `meta.intent`）聚合；`regressions.jsonl` 每行含 query / answer / 引用 / 归因 / 纠正答案等字段，可作为策略变更后的回放集。无负反馈时输出空文件并正常退出（exit 0）。

## 安全与可靠性加固

- **上传**：仅接受 pdf/docx/txt/md/html；空文件与超限（100 MiB → 413）前置拒绝；`object_key`/`file_url`/`content_hash` 等保留元数据由服务端写入，客户端表单/JSON 无法伪造；内容去重命中时删除刚落盘的孤儿对象。
- **SSRF 防护**：JSON 直传的 `presigned_url` 仅允许 `OBJECT_STORE_ALLOWED_HOSTS`（缺省取 `OBJECT_STORE_BASE` 的 host）中的 http/https 主机，`file://` 等一律拒绝。
- **队列**：RabbitMQ 每个消费者独占 channel（规避 amqp.Channel 非并发安全）、prefetch=10、`x-retry` 计数超过 5 次转入 `<topic>.dlq`；MemoryBus 失败回调记录日志。
- **一致性**：建任务/重试时投递失败会回滚为 FAILED（不再滞留 PENDING）；索引阶段支持从 Chunked/Embedded/Indexed 断点续推，仅 COMPLETED 跳过。
- **检索上下文**：拼接进 prompt 的检索内容截断至 32 KiB 并用 `<<<CONTEXT` 分隔、显式声明为不可信资料，以缓解提示词注入（非完全防护）。

## 可观测性（Tracing / Langfuse）

检索与问答链路全程埋点（`backend/internal/platform/tracing`），统一抽象 `Tracer` / `Span`；`trace_id` 经 HTTP 头 `x-trace-id` 传入并透传到下游 gRPC（metadata `x-trace-id`），同一次请求的所有 span 挂在同一 trace 下。

三种后端实现，由环境变量选择（`tracing.NewFromEnv`）：

| 实现 | 触发条件 | 用途 |
| --- | --- | --- |
| `MemoryTracer` | `APP_ENV=dev` 或 `TRACING_MEMORY=true` | 进程内保留最近 500 条 trace，驱动控制台"链路分解"面板 |
| `LangfuseTracer` | `LANGFUSE_BASE_URL` 非空 | 批量导出到 Langfuse（`trace-create` / `span-create` / `score-create`），Basic 鉴权，2s 或满 50 条触发 flush |
| `NoopTracer` | 默认 | 零开销，不记录也不导出 |

埋点覆盖：

- **检索**（`query.Engine.Search`）：root `query.search` → 每路并行 `recall.<name>` → `fusion.rrf` → `rerank`；每个 span 记录耗时、状态与各阶段 count，单路失败标记为 `error` 但不阻断整体降级。
- **问答**（`query.GRPCService.Answer`）：`answer.generate` span 记录所用 prompt 的 `name` / `version`（版本化 prompt 注册表 `internal/platform/prompt`）。
- **用户反馈**：`POST /v1/feedback` 以 score 形式挂到指定 trace；控制台检索页的赞 / 踩按钮与"链路分解"面板（`GET /v1/traces/:id`）消费这些数据。

> Langfuse 导出为 fail-open：导出失败仅记录 warn 日志并丢弃该批次，不影响业务请求。

开发态查看：启动 `cmd/dev-all` 后进入检索页，执行一次搜索即可在"链路分解"面板看到 span 树与耗时分解；`GET /v1/traces` 列出最近 trace。生产启用 Langfuse 只需在 `.env` 填入：

```bash
LANGFUSE_BASE_URL=https://your-langfuse.example.com
LANGFUSE_PUBLIC_KEY=pk-lf-...
LANGFUSE_SECRET_KEY=sk-lf-...
```

## 开发者命令

```bash
make proto         # 重新生成 Go protobuf / gRPC 代码
make build         # 编译全部 Go 包
make test          # Go + Python 测试
make vet           # go vet
make web-build     # 前端类型检查与生产构建
make up / down     # 容器编排启停
```

## 持续集成（GitHub Actions）

[`.github/workflows/ci.yml`](.github/workflows/ci.yml) 在 `main` / `master` 的 push 与所有 PR 上运行，并按模块拆分为并行 Job：

| Job | 内容 |
| --- | --- |
| Backend | `go vet ./...` + `go test ./...`（Go 版本取自 `backend/go.mod`） |
| Agent hub / LlamaIndex | 各自安装依赖后执行 `pytest`；agent-hub 额外构建镜像 |
| Offline eval scripts | `python -m unittest discover -s eval`（离线评测脚本单测，纯标准库） |
| Offline RAG gate | 编译并启动 `dev-all`，等待 `:8080/healthz` 后执行 `eval/rag_eval.py`；任一指标低于 `eval/thresholds.json` 即失败（日志作为 artifact 上传） |
| Web | `npm ci && npm run build`（tsc 类型检查 + Vite 产物） |

## 当前实现边界

- `MINERU_TARGET` 为空时使用内置 Mock 解析器；对 txt/md/csv/json 等文本类文件会回显真实内容，使"上传 → 检索"闭环可演示。配置真实 MinerU 4.x 时使用官方 V1 Tasks 协议；Worker 还需通过 `OBJECT_STORE_DIR` / `OBJECT_STORE_BASE` 读取共享对象卷，TXT/Markdown 原文直读，PDF/DOCX/HTML 由 MinerU 解析。
- 单进程 dev-all 模式下，上传文件经解析、切块后写入共享内存索引（DocStore），检索可命中真实上传内容，任务状态推进至 COMPLETED。独立部署时 `processing-worker` 调 LlamaIndex `/v1/index` 写入 Qdrant，`query-service` 的 vector/bm25/graph 三路召回均走 LlamaIndex（`LLAMAINDEX_URL`）；`dev-all` 仍保留内存实现以便零依赖演示。
- LlamaIndex Service 的切块、索引与检索依赖 Qdrant 与 Embedding API Key；未配置时相关接口返回 503。
- K8s HPA 使用自定义指标 `nexusflow_queue_length`，需部署 prometheus-adapter 将指标转换为 Custom Metrics API。
- 设计文档中的 NL2SQL 章节已落地为 `nl2sql` 工作流节点（只读 SQL 护栏 + 可选 LLM 生成）；LightRAG 图谱检索已落地为 `graph` 召回路由：LlamaIndex 提供 `/v1/graph/extract` 抽取接口与 `/v1/search` 的 graph 子路由（配置 `LLAMAINDEX_LLM_*` 时用 LLM 抽取实体/关系，否则或调用失败时自动退化为共现词图），Go 侧 `LlamaIndexRetriever` 将三路结果统一接入 RRF。长期记忆与 intent / summary 已合并到 `agent-hub`（默认 in-process，可用 `ROLE` 拆分）：记忆正文在 Postgres，向量在 Qdrant，两者职责分离。（可观测性 / Langfuse 导出与 dev trace viewer 已实现，见上节。）
- 用户与角色支持三种存储：`memory`（dev-all 单进程默认）、`redis`（`IDENTITY_DRIVER=redis`）与 `postgres`（`IDENTITY_DRIVER=postgres` + `IDENTITY_DATABASE_URL`），后两者供独立部署的 Auth / User 服务跨进程共享；postgres 启动时自动建表并复用内置角色种子。Compose 真机冒烟已验证自动建表、种子管理员登录及 Gateway 身份读取。
- 登出通过令牌吊销名单即时生效：JWT 携带 `jti`，登出时将该 id 写入 `TOKEN_BLACKLIST_DRIVER`（`memory` 单进程 / `redis` 供 gateway 与 auth-service 共享）并设置剩余有效期 TTL；网关在每次鉴权时拦截已吊销的 access token，auth-service 在 validate/refresh 时拒绝已吊销的 refresh token。Redis 不可用时自动降级为内存名单并告警。
- 工具调用、审批、审计与工作流运行状态默认仍为进程内实现（`ToolService` / `ApprovalService` / `AuditLog` / `MemoryRunStore`），重启会丢失；生产多副本按需开启 `AGENT_HUB_APPROVAL_DRIVER=postgres`、`AGENT_HUB_AUDIT_DRIVER=postgres`（`AGENT_HUB_STATE_DATABASE_URL`）与 `WORKFLOW_RUN_STORE_DRIVER=postgres`（`WORKFLOW_RUN_DATABASE_URL`），分别落库 `agent_approvals` / `audit_logs` 与 `workflow_executions` / `workflow_approvals`。`audit_logs` 建议按月分区并配置保留策略。
- MCP 支持 Streamable HTTP（`POST /mcp`，可返回 JSON 或单次 SSE；`GET /mcp` 通知流；`DELETE /mcp` 结束会话）、旧版 HTTP+SSE（`GET /mcp/sse` + `POST /mcp/messages`）与 stdio（`python -m nexusflow_agents.mcp.stdio`），均复用同一套分发：`initialize`、`tools/list|call`、`resources/list|templates/list|read|subscribe|unsubscribe`、`prompts/list|get`。资源为只读视图（`nexusflow://agents`、`nexusflow://skills`、`nexusflow://tools` 及单条模板），订阅后由 `GET /mcp` 轮询快照并推送 `notifications/resources/updated`，资源目录变化时推送 `notifications/resources/list_changed`；提示词为 Hub 内置模板（`rag_answer` / `summarize` / `intent_classify`）。`AGENT_HUB_MCP_SESSION_DRIVER=postgres` 时会话存在性/租户/订阅写入共享 `mcp_sessions`，跨副本可续接同一会话（SSE 流仍绑定当前副本），并按 `AGENT_HUB_MCP_SESSION_TTL_SECONDS` 清理空闲会话；默认 memory 驱动为单副本模式。外部工具默认 `risk=external` 且需审批，`sandbox.execute` 默认不通过 MCP 暴露（`AGENT_HUB_MCP_ALLOW_SANDBOX` 显式开启）。
- 推理模式的生成轮按 token 增量下发，工具调用阶段的内容会被抑制；`tool_call` / `tool_result` 逐条下发，工具在独立线程执行并受 `AGENT_HUB_REASONING_TOOL_TIMEOUT_SECONDS` 限制，执行期间按 `AGENT_HUB_REASONING_TOOL_PROGRESS_SECONDS` 发送 `tool_progress`，超时发送 `tool_timeout` 并把失败结果交回模型。预算耗尽或工具异常回退普通生成时，最终回答仍以单个 `token` 事件下发。
- 审批重试：Chat 请求携带 `approval_id` 重放同一问题；`/v1/call` 通过 `envelope.approval_id` 重放；DAG 审批使用 `/v1/workflows/runs/{id}/approve|resume`。
