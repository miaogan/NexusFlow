# NexusFlow v1.1.0 – v1.5.0 企业级能力演进方案（ROADMAP_V1_1_V1_5）

> **定位**：本文件是面向 v1.1.0 至 v1.5.0 的能力演进方案，共 5 个 Milestone、11 个结构化任务。
> **2026-11-27 增补**：Agent Hub × Workflow × MCP × HITL 已按规划文档 `.trae/documents/agent-hub-workflow-mcp-hitl.md` 交付；Task 3.2 完成（运行状态为进程内存储，跨副本持久化拆为 Task 3.3），Task 5.2 的 MCP Server 部分提前落地，其余任务状态不变。
> **v1.2.0 范围调整**：最新版本计划见 [ROADMAP_V1_2.md](ROADMAP_V1_2.md)；本文件 Milestone 2 保留为原规划记录。
> **与 `ROADMAP_V1_1.md` 的关系**：`ROADMAP_V1_1.md` 是上一轮已执行完毕的交付清单（知识库实体、级联删除、配额计量、K8s HPA，对应提交 c880e64 / 44b9284 / 9bc2c01 / a244bd7）。本文件在此基础上重新规划更完整的企业级能力，Milestone 1 为增量升级。
> **执行约定**：按 Milestone 顺序逐个执行，禁止跨任务并发；每个任务验证通过后提交独立 Commit，并将任务状态更新为 `[x]`；接口变更优先更新 proto 契约，存储扩展保留 Memory/Mock 降级路径。
> **数据契约**：涉及数据库表结构新增或变更时，必须严格对齐 [docs/schema_enterprise_v1_1_to_1_5.sql](docs/schema_enterprise_v1_1_to_1_5.sql) 中的表名、字段名与状态枚举值，并在 Postgres 初始化逻辑中保证幂等建表。

---

## Milestone 1: 知识库生命周期管理与精细化权限（v1.1.0）

* **[x] Task 1.1: 实现 kb_id 维度的独立生命周期与多维度 RBAC 权限隔离（kb:admin / kb:editor / kb:viewer），改造网关鉴权与前端切换器**
  * 历史现状：基础版 `kb_id` 隔离与知识库 CRUD 已交付（c880e64），此前仅有 `kb:read` / `kb:write` 两档权限。
  * 交付情况：已新增 `knowledge_bases` / `kb_permissions`（旧 `identity_knowledge_bases` 自动迁移），支持按 `kb_id` 授予 `kb:admin` / `kb:editor` / `kb:viewer` 并按用户或角色绑定；网关在文件上传/删除/重建、检索问答与知识库管理入口按角色校验，前端切换器展示当前库角色并限制操作。

* **[x] Task 1.2: 文档全生命周期治理，支持分布式级联物理删除（Qdrant 向量点、LightRAG 子图、对象存储源文件）及无感重新索引（Reindex）**
  * 现状：级联删除已交付（44b9284），覆盖 Qdrant 分片、图谱缓存失效与对象文件清理；Reindex 已交付：解析产物持久化、REINDEXING 状态、稳定分片 ID 与先写后清理。
  * 范围调整（已确认）：LightRAG 独立持久化图库暂不做，当前以“删除向量 + 失效图缓存”等效子图清理，后续单独立项。

---

## Milestone 2: 多模态与外部数据源连接器（v1.2.0）

* **[ ] Task 2.1: 释放 MinerU 图文多模态解析能力，在 RAG 问答中支持图文联合检索、图表缩略图渲染与点击放大**
  * 解析层保留 MinerU 输出的图片与版面信息，切块时建立图-文关联；检索结果返回图表资源引用，问答链路注入图片摘要；前端支持缩略图展示与点击放大。

* **[ ] Task 2.2: 内置统一 Connector 框架与开箱即用连接器，支持 web_crawler、s3_bucket 及 db_query 的增量同步与去重**
  * 抽象统一的 Connector 接口（发现 → 拉取 → 解析 → 入库 → 游标持久化），内置网页抓取、S3 桶与数据库查询三类连接器；以内容哈希与外部主键去重，按游标增量同步，失败可断点续传。

