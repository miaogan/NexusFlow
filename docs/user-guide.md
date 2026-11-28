# NexusFlow 用户使用指南

本指南面向使用 NexusFlow 控制台和 API 的业务与集成人员，覆盖从导入示例数据到日常使用的完整流程。

- 首次使用建议先看 [快速上手](./quickstart.md)
- 全部文档索引见 [文档中心](./README.md)
- 部署与运维细节见 [deployment.md](./deployment.md)
- 系统设计见 [architecture.md](./architecture.md)
- 开发者命令与实现边界见 [README](../README.md)

## 1. 启动与登录

### 1.1 本地 Docker Compose

```bash
cp .env.example .env          # 首次：按需填写 LLM / Embedding / MinerU 配置
make up                       # 构建并启动全部服务
```

默认端口（以 `.env` 为准）：

| 服务 | 地址 |
| --- | --- |
| 控制台 | http://127.0.0.1:8088 |
| Gateway API | http://127.0.0.1:${GATEWAY_PUBLISH_PORT:-8080}（示例 `.env` 为 18080） |
| Workflow Engine | http://127.0.0.1:8081 |
| Agent Hub | http://127.0.0.1:8001 |
| LlamaIndex 检索 | http://127.0.0.1:8000 |
| Qdrant | http://127.0.0.1:6333 |
| PostgreSQL | 127.0.0.1:${POSTGRES_PUBLISH_PORT:-5432} |

首次启动会写入种子管理员：`admin / admin123`（可用 `SEED_ADMIN_*` 覆盖）。打开控制台会自动跳转登录页，登录后可看到“概览 / 文件管理 / 任务中心 / 检索测试 / 智能问答”等菜单；管理类菜单按权限动态显示。

未登录时可点“以匿名身份浏览”（需 `ALLOW_ANONYMOUS=true`）。匿名身份仅能检索与问答，不能上传、管理或查看审计。

### 1.2 打开可选能力（示例数据与审批演示会用到）

以下开关默认关闭，写入 `.env` 后重新 `make up` 生效：

```dotenv
# 工作流：Agent 节点与人工审批
WORKFLOW_AGENT_NODE=true
HITL_ENABLED=true

# Agent Hub：工具调用、MCP、HITL、工作流工具
AGENT_HUB_TOOLS_ENABLED=true
AGENT_HUB_REASONING_ENABLED=true
AGENT_HUB_MCP_ENABLED=true
AGENT_HUB_HITL_ENABLED=true
AGENT_HUB_WORKFLOW_ENABLED=true
AGENT_HUB_MCP_SESSION_DRIVER=postgres
RAG_PII_ENABLED=true

# LightRAG 图谱增强（Embedding 维度必须与模型一致）
LLAMAINDEX_LIGHTRAG_ENABLED=true
LLAMAINDEX_LIGHTRAG_STORAGE_DRIVER=postgres
LLAMAINDEX_LIGHTRAG_GRAPH_STORAGE=PGTableGraphStorage
LLAMAINDEX_LIGHTRAG_EMBEDDING_DIM=1024
```

> LightRAG 需要 OpenAI 兼容的 `LLAMAINDEX_LLM_*`；图谱写入与查询在服务内串行执行，避免共享数据库连接池并发问题。

## 2. 导入示例数据

```bash
python scripts/seed_demo_data.py
```

脚本使用标准库，默认连接 `.env` 中的 Gateway 端口与管理员账号，执行以下操作（幂等，可重复运行）：

1. 创建知识库 **示例知识库**，并上传 `examples/demo/docs/*.md` 三篇文档（产品概览、检索链路、API 与运维）。
2. 等待解析与索引完成（文本文件走直通解析，无需 MinerU）。
3. 创建演示账号并授权该知识库：
   - `demo-viewer / Demo@12345` → 只读（`kb:viewer`）
   - `demo-editor / Demo@12345` → 可编辑（`kb:editor`）
