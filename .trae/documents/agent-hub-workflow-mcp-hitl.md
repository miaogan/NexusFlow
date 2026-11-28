# Agent Hub × Workflow × MCP × HITL 演进方案

## Context

Agent Hub 已完成单容器与星型调度，但目前仍是固定能力（intent / summary / memory）和单轮生成，无法与 Go Workflow DAG 双向编排，也没有工具循环、MCP 与审批护栏。Go Workflow 仍是同步 JSON DAG，无鉴权、无 run 状态、无挂起恢复；Sandbox 与外部生态缺少统一工具契约。

目标分四条主线：

1. **双向联动**：DAG 可调用 Agent Hub；Agent 可调用/触发 Workflow。
2. **自助推理**：LLM 多轮 plan → tool → observe → decide，带预算与停止条件。
3. **外部生态**：标准 Tool/Agent 契约 + MCP Server/Client。
4. **安全治理**：工具风险分级、护栏、审计、人机审批与恢复。

所有能力默认带开关，保留现有 `/v1/chat`、SSE 与 `/v1/workflows/run` 兼容路径。

## 实施状态（2026-11-27）

| 阶段 | 状态 | 已交付 | 边界 / 后续 |
| --- | --- | --- | --- |
| P0 统一契约 | 已完成 | `ToolSpec` / `ToolRegistry` / `ToolService`、`CallEnvelope`、`AgentSpec` 契约扩展、`POST /v1/call`、`GET /v1/tools` | — |
| P1 双向联动 | 已完成 | Go `agent` 节点、`ExecutionContext` 身份元数据、Gateway `/v1/workflows/*` 代理、服务令牌、Hub `workflow.run` 工具 | `/v1/call` 的 agent 分支恒可用；tool 分支需 `AGENT_HUB_TOOLS_ENABLED` |
| P2 推理循环 | 已完成 | `ReasoningLoop`（预算/重复检测/失败回退）、`tool_call` / `tool_result` / `approval_required` SSE、最终回答逐 token 流式、`ChatService` 集成 | `tool_call` 进度与超时事件未实现 |
| P3 MCP 与契约 | 已完成 | `POST /mcp`（initialize / tools / resources / prompts）、Streamable HTTP（`GET` / `DELETE /mcp` + `Mcp-Session-Id`）、`GET /mcp/sse` + `POST /mcp/messages`、stdio 入口、MCP Client、`mcp.<server>.<tool>`、动态注册复用同一 Schema | 会话与订阅为进程内状态；`resources/listChanged` 主动推送未实现 |
| P4 人机协同 | 已完成 | `ApprovalService`、高风险工具 202 + `approval_id`、审批 API、`human_approval` 节点、`RunStore`、runs / approve / resume、Web `/approvals` | Postgres 运行/审批/审计已接入；节点级 `EventSink` 已交付（`workflow.node.*`） |
| P5 安全护栏 | 已完成 | 风险分级、JSON Schema、输出截断、预算 429、审计、工具结果不可信包裹、sandbox 权限与 local 后端生产保护 | 审计为进程内环形缓冲；PII 脱敏不属于本期 |

各阶段保留旧路径：关闭开关即回到 `retrieve → generate` 与同步 `/v1/workflows/run`。实现细节以本文件后续章节 + 代码为准，环境变量名以下表为准。

## P0 统一契约与通用调用（前置）

新增：

- `core/tooling.py`：`ToolSpec{name, description, input_schema, output_schema, risk, timeout_ms, requires_approval}`、`ToolRegistry`、JSON Schema 校验。
- `core/envelope.py`：`CallEnvelope{tenant_id, user_id, trace_id, request_id, approval_id, deadline_ms, budget}`。
- `core/registry.py`：`AgentSpec` 扩展 `tools` / `model` / `max_turns` / `token_budget` / `risk`（默认值保持兼容）；能力白名单增加 `agent.call`、`tool.call`。
- `api/call.py`：`POST /v1/call`，body `{agent|tool, input, envelope}`，内部复用 `Hub.call`。
- 统一 envelope 也用于 MCP、Workflow agent 节点与审批恢复。

验证：`pytest tests/test_registry.py tests/test_hub.py tests/test_contracts.py`。

## P1 双向最小闭环

### DAG → Agent Hub