---

## Milestone 3: 高级工作流控制流与人工介入（v1.3.0）

* **[ ] Task 3.1: 升级 DAG 引擎，支持基于上下文的条件分支（condition）节点与并发迭代批处理（loop）节点**
  * 条件节点按上游上下文结果选择分支；loop 节点对集合元素并发执行子图并聚合输出；保持现有拓扑校验、层级并发与失败语义不回退。

* **[x] Task 3.2: 引入工作流人工介入机制（Human-in-the-Loop），支持 human_approval 审批挂起、上下文快照持久化及审批恢复**
  * 审批节点触发后持久化执行上下文并挂起实例；提供查询待办、批准/驳回接口；审批通过后从快照恢复继续执行，驳回则按失败或指定分支收尾。
  * 交付情况：新增 `human_approval` 节点、`MemoryRunStore` 检查点、`GET /v1/workflows/runs`、`POST /v1/workflows/runs/{id}/{approve,resume}`；状态对齐 schema 的 `COMPLETED / FAILED / REJECTED / WAITING_APPROVAL`，重复审批返回 409。同时交付 Hub 侧高风险工具审批（202 + `approval_id`、`/v1/approvals/{id}/decide`）。
  * 边界：检查点与审批记录当前为进程内存储，跨副本/重启恢复由 Task 3.3 承接。

* **[ ] Task 3.3: 将工作流运行、审批与审计状态接入 Postgres，支持多副本恢复**
  * 按 `docs/schema_enterprise_v1_1_to_1_5.sql` 落库 `workflow_executions` / `workflow_approvals` / `audit_logs`，以 `RunStore` / `ApprovalService` 接口替换内存实现；明确同一 run 的副本粘性或乐观锁，保证审批并发与幂等；审计按月分区并支持保留策略。

---

## Milestone 4: 企业级安全合规与大模型网关治理（v1.4.0）

* **[ ] Task 4.1: 实现 PII 敏感信息双向脱敏引擎与结构化合规审计日志中间件，支持安全追溯**
  * 入口对提示词与上传文本做 PII 识别与脱敏，出口对模型回复做还原或再次脱敏；审计日志记录操作者、资源、动作与脱敏命中项，支持按租户与时间检索。
  * 进展：工具调用与审批决策审计已落地（`AuditLog` + `GET /v1/audit`，进程内）；PII 识别/脱敏与 `audit_logs` 持久化待实现。

* **[ ] Task 4.2: 建设多模型路由网关（主备自动故障转移）与租户级用量计量/配额管控（防 429 溢出）**
  * 统一 LLM 调用入口，按模型与租户路由至主/备通道，失败自动切换并熔断；复用并扩展已有计量能力，按租户累计 token 与调用量，超限前置拦截并避免上游 429 溢出。

---

## Milestone 5: 生态集成与对外服务能力（v1.5.0）

* **[ ] Task 5.1: 支持独立 API Key 鉴权体系，并提供兼容 OpenAI 协议的 /v1/openai/chat/completions 对外接入端点**
  * 新增租户级 API Key 的签发、吊销与作用域控制（可绑定知识库），与现有 JWT 鉴权并存；实现 OpenAI Chat Completions 兼容端点，支持流式与非流式响应、引用透传。

* **[ ] Task 5.2: 支持租户级异步事件 Webhook 订阅（HMAC 签名与指数退避重试）以及标准 MCP Server 适配模块**
  * 提供事件订阅管理（任务完成、索引完成、审批结果等），投递时携带 HMAC 签名，失败按指数退避重试并记录投递日志；实现标准 MCP Server，暴露检索、问答与工作流能力供外部 Agent 调用。
  * 进展：MCP Server 已提前落地（Agent Hub `POST /mcp`，`initialize` / `tools/list` / `tools/call`，工具源为 `ToolRegistry`）；Webhook 订阅、HMAC 签名与事件投递仍待实现。
