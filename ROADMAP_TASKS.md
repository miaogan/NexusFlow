# NexusFlow 版本演进与 Agent 自动执行任务清单 (ROADMAP_TASKS)

> **状态：已归档（全部任务已交付）。** 本文件是 v1.0.0 – v1.2.0 的历史执行清单，仅作交付记录；后续演进以 [ROADMAP_V1_1_V1_5.md](ROADMAP_V1_1_V1_5.md) 为准。
>
> **Agent 执行全局指令：** 严格按顺序执行；修改前检索架构与契约；每完成后运行验证命令、更新任务状态（`[x]`）并提交独立 Commit；所有外部依赖保留内存降级路径。

---

## Milestone 1: v1.0.0（生产基线与安全加固）

* **[x] Task 1.1: 验证并修复 Postgres 身份认证存储链路**
  * **目标**：确保 `IDENTITY_DRIVER=postgres` 下自动建表与种子用户初始化幂等，补充集成测试 `backend/internal/identity/postgres_test.go`，对齐 `deploy/docker-compose.yml` 默认配置。
  * **验证命令**：`make vet && go test ./backend/internal/identity/...`
* **[x] Task 1.2: 实现基于 Redis 的 JWT 令牌吊销名单（Token Revocation）**
  * **目标**：设计 `TokenBlacklist` 接口（支持 memory/redis），在 `/v1/auth/logout` 时写入黑名单并设置 TTL，改造网关与鉴权逻辑拦截失效 Token。
  * **验证命令**：`make vet && go test ./backend/internal/gateway/... ./backend/internal/identity/...`
* **[x] Task 1.3: 生产级 Docker Compose 配置与健康检查收敛**
  * **目标**：对齐 `.env.example` 环境变量，为 `deploy/docker-compose.yml` 所有业务容器添加 `/healthz` 健康检查与依赖关系。
  * **验证命令**：`docker compose -f deploy/docker-compose.yml config -q && echo "Compose config valid"`

### 真机联调验收（2026-11-27）

* **[x] Postgres 身份链路**：`identity_users` / `identity_roles` 自动建表、内置角色、种子管理员登录及 Gateway 身份读取均通过。
* **[x] Redis 跨进程吊销**：登出写入带 TTL 的 `jti`，Gateway 拒绝已吊销 Token，新登录不受影响。
* **[x] MinerU 4.2.3 + Qdrant 闭环**：真实 PDF 经 GPU standard 解析、Markdown 解包、LlamaIndex 切块与 Embedding、Qdrant 入库、内容标记检索及 Gateway 混合检索均通过。
* **验收命令**：`python scripts/smoke_test.py --require-mineru`
* **验收结果**：29 passed / 0 failed / 0 skipped；MinerU V1 协议单测、Compose 配置校验通过。

---

## Milestone 2: v1.1.0（反馈数据闭环与自动化评测）

* **[x] Task 2.1: 实现负反馈在线检索降权（Online Down-ranking）**
  * **目标**：在检索重排流程中引入 `FeedbackPenalty` 阶段，对累计负反馈超阈值的 Chunk 乘以惩罚系数。
  * **验证命令**：`make vet && go test ./backend/internal/query/...`
* **[x] Task 2.2: 打通纠正答案（Corrected Answer）灰度回灌能力**
  * **目标**：在 `rag-agent` 检索前置步骤中匹配高优 `corrected_answer` 历史记录并注入 Prompt。
  * **验证命令**：`cd services/rag-agent && pytest`
* **[x] Task 2.3: 配置 GitHub Actions CI 自动化测试与离线评测卡点**
  * **目标**：新建 `.github/workflows/ci.yml`，配置前后端构建、单元测试及离线 RAG 评测工作流。
  * **验证命令**：`make test && make web-build`

---

## Milestone 3: v1.2.0（高阶检索能力扩展）

* **[x] Task 3.1: 实现 LightRAG 图谱检索路由（Graph Route）**
  * **目标**：在 `services/llamaindex` 补充图谱抽取/查询接口（无 LLM 时自动降级为共现词图谱），并在 Go 侧注册 `graph` 召回路由接入 RRF 融合。
  * **验证命令**：`make vet && go test ./backend/internal/query/...`
* **[x] Task 3.2: 实现 NL2SQL 结构化查询工作流节点**
  * **目标**：在 `backend/internal/workflow/` 注册 `nl2sql` 节点，内置 SQL 安全护栏（仅允许 SELECT 只读语句）。
  * **验证命令**：`make vet && go test ./backend/internal/workflow/...`
