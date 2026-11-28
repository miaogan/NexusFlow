# NexusFlow v1.2.0 演进与 Agent 自动执行任务清单 (ROADMAP_V1_2)

> **状态：已完成（Milestone 1–3 全部交付）。** 本文件是 v1.2.0 的版本计划摘要；任务拆解与验收命令在执行时补充。
>
> **核心目标**：实现**用户自定义 SubAgent + OpenSandbox 安全动态编排**，包含多租户表结构迁移、注册表隔离、沙箱执行器打通、动态技能路由以及前端管理控制台。
>
> **执行约定**：按 Milestone 顺序执行；每个 Task 验证通过后更新为 `[x]` 并独立提交；存储变更以 Postgres 幂等迁移为准，同时保留内存降级路径。

## Milestone 1: 存储契约与多租户注册表隔离

* **[x] Task 1.1**: 定义自定义 SubAgent 存储模型与 Postgres 幂等迁移 (`docs/schema_enterprise_v1_1_to_1_5.sql`)。
  * 交付：新增 `custom_agents` 表（主键 `tenant_id + name`，含 capabilities / schema / risk / tools 等契约字段）；Agent Hub `core/agent_store.py` 提供 `InMemoryAgentStore` 与 `PostgresAgentStore`，Postgres 模式启动时幂等建表；数据库不可用时回退内存并告警。
* **[x] Task 1.2**: 改造 `AgentRegistry` 实现多租户隔离与 CRUD 接口暴露 (`services/agent-hub/src/nexusflow_agents/core/registry.py`)。
  * 交付：注册表以 `(tenant_id, name)` 为键，内置 Agent 全局共享、自定义 Agent 仅所属租户可解析，租户 Agent 在同优先级下优先；`/v1/agents` 提供按 `x-tenant-id` 限定的 GET/POST/GET{name}/DELETE 与心跳；启动时自动加载持久化 Agent。写接口仍受 `AGENT_HUB_DYNAMIC_REGISTRATION` 与 Admin Token 约束。

## Milestone 2: OpenSandbox 调度打通与动态技能路由

* **[x] Task 2.1**: 实现 `SandboxAgentExecutor` 适配器打通安全代码执行。
  * 交付：`AgentSpec` 新增 `kind=sandbox` 与 `sandbox` 配置（code/runtime/timeout/packages/env_vars），存储层新增 `custom_agents.sandbox` 列；`SandboxAgentExecutor` 经 Gateway `/v1/sandbox/execute` 执行，透传用户 Bearer 强制 `sandbox:execute`；新增代码/入参大小与超时上限、列表脱敏、`POST /v1/agents/{name}/call` 调用入口；HTTP 自定义 Agent 改为直接调用注册的 endpoint。
* **[x] Task 2.2**: 升级 `Intent / Router` 节点为动态技能路由（Dynamic Skill Routing）。
  * 交付：`AgentSpec`/存储新增 `skills`；`AgentRegistry` 提供 `resolve_skill` 与 `available_skills`；`Hub.call_skill` 按租户/优先级选择 SubAgent；Intent 节点接收技能列表并输出 `intent=skill + skill`，规则与 LLM 双通道；`SkillRouter` 接入 Chat（同步 + SSE），技能失败自动回退 RAG；新增 `GET /v1/skills`。

## Milestone 3: 前端可视化编排控制台与全链路验收

* **[x] Task 3.1**: 前端控制台新增“自定义 SubAgent”管理与在线调试页 (`web/src/pages/SubAgents.tsx`)。
  * 交付：新增 `/subagents` 页面与导航（`user:read` 可见，写操作需 `user:write`）；支持 HTTP / Sandbox Agent 的列表、创建/更新、删除、技能声明、在线调用调试与技能清单；Gateway 新增 `/v1/agents` CRUD 与 `/v1/skills` 代理；Sandbox 元数据更新可省略代码并复用已存代码。
* **[x] Task 3.2**: 扩展真机冒烟测试并同步项目文档 (`scripts/smoke_test.py`)。
  * 交付：冒烟脚本新增 Agent Hub `/readyz`、`/v1/agents`、`/v1/skills` 检查；README、架构、部署、`.env.example`、Compose/K8s 同步 SubAgent 管理、技能路由与权限说明。

## 后续待办（未开始）

> v1.2.0 的三阶段目标已全部交付。以下能力未纳入本版本，拆解与验收标准统一维护在 [ROADMAP_V1_1_V1_5.md](ROADMAP_V1_1_V1_5.md)，按编号执行即可。

* **Task 5.1 对外服务**：租户级 API Key 签发/吊销/作用域，以及 OpenAI 兼容 `/v1/openai/chat/completions`。
* **Task 5.2 生态集成**：Webhook 订阅（HMAC 签名、指数退避、投递日志）；MCP Server 已交付，Webhook 未开始。
