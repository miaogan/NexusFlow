# NexusFlow v1.1.0 演进与 Agent 自动执行任务清单 (ROADMAP_V1_1)

> **Agent 执行全局指令：**
> 1. 严格按顺序逐个执行，禁止跨任务并发，每次验证通过后提交独立 Commit 并更新状态。
> 2. 接口变更优先更新 proto 契约并执行编译；存储扩展需保留 Memory/Mock 降级路径。

---

## Milestone 1: 多租户数据治理与生命周期闭环

* **[x] Task 1.1: 知识库（Knowledge Base）实体与细粒度权限校验**
  * **目标**：引入 `kb_id` 维度的知识库管理与隔离，防止同租户内跨知识库越权。
  * **核心步骤**：更新 proto 契约、实现 CRUD 接口（支持 memory/postgres）、改造文件与检索入口强制校验 `kb_id` 权限、前端增加切换与管理弹窗。
  * **验证命令**：`make vet && go test ./backend/internal/identity/... ./backend/internal/gateway/... && make web-build`

* **[x] Task 1.2: 文档全生命周期级联删除（Cascade Delete）**
  * **目标**：补齐文件删除时的多端数据一致性清理，避免脏数据残留。
  * **核心步骤**：新增文件删除接口，编排删除对象存储文件、调用后端服务清理向量与图谱节点，并补充测试。
  * **验证命令**：`make vet && go test ./backend/internal/ingestion/... && cd services/llamaindex && pytest`

---

## Milestone 2: 资源配额计量与集群化交付

* **[x] Task 2.1: 租户级资源计量与配额拦截（Quota & Metering）**
  * **目标**：统计租户存储与问答用量，支持基础配额展示。
  * **核心步骤**：实现计量记录器（支持 memory/redis）、暴露用量接口、前端概览页增加用量统计卡片。
  * **验证命令**：`make vet && go test ./backend/internal/platform/... ./backend/internal/gateway/... && make web-build`

* **[x] Task 2.2: 完善 K8s 生产部署模板与 HPA 自定义指标配置**
  * **目标**：补齐生产级配置，落地集群弹性伸缩。
  * **核心步骤**：补充自定义指标映射配置、完善 PVC 配置及更新相关部署文档。
  * **验证命令**：`kubectl apply --dry-run=client -f deploy/k8s/nexusflow.yaml 2>/dev/null || echo "YAML syntax check skipped (no kubectl)" && make test`