4. 签发 API Key `demo-openai`（明文只显示一次）。
5. 执行一次混合检索与一次问答，验证链路可用。

常用参数：

```bash
python scripts/seed_demo_data.py --skip-users --skip-api-key   # 只导入文档
python scripts/seed_demo_data.py --password 'ChangeMe123' --kb-name '演示库'
```

清理示例数据：在“文件管理”中删除示例文档（会级联清理对象存储、向量与 LightRAG 状态），再删除知识库。注意：**删除知识库本身不会自动删除其历史文档**，请先删文档。

导入后可以尝试这些问题：

- NexusFlow 的混合检索包含哪些召回路由？
- LightRAG 在什么情况下替代内置图？
- 删除文档时会清理哪些数据？
- API Key 被吊销后多久生效？

### 2.1 全流程验证（带证据）

```bash
python scripts/e2e_demo.py --output docs/e2e-evidence.md
```

脚本会新建一个验证知识库并上传带唯一标记的 PDF，依次验证：MinerU 解析与 `layout.json`、Qdrant 向量维度、vector / bm25 / graph 三路召回、Gateway 混合检索、RAG 问答引用、循环子运行、人工审批恢复与重复决策 `409`、MCP 初始化与工具列表。执行后会把真实任务 ID、召回数量与回答摘录写入 `docs/e2e-evidence.md`，可随每次发版重新生成。

## 3. 控制台使用

### 3.1 知识库与文件

- 顶部知识库切换器决定当前页面操作的命名空间；列表与检索都按当前知识库过滤。
- “文件管理”支持上传 PDF、Word、Markdown、文本等格式。上传后任务进入“任务中心”，状态为 `PENDING → PARSING → INDEXING → COMPLETED`。
- 完成解析后，任务详情可以读取 MinerU 生成的布局 JSON（`layout.json`）与抽取图片；这些产物通过 `/v1/media` 受控读取。
- “重建索引”使用已保存的 Markdown 重新切块与向量化，不需要重新解析原文件。
- “删除”会级联清理对象存储、Markdown/布局产物、向量与 LightRAG 文档状态。

权限：平台 `viewer` 可以查看和检索；上传/删除需要 `editor` 或对应知识库的 `kb:editor` / `kb:admin`。

### 3.2 检索测试

- 选择召回路由（vector / bm25 / filter / parent / graph）、每路 `top_k` 与最终条数后执行检索。
- 结果会显示融合后的片段与元数据；`meta.retriever=lightrag` 表示该片段来自 LightRAG 图谱路由。
- 对结果点赞/点踩会写入反馈，用于后续排序惩罚与评测。
- 空知识库检索返回 `200` 与空结果，不视为故障。

### 3.3 智能问答

- 支持多轮对话，回答附带引用角标，可跳转查看原片段。
- 若配置了纠错历史，相似问题会参考人工修正后的答案。
- 回答不满意时可在消息上提交评分或“纠正答案”，纠正内容会进入纠错池。
- 开启 `RAG_PII_ENABLED=true` 后，邮箱、手机号等会在提示词与上下文中脱敏，审计中记录 `pii.mask`。

### 3.4 工作流编排

“工作流编排”页面提供节点库、画布、校验与运行，并可导出 DAG JSON。可用节点：

| 类型 | 说明 | 关键配置 |
| --- | --- | --- |
| `noop` / `set` | 占位与变量赋值 | `set`: `key`、`value` |
| `ingest` / `parse` / `index` / `sandbox` / `query` | 声明式阶段节点 | 无 |
| `condition` | 条件分支 | `field`、`equals`/`contains`/`in`、`then`、`else` |
| `loop` | 按列表并发执行子 DAG | `items`、`max_concurrency`、`sub_dag` |
| `human_approval` | 挂起等待人工审批 | 需 `HITL_ENABLED=true` |
| `agent` | 调用 Agent Hub 的能力或工具 | `agent` 或 `capability`、`input`；需 `WORKFLOW_AGENT_NODE=true` |
| `nl2sql` | 只读结构化查询 | `sql` / `sql_from` 等 |

