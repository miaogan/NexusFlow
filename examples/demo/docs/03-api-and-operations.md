# API 集成、安全与运维

## 认证方式

控制台使用登录返回的 JWT（`Authorization: Bearer <access_token>`）。外部系统可以使用两类凭证：

1. **API Key**：在“API Key”页面签发，作用域可选 `chat:completions` 与 `search:use`，可绑定允许访问的知识库，并设置每分钟请求上限。
2. **OpenAI 兼容端点**：`POST /v1/openai/chat/completions`，既接受 API Key，也接受控制台 JWT。

OpenAI SDK 的 `base_url` 应配置为 `http://<host>:<port>/v1/openai`，SDK 会自动拼接 `/chat/completions`。

## 常见错误码

- `401`：缺少或失效的凭证；API Key 被吊销后立即生效。
- `403`：权限不足或未被授予该知识库的角色。
- `404`：跨租户访问、资源不存在，或 MCP 会话未知。
- `409`：重复决策（例如已完成的审批再次批准）或资源冲突。
- `429`：超过 API Key 的每分钟请求上限或网关限流。
- `502`：下游服务不可用（例如检索服务故障、LLM 上游未配置）。

## Webhook 事件

支持的订阅事件包括：`task.completed`、`task.failed`、`index.completed`、`workflow.completed`、`workflow.failed`、`workflow.node.*`、`approval.decided`、`webhook.test`，以及通配符 `*`。

投递请求包含：

- `X-NexusFlow-Event`：事件类型。
- `X-NexusFlow-Delivery`：投递 ID。
- `X-NexusFlow-Timestamp`：时间戳。
- `X-NexusFlow-Signature`：`sha256=<hex(HMAC_SHA256(secret, body))>`。

接收方应使用订阅时返回的密钥校验签名，并保证校验逻辑在 5 秒内完成。失败投递可在控制台或 API 中重放。

## 审计与可观测性

- `/v1/audit` 记录工具调用、审批决策与 PII 脱敏事件，支持按动作、决策与时间范围查询。
- `/metrics` 暴露 Prometheus 指标；可在 `extras` profile 中启动 Prometheus 与 Grafana。
- 审批与审计在 PostgreSQL 中持久化，服务重启后仍然可见；MCP 会话同样共享在 PostgreSQL 中。

## 安全建议

- 生产环境关闭匿名访问（`ALLOW_ANONYMOUS=false`），并为每个集成方单独签发 API Key。
- 高风险工具（如 `sandbox.execute`、`workflow.run`）启用人工审批（HITL），审批记录会写入审计表。
- Webhook 目标地址默认禁止内网；仅在本机演示时设置 `WEBHOOK_ALLOW_PRIVATE=true`。
- 删除文档会级联清理对象存储、向量索引与 LightRAG 文档状态；删除知识库本身不会自动清理其历史文档，请先删除文档。