- 新增 `backend/internal/workflow/agent_node.go`：`type=agent`，config `{agent, capability, input}`；请求 `/v1/call`，透传 `x-tenant-id` / `x-trace-id` / deadline；输出并入 `ExecutionContext`。
- `cmd/workflow-engine/main.go`、`cmd/dev-all/main.go` 注册节点。
- `ExecutionContext` 增加 tenant/user/trace 元数据（不破坏现有 `vars/nodes`）。

### Agent Hub → Workflow

- 新增 `agents/rag/tools/workflow.py`：`workflow.run` 工具，经 Gateway 调 Go；限制节点/边数量、深度、超时，禁止 `workflow.run` 嵌套。
- Gateway 新增 `/v1/workflows/validate|run` 代理，挂 `workflow:run` 权限；`/wf` 保留一版兼容。
- Workflow 服务增加内部服务令牌校验（`WORKFLOW_SERVICE_TOKEN`），防止绕过 Gateway 直调。

验证：`go test ./internal/workflow ./internal/gateway`；经 Gateway 运行 `agent → query` DAG，trace_id 在 Go/Hub 两侧一致。

## P2 ReasoningLoop（多轮推理 + 工具调用）

新增 `agents/rag/services/reasoning.py`：

- LLM `bind_tools`；循环 `plan → tool_call → tool_result → decide → final`。
- 停止条件：`max_turns`（默认 6）、总 deadline、token/调用预算、重复 `tool+args` 指纹检测、无进展检测。
- 工具失败返回结构化错误，由模型决定重试/换工具/直接回答；耗尽预算回退现有 `generate`。
- SSE 在现有事件基础上新增 `tool_call` / `tool_result` / `approval_required`，`stage` / `token` / `done` 不变。

`agents/rag/services/chat.py`：存在工具时走 ReasoningLoop，否则保持原 `retrieve → generate`；异常自动回退。

验证：`pytest tests/rag` + 新增 `test_reasoning.py`（轮次上限、预算、重复调用、工具失败、SSE 事件）。

## P3 MCP 与标准 Agent 契约

### MCP Server

- 新增 `mcp/server.py`，HTTP JSON-RPC 2.0：`initialize`、`tools/list`、`tools/call`；`/mcp` 单入口。
- 工具来源为 `ToolRegistry`，禁止直接暴露 sandbox；输入输出走同一 JSON Schema。

### MCP Client

- 新增 `mcp/client.py`：仅允许配置的 server URL；限制超时、响应大小、并发；外部工具注册为 `mcp.{server}.{tool}`。
- 配置：`AGENT_HUB_MCP_SERVERS`、`AGENT_HUB_MCP_TIMEOUT_SECONDS`、`AGENT_HUB_MCP_MAX_BYTES`、`AGENT_HUB_MCP_ALLOW_SANDBOX`。

### 标准 Agent 契约

- `AgentSpec` 暴露 `input_schema` / `output_schema` / `version` / `permissions` / `tenant_scope`。
- 动态注册接口复用同一校验；MCP tool schema 与 `ToolSpec` 同源。

验证：`curl :8001/mcp` 完成 initialize/list/call；`resources/list` + `resources/read`（`nexusflow://agents` / `nexusflow://tools`）、`prompts/list` + `prompts/get`（`rag_answer`）可用；`GET /mcp/sse` 获取 endpoint 后经 `POST /mcp/messages` 收发；`python -m nexusflow_agents.mcp.stdio` 处理 stdin/stdout JSON-RPC；外部 MCP tool 注册后需审批，批准后可在 ReasoningLoop 中被调用。

## P4 人机协同（HITL）

### Agent 工具审批

- 新增 `agents/rag/services/approvals.py`：`approval_id → checkpoint`；高风险工具返回 `approval_required`，不执行。
- `POST /v1/approvals/{id}/decide`（approve/reject）复用 Gateway 身份与 RBAC；批准后 resume 同一轮推理。
- 默认 `requires_approval`：sandbox、写操作、workflow.run、外部 MCP 写工具。

### Workflow 人工节点

- `engine.go` 增加 `RunOptions{Meta, RunID, Store, Resume, Approved...}`；新增 `human_approval` 节点，返回 `WAITING_APPROVAL` 并保存 `ExecutionContext` 快照。`EventSink` 已实现：节点开始/完成/失败/等待审批/跳过时发出 `NodeEvent`，并可桥接为 `workflow.node.*` Webhook；循环子运行通过 `Nested` 避免重复上报。
- `workflow/http.go` 增加 runs / approve / reject / resume；`MemoryRunStore` 与 Postgres `RunStore`（`workflow_executions` / `workflow_approvals`）均已接入，跨副本审批通过行锁 + 状态 CAS 保证只生效一次。
- Gateway 已增加审批代理（`/v1/approvals*`、`/v1/workflows/runs*`）；Web 审批中心（工具审批收件箱 + 工作流运行时间线）已交付于 `/approvals`；重复审批返回 409。