示例文件（请求体可直接用于 `/v1/workflows/run`）：

- `examples/demo/workflows/conditional-routing.json`：条件分流，运行后未选中的分支会被标记为 skipped。
- `examples/demo/workflows/loop-batch.json`：并行批处理，子运行会以 `parent_run_id` 关联到父运行。
- `examples/demo/workflows/approval-gate.json`：人工审批后继续（需 `HITL_ENABLED=true`）。
- `examples/demo/workflows/agent-intent-loop.json`：循环调用 `intent.classify`（需 Agent 节点与意图 LLM）。

命令行运行示例：

```bash
GATEWAY=http://127.0.0.1:18080
TOKEN=$(curl -s $GATEWAY/v1/auth/login -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin123"}' | python -c 'import sys,json;print(json.load(sys.stdin)["access_token"])')

curl -s $GATEWAY/v1/workflows/run -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d @examples/demo/workflows/loop-batch.json
```

### 3.5 审批中心

以下动作会生成审批单：

- 工作流执行到 `human_approval` 节点；
- 工作流通过 `workflow.run` 工具触发（高风险工具）；
- 其他被标记为 `requires_approval` 的工具调用。

在“审批中心”可以批准或拒绝；批准后工作流从挂起点恢复。对已完成或已拒绝的运行再次决策会返回 `409`。审批记录与审计持久化在 PostgreSQL，重启后仍可查询。

命令行示例：

```bash
# 运行会挂起的工作流（approval-gate.json）
curl -s $GATEWAY/v1/workflows/run -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d @examples/demo/workflows/approval-gate.json

curl -s $GATEWAY/v1/workflows/runs/<run_id>/approve -X POST \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"decision":"approve","reason":"demo"}'
```

### 3.6 SubAgent 与 MCP

“SubAgent 管理”页面可以注册 HTTP 或 Sandbox Agent，声明能力与技能，并发起调试调用（写操作需 `user:write`，Hub 需开启动态注册）。

开启 `AGENT_HUB_MCP_ENABLED=true` 后，Agent Hub 在 `http://127.0.0.1:8001/mcp` 暴露 MCP（Streamable HTTP）。客户端配置示例：

```json
{
  "mcpServers": {
    "nexusflow": {
      "type": "http",
      "url": "http://127.0.0.1:8001/mcp",
      "headers": { "x-admin-token": "<AGENT_HUB_ADMIN_TOKEN>" }
    }
  }
}
```

可用工具：`search.query`（知识库检索）、`workflow.run`（需审批）。设置 `AGENT_HUB_MCP_ALLOW_SANDBOX=true` 后会额外暴露 `sandbox.execute`。会话保存在 PostgreSQL，服务重启后仍可继续使用；关闭事件流不会使会话失效，只有显式 `DELETE /mcp` 才会终止。

### 3.7 API Key 与 OpenAI 兼容调用

在“API Key”页面签发 Key，选择作用域（`chat:completions`、`search:use`）、允许访问的知识库与每分钟上限。明文只在创建时显示。

```bash
GATEWAY=http://127.0.0.1:18080

curl -s $GATEWAY/v1/openai/chat/completions \
  -H "Authorization: Bearer nf-sk-..." -H 'Content-Type: application/json' \
  -d '{"model":"nexusflow","kb_id":"<kb_id>","messages":[{"role":"user","content":"NexusFlow 的召回路由有哪些？"}]}'
```

