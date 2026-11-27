# 多 Agent 单容器 + 动态星型调度中枢

## Context

当前项目把 RAG 编排与三个协作 Agent 拆成 4 个独立 Python 服务：`rag-agent(:8001)`、`intent-agent(:8002)`、`summary-agent(:8003)`、`memory-agent(:8004)`。它们都以顶层包 `app` 打包，无法在同一镜像内共存；K8s 只部署了 `rag-agent`，其余三个不可达，实际全部走降级；同时 `ChatService` 里的调用顺序与 URL 是写死的，新增/替换 Agent 必须改代码和部署。

本次改造目标：

1. **默认单容器**：一个 `agent-hub` 镜像同时提供 RAG 对话与 intent / summary / memory 能力，对外仍是 `8001`。
2. **动态星型调度**：中心 Hub 通过注册表按 capability 选择 Agent，支持本地进程内执行与远程 HTTP 执行、优先级、超时、健康摘除、故障转移与运行时注册。
3. **兼容拆分部署**：同一镜像可用 `ROLE` 启动为独立 worker，保留 `remote` / `hybrid` 模式作为回滚与横向扩展路径。
4. **契约不变**：Gateway、Web SSE、`/v1/chat`、`/v1/chat/stream`、`/v1/conversations`、`/v1/intent`、`/v1/summarize`、`/v1/memories/*` 的请求/响应保持不变；Postgres 事实源、Qdrant 向量存储的职责不变。

## 目标拓扑

```text
默认（单容器）：
Gateway → agent-hub:8001 (ROLE=all, MODE=local)
              ├─ RAG chat / SSE
              ├─ intent agent (in-process)
              ├─ summary agent (in-process)
              └─ memory agent (in-process → Postgres 正文 / Qdrant 向量)

拆分/回滚（MODE=remote|hybrid）：
Gateway → agent-hub:8001 (ROLE=rag)
              ├─ agent-hub:8002 (ROLE=intent)
              ├─ agent-hub:8003 (ROLE=summary)
              └─ agent-hub:8004 (ROLE=memory)
```

## 目录与包边界

新建 `services/agent-hub`，单一发行包 `nexusflow_agents`，通过 `git mv` 平迁现有代码，只改导入路径，不重写业务逻辑：

```text
services/agent-hub/
  pyproject.toml                 # extras: base / memory / dev
  Dockerfile                     # uvicorn nexusflow_agents.main:app
  src/nexusflow_agents/
    main.py                      # 按 ROLE 装配 FastAPI app
    core/
      config.py                  # RAG_ / INTENT_ / SUMMARY_ / MEMORY_ / AGENT_HUB_ 配置
      registry.py                # AgentSpec / AgentRegistry
      executor.py                # LocalExecutor / HttpExecutor
      hub.py                     # Hub：能力路由、失败转移、指标
      lifecycle.py               # 后台健康检查、TTL 清理、后台任务池
      metrics.py                 # hub 指标注册
    agents/
      rag/                       # 原 rag-agent 业务代码
      intent/                    # 原 intent-agent 业务代码
      summary/                   # 原 summary-agent 业务代码
      memory/                    # 原 memory-agent 业务代码
    api/
      chat.py                    # /v1/chat, /v1/chat/stream, /v1/conversations
      agents.py                  # /v1/intent, /v1/summarize, /v1/memories/*
      admin.py                   # /v1/agents 注册/注销/列表
  tests/                         # 4 套原测试 + hub/registry/contract/sse 测试
```

`agents/rag` 保留 `chat.py`、`graph.py`、`store.py`、`services/{intent,summary,memory,corrected,retrieval,llm}.py`；三个外部 HTTP client 改为 Hub 适配器，原有 Protocol 不变，现有 Fake 测试可复用。

## 核心设计

### 1. Agent 注册表

```python
@dataclass(frozen=True)
class AgentSpec:
    name: str
    capabilities: set[str]        # intent.classify / summary.compact / memory.recall / memory.extract
    kind: Literal["local", "http"]
    endpoint: str = ""
    timeout_ms: int = 5000
    priority: int = 100           # 数字越小越优先
    ttl_s: int = 0                # 0 = 不过期（本地/静态）
    fallback: str = ""            # 可选能力降级策略
```