验证：Agent 高风险工具无审批返回 202，批准后重放恢复；Workflow 挂起 → 审批 → 同一进程内恢复。跨进程重启恢复需先接入 Postgres `RunStore`。

## P5 安全护栏与审计

- 工具风险分级：`read / write / execute / external`；参数 JSON Schema、输出大小/敏感字段截断。
- Sandbox：`sandbox:execute` 权限挂到 Gateway；`SANDBOX_BACKEND=local` 仅开发且显式开启；egress 默认拒绝；TTL 上限 5 分钟。
- 预算：每轮 token/工具调用/总时限；`/v1/call` 工具预算/时限超限返回 429，推理循环超限停止并回退普通生成。
- 审计：进程内 `AuditLog` 记录 tool、risk、decision、tenant、trace，`GET /v1/audit` 查询；`AGENT_HUB_AUDIT_DRIVER=postgres` 时落库 `audit_logs`（Task 3.3 已交付）。
- Prompt 注入：工具结果显式标记为不可信数据，禁止覆盖 system 指令。

验证：无 token 401、越权 403、高风险未审批 202、超预算 429、审计可查询。

## 阶段顺序与开关

`P0 → P1 → P2 → P3 → P4 → P5`，每阶段独立提交；以下为代码实际使用的环境变量：

| 开关 | 默认 | 作用 |
| --- | --- | --- |
| `AGENT_HUB_TOOLS_ENABLED` | false | 工具注册、`/v1/call` 的 tool 分支、`GET /v1/tools`、`GET /v1/audit` |
| `AGENT_HUB_WORKFLOW_ENABLED` | false | 注册 Hub → DAG 的 `workflow.run` 工具 |
| `WORKFLOW_AGENT_NODE` | false | Go `agent` 节点（DAG → Hub） |
| `AGENT_HUB_REASONING_ENABLED` | false | ReasoningLoop 与工具调用循环 |
| `AGENT_HUB_MCP_ENABLED` | false | `/mcp` Server 与外部 MCP Client |
| `AGENT_HUB_HITL_ENABLED` | false | 工具审批与 `/v1/approvals` |
| `HITL_ENABLED` | false | DAG `human_approval` 节点与 runs 持久化 |

辅助参数：`AGENT_HUB_WORKFLOW_URL` / `AGENT_HUB_WORKFLOW_SERVICE_TOKEN`、`AGENT_HUB_REASONING_{MAX_TURNS,TIMEOUT_SECONDS,TOOL_BUDGET,TOKEN_BUDGET}`、`AGENT_HUB_MCP_{SERVERS,TIMEOUT_SECONDS,MAX_BYTES,ALLOW_SANDBOX}`、`WORKFLOW_SERVICE_TOKEN`、`AGENT_HUB_URL` / `AGENT_HUB_TOKEN`、`SANDBOX_ALLOW_LOCAL`。

每阶段保留旧路径；关闭开关即回滚。Workflow 故障不影响 Chat，工具异常回退原 generate。

## 接口清单

| 方法 / 路径 | 所属 | 开关 / 权限 | 说明 |
| --- | --- | --- | --- |
| `POST /v1/call` | Agent Hub | agent 分支恒可用；tool 分支需 `AGENT_HUB_TOOLS_ENABLED` | 调用指定 agent 或 tool，body 携带 `envelope` |
| `POST /v1/agents/{name}/call` | Gateway | `search:use`；sandbox agent 另需 `sandbox:execute` | 调用自定义 SubAgent |
| `GET /v1/tools` | Agent Hub | 同上 | 工具契约列表 |
| `GET /v1/approvals`、`GET /v1/approvals/{id}` | Agent Hub | 审批开关 | 待办与详情 |
| `POST /v1/approvals/{id}/decide` | Agent Hub / Gateway | `workflow:run` | 批准 / 拒绝，重复决策 409 |
| `GET /v1/audit` | Agent Hub / Gateway | `trace:read` | 工具与审批审计查询 |
| `POST /mcp` | Agent Hub | `AGENT_HUB_MCP_ENABLED` | MCP JSON-RPC：initialize / tools/list / tools/call |
| `POST /v1/workflows/{validate,run}` | Gateway | `workflow:run` | DAG 校验与执行 |
| `GET /v1/workflows/runs`、`GET /v1/workflows/runs/{id}` | Gateway | `workflow:run` | 运行列表与详情 |
| `POST /v1/workflows/runs/{id}/{approve,resume}` | Gateway | `workflow:run` | 审批并恢复挂起的 run |
| `type=agent` 节点 | Workflow Engine | `WORKFLOW_AGENT_NODE` | 调用 Hub `/v1/call`，支持 `${node.field}` 引用上游输出 |
| `type=human_approval` 节点 | Workflow Engine | `HITL_ENABLED` | 挂起为 `WAITING_APPROVAL` 并保存检查点 |