Python（OpenAI SDK）：

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:18080/v1/openai",
    api_key="nf-sk-...",
)
resp = client.chat.completions.create(
    model="nexusflow",
    messages=[{"role": "user", "content": "NexusFlow 的召回路由有哪些？"}],
)
print(resp.choices[0].message.content)
```

Key 被吊销后立即返回 `401`；超过 `rate_limit_rpm` 返回 `429`。

### 3.8 Webhook

在“Webhook”页面订阅事件并保存密钥（只显示一次）。支持的事件：

`task.completed`、`task.failed`、`index.completed`、`workflow.completed`、`workflow.failed`、`workflow.node.started`、`workflow.node.completed`、`workflow.node.failed`、`workflow.node.waiting_approval`、`workflow.node.skipped`、`approval.decided`、`webhook.test`、`*`。

签名校验（Python）：

```python
import hashlib, hmac

def verify(secret: str, body: bytes, signature: str) -> bool:
    expected = "sha256=" + hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(signature, expected)
```

本机回环地址默认被拒绝；演示环境可设置 `WEBHOOK_ALLOW_PRIVATE=true`。

### 3.9 数据连接器

“连接器”API（控制台通过 Gateway 代理）支持三种类型：

| 类型 | 用途 | 主要配置 |
| --- | --- | --- |
| `web_crawler` | 抓取站点页面 | `urls`、`max_depth`、`max_pages`、`allow_private` |
| `s3_bucket` | 同步 S3/MinIO 对象 | `endpoint`、`bucket`、`prefix`、`access_key`、`secret_key` |
| `db_query` | 只读 SQL 按行生成文档 | `dsn`、`sql`、`id_column`、`max_rows` |

示例（容器内访问宿主机页面）：

```bash
curl -s $GATEWAY/v1/connectors -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{
    "name": "demo-web", "kb_id": "<kb_id>", "type": "web_crawler",
    "config": {"urls": ["http://host.docker.internal:8080/demo.html"],
               "max_depth": 1, "max_pages": 5, "allow_private": true}
  }'