`AgentRegistry`：`register` / `deregister` / `heartbeat` / `prune_expired` / `resolve(capability)`。`resolve` 只返回启用且健康的 spec，按 `priority` 排序。

### 2. 执行器与 Hub

```python
class Executor(Protocol):
    def call(spec: AgentSpec, capability: str, payload: dict, ctx: dict) -> dict: ...
    async def astream(spec, capability, payload, ctx) -> AsyncIterator[dict]: ...
```

- `LocalExecutor`：直接调用进程内 agent 函数。
- `HttpExecutor`：httpx 调用远程 worker，沿用现有 `/v1/intent`、`/v1/summarize`、`/v1/memories/*`。
- `Hub.call(capability, payload, ctx, required=False)`：按优先级逐个尝试，失败/超时切下一个；可选能力全部失败时返回 `degraded=True` 的默认值，绝不抛错。
- `Hub.astream`：本期只保留接口，token 仍由本地 LLM 流式生成，不进 Hub。
- 结果结构：`AgentResult{ok, data, error, agent, latency_ms, degraded, attempts}`。

### 3. ChatService 接入

保留 `IntentClassifier`、`ConversationSummarizer`、`ConversationMemory` 三个 Protocol，新增：

- `HubIntentClassifier` → `hub.call("intent.classify", ...)`
- `HubSummarizer` → `hub.call("summary.compact", ...)`
- `HubMemory` → `hub.call("memory.recall", ...)` / 后台池提交 `memory.extract`

`ChatService` 的串行流程、SSE 事件（`stage|intent|sources|token|done|error`）、错误体保持一致。`_remember` 从裸 daemon 线程改为 Hub 有界后台池，队列满时丢弃并计数。

降级语义收敛到 `AgentSpec`：

| 能力 | 失败默认值 |
| --- | --- |
| `intent.classify` | `fallback_intent()`：knowledge_qa + need_retrieval |
| `summary.compact` | `None`：保留原文窗口 |
| `memory.recall` | `[]`：无长期记忆 |
| `memory.extract` | 丢弃并记日志/指标 |

### 4. 动态注册与健康

- 启动时按 `ROLE` 注册本地 Agent；`MODE=remote|hybrid` 时把 `INTENT_URL` / `SUMMARY_URL` / `MEMORY_URL` 注册为 HTTP spec。
- `GET /v1/agents` 列出注册项；`POST /v1/agents` / `DELETE /v1/agents/{name}` 动态注册/摘除。
- 注册接口默认关闭；开启需 `AGENT_HUB_ADMIN_TOKEN`，校验 name 正则、capability 白名单与条目上限。
- 后台线程每 `AGENT_HUB_HEALTH_INTERVAL_S` 探测 HTTP Agent；连续 `FAIL_THRESHOLD` 次失败标记 unhealthy；TTL 到期的动态条目自动清理。
- `/healthz` 返回 `status`、`role`、`mode`、各 Agent 状态；新增 `/readyz` 供 K8s readiness；`/metrics` 统一暴露。

## 配置

保留现有 `RAG_*` / `INTENT_*` / `SUMMARY_*` / `MEMORY_*`，新增：

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `AGENT_HUB_ROLE` | `all` | `all|rag|intent|summary|memory` |
| `AGENT_HUB_MODE` | `local` | `local|remote|hybrid` |
| `AGENT_HUB_PREFER` | `local` | hybrid 下优先本地或远程 |
| `AGENT_HUB_PORT` | `8001` | 单容器端口 |
| `AGENT_HUB_HEALTH_INTERVAL_S` | `10` | 健康探测间隔 |
| `AGENT_HUB_HEALTH_TIMEOUT_S` | `2` | 单次探测超时 |
| `AGENT_HUB_REGISTRY_TTL_S` | `30` | 动态注册 TTL |
| `AGENT_HUB_FAIL_THRESHOLD` | `3` | 连续失败摘除阈值 |
| `AGENT_HUB_MAX_ATTEMPTS` | `2` | 单能力最大尝试数 |
| `AGENT_HUB_DYNAMIC_REGISTRATION` | `false` | 是否开放运行时注册 |
| `AGENT_HUB_ADMIN_TOKEN` | 空 | 注册接口令牌 |
| `AGENT_HUB_BACKGROUND_WORKERS` | `8` | 记忆抽取后台池 |

`AGENT_HUB_MODE=remote` + split profile 等价于当前四服务拓扑，是回滚开关。