## 剩余工作（P6+）

1. **持久化**：将 `MemoryRunStore` / `ApprovalService` / `AuditLog` 接入 `docs/schema_enterprise_v1_1_to_1_5.sql` 已定义的 `workflow_executions` / `workflow_approvals` / `audit_logs`；解决多副本下的 run 粘性路由与审批一致性。
2. **MCP 传输与能力**：Streamable HTTP、`resources/subscribe` 与三种原语均已交付；剩余 `resources/listChanged` 主动推送，以及会话/订阅跨副本共享（当前需粘性会话）。
3. **推理流式体验**：最终回答已改为逐 token 流式下发；剩余为 `tool_call` 进度事件与单工具超时控制。
4. **控制台**：新增审批收件箱、run 时间线 / 挂起恢复入口、审计查询页；与 `workflow:run` / `trace:read` 权限联动。
5. **端到端验证**：Gateway 发起 `agent → query` DAG 并核对 Go/Hub 两侧 trace；Chat 高风险工具审批 → 重试 → 结果；外部 MCP 工具在推理循环中调用。
6. **安全增强**：PII 脱敏、审计持久化与按月分区、MCP 外部域白名单与出口审计。

## 关键文件

- Agent Hub 契约：`core/{tooling,envelope,registry,hub,audit,config,bootstrap,runtime}.py`
- Agent Hub 接口：`api/{call,approvals,audit,admin}.py`、`main.py`
- Agent Hub 服务：`agents/rag/services/{reasoning,approvals,chat,llm}.py`、`agents/rag/tools/{workflow,gateway}.py`
- Agent Hub MCP：`mcp/{server,client}.py`
- Go Workflow：`backend/internal/workflow/{agent_node,approval,run_store,engine,http,context}.go`
- Go Gateway：`backend/internal/gateway/{workflow,agents,gateway,rag}.go`
- 入口装配：`backend/cmd/{workflow-engine,gateway,dev-all}/main.go`、`backend/internal/config/config.go`
- 契约/Schema：`docs/schema_enterprise_v1_1_to_1_5.sql` 的 `workflow_executions` / `workflow_approvals` / `audit_logs`（目标模型，待运行时接入）
- 测试：`services/agent-hub/tests/{test_tools,test_mcp,test_approvals_audit}.py`、`tests/rag/test_reasoning.py`、`backend/internal/workflow/{agent_node_test,approval_test}.go`
- 文档与部署：README、`docs/architecture.md`、`docs/deployment.md`、`.env.example`、Compose/K8s/CI

## 验证

已执行（2026-11-27）：

1. `cd services/agent-hub && python -m pytest -q` → 106 passed
2. `cd services/agent-hub && python -m ruff check src tests` → All checks passed
3. `cd backend && go build ./...` → 通过
4. `go test ./internal/workflow ./internal/gateway` → ok
5. `cd deploy && docker compose config -q` → 通过
6. 全量 `go test ./...` 仅 `internal/sandbox` 的本机 Python 执行用例失败（`exit status 9009`，环境无该命令），与本期改动无关

待真机联调：

1. 经 Gateway 跑 `agent → query` DAG，核对 trace_id 跨 Go/Hub 一致
2. `/v1/chat/stream` 验证 `tool_call` / `tool_result` 与旧 `stage` / `token` 事件
3. MCP initialize / tools/list / tools/call（含外部 MCP 工具在推理循环中调用）
4. 高风险工具审批 → 重试；Workflow `human_approval` 挂起 → 审批 → 同一进程内恢复（跨重启需先接 Postgres RunStore）
5. 安全用例：401 / 403 / 202 / 429 与审计查询