curl -s -X POST $GATEWAY/v1/connectors/<connector_id>/sync -H "Authorization: Bearer $TOKEN"
```

同步会为抓取到的每个文档创建上传任务，随后与普通文件一样进入解析与索引流程。

### 3.10 沙箱

“沙箱 Playground”可以执行 Python / Node 代码，输入通过 `input_json` 传入，输出写入 `output_json`。平台角色需要 `sandbox:execute`（`editor` 及以上）。超时、包安装与网络策略由 `SANDBOX_*` 配置控制。

### 3.11 用户与知识库权限

- 平台角色：`admin`（全部权限）、`editor`（内容 + 检索 + 沙箱 + 工作流）、`viewer`（只读检索）。
- 知识库角色：`kb:admin`、`kb:editor`、`kb:viewer`，在知识库“管理”弹窗中授权给用户或角色。
- 自定义角色可在“用户管理”中创建并勾选权限点。
- 跨租户访问统一返回 `404`；无该知识库授权返回 `403`。

### 3.12 用量、审计与指标

- “概览”展示任务数、存储/文件/问答配额使用情况。
- “审计日志”可按 `tool.call`、`approval.*`、`pii.mask` 等动作与时间范围查询。
- Gateway `GET /metrics` 提供 Prometheus 指标；`extras` profile 可启动 Prometheus 与 Grafana：

```bash
docker compose --env-file .env -f deploy/docker-compose.yml --profile extras up -d prometheus grafana
```

## 4. API 速查

所有受保护接口都需要 `Authorization: Bearer <token>`；权限列表示平台权限或知识库角色。

| 方法 | 路径 | 说明 | 权限 |
| --- | --- | --- | --- |
| POST | `/v1/auth/login` | 登录，返回 access/refresh token | 公开 |
| GET | `/v1/me` | 当前身份与权限 | 登录 |
| GET/POST | `/v1/knowledge-bases` | 知识库列表 / 创建 | `kb:read` / `kb:write` |
| POST | `/v1/files` | 上传文件（multipart 或元数据登记） | `file:write` + `kb:editor` |
| DELETE | `/v1/files/{id}` | 级联删除文档 | `file:delete` + `kb:editor` |
| POST | `/v1/files/{id}/reindex` | 使用已存 Markdown 重建索引 | `task:write` |
| GET | `/v1/tasks`、`/v1/tasks/{id}` | 任务列表 / 状态与 metadata | `task:read` |
| GET | `/v1/media?key=` | 读取解析图片或 layout.json | `search:use` |
| POST | `/v1/search` | 多路召回 + 融合检索 | `search:use` |
| POST | `/v1/chat` | RAG 问答（返回 citations） | `search:use` |
| GET | `/v1/conversations` | 会话列表 / 详情 | `search:use` |
| POST/GET | `/v1/feedback` | 提交评分或纠错 / 列表 | 登录 / `user:read` |
| GET | `/v1/corrected-answers?query=` | 相似问题的人工纠正 | `search:use` |
| POST | `/v1/workflows/validate`、`/run` | DAG 校验 / 运行 | `workflow:run` |
| GET | `/v1/workflows/runs`、`/{id}`、`/{id}/children` | 运行列表 / 详情 / 循环子运行 | `workflow:run` |
| POST | `/v1/workflows/runs/{id}/approve` | 批准或拒绝并恢复 | `workflow:run` |
| GET/POST | `/v1/approvals`、`/{id}/decide` | 工具审批箱 | `workflow:run` |
| GET | `/v1/audit` | 审计查询 | `trace:read` |
| GET/POST/DELETE | `/v1/agents`、`/v1/agents/{name}/call` | SubAgent 管理与调用 | `user:read` / `search:use` |
| GET/POST/DELETE | `/v1/api-keys` | 签发 / 列表 / 吊销 | `apikey:write` |
| POST | `/v1/openai/chat/completions` | OpenAI 兼容端点 | `chat:completions` |
| GET/POST | `/v1/webhooks` | 订阅管理与测试 | `webhook:write` |
| GET/POST | `/v1/connectors` | 数据连接器 | `kb:write` |
| POST | `/v1/sandbox/execute` | 执行代码 | `sandbox:execute` |
| GET/POST | `/v1/users`、`/v1/roles` | 用户与角色管理 | `user:read` / `role:read` |

## 5. 常见问题

**控制台打不开或 API 连不上**
确认 `make up` 后所有服务为 healthy；端口以 `.env` 的 `GATEWAY_PUBLISH_PORT` / `POSTGRES_PUBLISH_PORT` 为准，若与本机其他项目冲突请修改后重启。

**上传后一直 PARSING / 任务失败**
检查 `MINERU_TARGET` 是否指向可用的 MinerU 服务；没有 MinerU 时可改用 Markdown/文本文件验证链路。任务详情里的 `error_msg` 会给出具体原因。

**检索返回 502**
通常是检索服务（LlamaIndex/query-service）未启动或正在重启；空知识库应返回 `200` 空结果，若出现 502 请查看 `query-service` 日志中的 `recall route failed`。

**菜单看不到审批中心 / 审计 / API Key**
这些页面按权限显示：审批需 `workflow:run`，审计需 `trace:read`，API Key 需 `apikey:write`。另外审批依赖 `HITL_ENABLED=true` 与 `AGENT_HUB_HITL_ENABLED=true`。

**MCP 客户端连接 401**
Agent Hub 配置了管理员令牌时，请求必须带 `x-admin-token`（或 `x-service-token`），值与 `AGENT_HUB_ADMIN_TOKEN` 一致。

**LightRAG 没有生效**
确认 `LLAMAINDEX_LIGHTRAG_ENABLED=true`、`LLAMAINDEX_LLM_*` 可用，且 `LLAMAINDEX_LIGHTRAG_EMBEDDING_DIM` 与 Embedding 模型实际维度一致。图谱写入耗时较长，可在检索结果的 `meta.retriever` 中确认是否命中。

**示例账号无法登录**
`demo-viewer` / `demo-editor` 由种子脚本创建，密码默认 `Demo@12345`；若已存在则不会重置密码，可用管理员在“用户管理”中更新。