## 分阶段实施

### Phase 0：基线

- 记录现有 4 服务测试结果、SSE 事件 golden 样例。
- 不改代码。

### Phase 1：迁包

- 新建 `services/agent-hub`，`git mv` 四个服务的 `app` 代码到 `src/nexusflow_agents/agents/*`。
- 全量替换 `from app.` 为 `from nexusflow_agents.`；迁移测试。
- 验证：`python -m pytest -q`、`python -c "import nexusflow_agents"`。

### Phase 2：Hub 内核

- 实现 `AgentSpec`、`AgentRegistry`、`LocalExecutor`、`HttpExecutor`、`Hub`、指标与后台生命周期。
- 单测覆盖优先级、超时、failover、TTL、健康摘除。
- 验证：`python -m pytest tests/test_registry.py tests/test_hub.py -q`。

### Phase 3：单应用与 ROLE

- `main.py` 按 `ROLE` 装配路由；默认 `ROLE=all` 暴露全部接口。
- `/healthz`、`/readyz`、`/metrics` 统一。
- 验证：curl 四个内部能力端点与 health/metrics。

### Phase 4：ChatService 接入 Hub

- 新增三个 Hub 适配器，替换 `deps.py` 中的 `Http*` 客户端。
- 保持 SSE 事件与降级语义；迁移并扩充测试。
- 验证：`pytest`、golden SSE 对比、无 LLM 时抽取式回答仍可用。

### Phase 5：部署与文档

- Compose：新增 `agent-hub`（8001，`ROLE=all`、`MODE=local`）；原三服务改为 `profiles:["split"]`，同镜像 + `ROLE` + 8002/8003/8004。
- K8s：Deployment 改为 `agent-hub`，保留 `rag-agent` Service 别名指向同一 selector；补 `MEMORY_*` / `AGENT_HUB_*` 环境变量。
- CI/Makefile：`rag-agent` job 改为 `agent-hub`，`pip install -e ".[dev]"`，补 docker build。
- 文档：README、架构、部署、`.env.example` 增加单容器与星型调度说明。
- 验证：`docker compose config -q`、`--profile split config -q`、`kubectl apply --dry-run=client -f`。

### Phase 6：清理

- 删除旧四个服务目录与旧 CI job（git 历史保留）。
- 全量回归 + 真机 compose smoke。

## 关键文件

- 新增：`services/agent-hub/{pyproject.toml,Dockerfile,src/nexusflow_agents/**}`、`tests/test_{registry,hub,contracts,sse_parity}.py`
- 迁移：`services/{rag,intent,summary,memory}-agent/app/**` → `services/agent-hub/src/nexusflow_agents/agents/**`
- 改造：`services/rag-agent/app/services/chat.py`、`app/deps.py`、`app/core/config.py`（迁入后路径）
- 部署：`deploy/docker-compose.yml`、`deploy/k8s/nexusflow.yaml`、`.env.example`、`Makefile`、`.github/workflows/ci.yml`
- 文档：`README.md`、`docs/architecture.md`、`docs/deployment.md`

## 风险与回滚

- **导入遗漏**：`rg "from app\."` 全量审计 + pytest；单提交可 revert。
- **镜像膨胀/启动慢**：`memory` 依赖放 extras 并惰性导入；默认 role 可按需装配。
- **单点故障**：K8s replicas ≥ 2 + readiness；`MODE=remote` + split profile 回到旧拓扑。
- **事件循环阻塞**：Hub 同步调用只允许在线程池内执行，测试中断言。
- **动态注册滥用**：默认关闭、令牌、白名单、条目上限。
- **存储职责不回退**：Postgres 正文 / Qdrant 向量保持不变，无 schema 变更。

## 验证

1. `cd services/agent-hub && python -m pytest -q`
2. `python -m ruff check src tests`
3. `docker compose -f deploy/docker-compose.yml config -q`
4. `docker compose -f deploy/docker-compose.yml --profile split config -q`
5. `docker compose -f deploy/docker-compose.yml up -d --build agent-hub gateway`，经 Gateway 验证 `/v1/chat` 与 `/v1/chat/stream`
6. `kubectl apply --dry-run=client -f deploy/k8s/nexusflow.yaml`
7. 关闭一个 split worker，验证 Hub 故障转移与 `degraded` 事件。
