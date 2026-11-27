# NexusFlow Kubernetes 部署指南

面向生产集群的部署说明，配套清单：

| 文件 | 用途 |
| --- | --- |
| `deploy/k8s/nexusflow.yaml` | 命名空间、配置、Secret、PVC、工作负载、Service 与 HPA |
| `deploy/k8s/prometheus-adapter-values.yaml` | prometheus-adapter 自定义指标映射（HPA 依赖） |

## 1. 前置条件

- Kubernetes 1.25+，已安装 Metrics Server（HPA 的 CPU/内存指标来源）。
- 可用的 **ReadWriteMany** StorageClass（NFS / CephFS / EFS 等）。清单默认 `nfs-client`，请按集群实际修改两个 PVC 的 `storageClassName`。
- 外部依赖已就绪并可通过 DNS 访问：Redis、RabbitMQ、MinIO（或 S3 兼容存储）、Qdrant、MinerU API、OpenSandbox。
- 已构建并推送 `gateway` / `ingestion` / `processing-worker` / `sandbox-execution` / `query-service` / `workflow-engine` / `auth-service` / `user-service` / `llamaindex` / `rag-agent` / `web` 镜像，并按需替换清单中的 `image` 标签。

## 2. 配置

1. 修改 ConfigMap `app-config`：
   - `REDIS_ADDR`、`QUEUE_URL`、`OBJECT_STORE_BASE`、`OPENSANDBOX_URL` 指向实际服务。
   - `IDENTITY_DRIVER=redis`、`METERING_DRIVER=redis`：身份与计量在副本间共享，必须使用同一 Redis。
   - 若改用 `IDENTITY_DRIVER=postgres`（`IDENTITY_DATABASE_URL`），服务启动时会在 advisory lock 下自动创建 `identity_users` / `identity_roles` / `knowledge_bases` / `kb_permissions`，并把旧 `identity_knowledge_bases` 幂等迁入；多副本并发启动安全。表结构与 `docs/schema_enterprise_v1_1_to_1_5.sql` 对齐。
   - `LLAMAINDEX_URL`：开启切块/索引与删除级联清理；未配置时删除只清理对象与任务记录。
   - `LLAMAINDEX_SPARSE_ENABLED=true`：`bm25` 路由使用 Qdrant 原生 sparse（每 KB 伴生集合 `<collection>__sparse`，IDF 权重）；关闭、集合缺失或调用失败时回退到 scroll + BM25。
   - 若独立部署 `memory-agent`：`MEMORY_DRIVER=qdrant`、`MEMORY_DATABASE_URL`、`MEMORY_QDRANT_URL`、`MEMORY_QDRANT_COLLECTION`；正文写 Postgres，向量写 Qdrant，历史 pgvector 数据用 `python -m app.backfill` 回填。
   - `QUOTA_MAX_STORAGE_BYTES` / `QUOTA_MAX_FILES` / `QUOTA_MAX_QUERIES`：租户配额，`0` 表示不限。
2. 修改 Secret `app-secret`：`JWT_SECRET`、`OPENSANDBOX_TOKEN`、`LLAMAINDEX_EMBEDDING_API_KEY`、`RAG_LLM_BASE_URL`、`RAG_LLM_API_KEY`。生产环境建议改用 SealedSecret / ExternalSecret 管理。

## 3. 存储

清单创建两个 PVC：

| PVC | 挂载点 | 使用方 | 说明 |
| --- | --- | --- | --- |
| `object-store` | `/data/objects` | gateway / ingestion / processing-worker | 上传落地、级联删除、解析回读与重建索引需共享同一份数据；解析产物落盘为 `tenant/kb/parsed/<task>.md` |
| `sandbox-work` | `/data/sandboxes` | sandbox-execution | 沙箱工作目录 |

```bash
kubectl -n nexusflow get pvc -w   # 等待 STATUS=Bound
```

> 若 PVC 长期 `Pending`，通常是集群没有匹配 `accessModes: ReadWriteMany` 的 StorageClass。请安装 NFS/CephFS provisioner 或改用支持共享读写的 CSI。

## 4. 部署

```bash
kubectl apply -f deploy/k8s/nexusflow.yaml
kubectl -n nexusflow rollout status deploy/gateway
kubectl -n nexusflow get pods,svc
```

访问入口：`gateway` Service（LoadBalancer，8080）与控制台 `web`（LoadBalancer，80）。首次登录使用 `admin / admin123`，上线前务必修改 `SEED_ADMIN_PASSWORD`。

## 5. HPA 与自定义指标

`processing-worker` 的 HPA 按队列积压扩缩容（平均每 Pod 50 条待处理消息），指标 `nexusflow_queue_length` 由 Prometheus 采集、prometheus-adapter 转换后暴露给 Custom Metrics API：

```bash
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm upgrade --install prometheus-adapter prometheus-community/prometheus-adapter \
  -n monitoring --create-namespace -f deploy/k8s/prometheus-adapter-values.yaml
```

验证：

```bash
kubectl get --raw /apis/custom.metrics.k8s.io/v1beta1 | grep nexusflow
kubectl -n nexusflow get hpa
kubectl -n nexusflow describe hpa processing-worker
```

清单中的 HPA：

| HPA | 指标 | 目标 | 范围 |
| --- | --- | --- | --- |
| `processing-worker` | Pods `nexusflow_queue_length` | 平均 50 条 | 2–12 |
| `gateway` | CPU 利用率 | 70% | 2–8 |
| `query-service` | CPU 利用率 | 65% | 2–10 |

`prometheus-adapter-values.yaml` 同时提供 `nexusflow_processing_rps` 映射（`nexusflow_processing_latency_seconds_count` 的 5 分钟速率），需要按吞吐扩缩容时可将 `processing-worker` 的 HPA 指标替换为该值。

若 `kubectl get hpa` 显示 `unknown`，依次检查：

1. Prometheus 是否抓取到目标 Pod（`prometheus.io/scrape` 注解、`METRICS_ADDR=:9100`）。
2. Prometheus 中的原始序列是否存在：`nexusflow_queue_length{service="processing-worker"}`。
3. adapter 是否就绪：`kubectl -n monitoring logs deploy/prometheus-adapter`。

## 6. 容量与配额

`GET /v1/usage`（需 `usage:read`）返回租户存储、文件数、检索/问答次数、按知识库拆分以及配额余量；控制台概览页直接展示三张用量卡片。上传或检索超过配额时 Gateway 返回 `429` 并携带当前用量。

计量与身份一样依赖 Redis，多副本部署时请勿使用内存驱动；Redis 不可用时计量记录会退化为 fail-open（不阻断业务），但配额拦截将失效。

## 7. 删除与数据一致性

`DELETE /v1/files/{task_id}?kb_id=...` 执行级联删除：

1. 调用 LlamaIndex `/v1/delete` 删除该文档向量并失效图谱缓存；
2. 删除对象存储中的文件；
3. 删除任务记录；
4. 回退租户存储计量。

索引清理失败时接口返回 `503` 且保留任务记录，便于重试，避免「任务已删但向量残留」。未配置 `LLAMAINDEX_URL` 时跳过第 1 步。

### 7.1 重建索引（Reindex）

`POST /v1/files/{task_id}/reindex?kb_id=...` 在不重新解析的前提下原地重建该文档索引，鉴权为 `task:write` 且当前知识库角色不低于 `kb:editor`。

**前提**：任务处于 `COMPLETED`，且 metadata 中存在 `markdown_key`——解析完成后 Markdown 会持久化为 `tenant/kb/parsed/<task>.md`，因此 `processing-worker` 与 gateway 必须挂载同一 `object-store` PVC。升级前上传的历史文档没有该产物，重建会返回 `400`，需重新上传后再重建（重新上传会补齐 `markdown_key`）。

**执行过程**：

1. 任务状态切到 `REINDEXING`，从对象存储读取已持久化的 Markdown；
2. 调用 LlamaIndex `/v1/index` 重新切块、Embedding，分片 ID 使用 `task_id + 序号 + 内容摘要` 的确定性 UUIDv5；
3. 新向量点全部写入成功后再删除旧点，检索侧无空窗；
4. 成功后回到 `COMPLETED`。

失败时状态回滚为 `COMPLETED`，旧索引保持可用，可直接重试，无需重新上传。级联删除会同时清理 `parsed/` 下的解析产物，请勿单独清理该目录，否则重建将不可用。

重建会同时刷新稠密索引与 sparse 伴生集合。启用 `LLAMAINDEX_SPARSE_ENABLED` 后，建议对升级前的历史文档执行一次 Reindex 补齐 sparse 向量；未补齐时 `bm25` 在 sparse 命中不足 `top_k` 时会混合旧 BM25 兜底，但旧文档的纯词法召回会弱于已补齐文档。

## 8. 沙箱后端（OpenSandbox）

`sandbox-execution` 支持两种后端：

| `SANDBOX_BACKEND` | 说明 |
| --- | --- |
| `local` | 进程内临时目录执行，仅用于开发，**不是安全边界** |
| `remote` | 通过官方 OpenSandbox Go SDK 调用 server，生产必须使用 |

### 8.1 运行形态与 Runtime 后端

OpenSandbox 不是一个容器，而是三层：

| 层 | 职责 |
| --- | --- |
| Lifecycle Server | `/v1` 控制面：鉴权、持久化、调度与销毁 |
| Runtime 后端 | `docker` 或 `kubernetes`，决定沙箱在哪里运行 |
| 沙箱内 execd | server 启动沙箱时注入的守护进程，负责命令执行（SSE）、文件读写与指标 |

**Docker 模式（DOOD）**：runtime 为 `docker` 时，server 必须能访问 Docker daemon。server 以容器运行时只能挂载宿主 socket：

```bash
-v /var/run/docker.sock:/var/run/docker.sock
```

此时沙箱由宿主 daemon 创建，是与 server 平级的 sibling 容器；server 容器内并没有再启动 dockerd，因此属于 **DOOD（Docker-outside-of-Docker）而非 DinD**。server 不需要 `privileged`，但它因此持有宿主 Docker 的控制权，安全权重等同于该 daemon。

**Kubernetes 模式**：server 通过 ServiceAccount 访问 apiserver，创建 `BatchSandbox` 等 CRD，由 controller 拉起沙箱 Pod；execd 通过 init container 注入，数据面流量经 ingress gateway，全程不需要 `docker.sock`。生产环境推荐该模式。

无论哪种模式，`sandbox-execution` 本身都只是 SDK 客户端，不挂 socket、不需要特权；需要 Docker 权限的只有 server。

### 8.2 客户端接入

remote 后端完全基于官方 SDK（`github.com/alibaba/OpenSandbox/sdks/sandbox/go`）实现：创建并等待沙箱就绪 → 通过 execd 上传代码/输入 → SSE 执行命令 → 下载输出 → 强制删除。鉴权使用 `OPEN-SANDBOX-API-KEY` 请求头（由 SDK 从 `OPENSANDBOX_TOKEN` 注入）。

```bash
SANDBOX_BACKEND=remote
OPENSANDBOX_URL=http://opensandbox.nexusflow.svc:8080   # 可带 /v1，SDK 会自行拼接
OPENSANDBOX_TOKEN=<server 的 api_key>
# 仅当运行时传裸 "python"/"node" 时生效；"python:3.11" 这类带版本的运行时直接作为镜像
OPENSANDBOX_PYTHON_IMAGE=python:3.12
OPENSANDBOX_NODE_IMAGE=node:18
```

注意事项：

- 运行时镜像必须包含 shell（server 需要注入 execd），并预装业务依赖；`packages` 参数不会在创建时安装。
- `egress_allow` 非空时转换为 deny-default 出网策略，空值沿用 server 默认。
- 每次执行都会创建并销毁独立沙箱，TTL 由请求超时强制约束。
- 可选的 `OPENSANDBOX_PYTHON_IMAGE` / `OPENSANDBOX_NODE_IMAGE` 用于指向内网镜像仓库。

### 8.3 数据面网络

execd 端口通常以容器 IP 或宿主随机端口的形式返回，跨主机或其他容器不一定可达。按环境选择访问方式：

| 场景 | 方式 |
| --- | --- |
| 本机 Docker / 跨网络调试 | SDK 开启 `UseServerProxy`，execd 请求经 server 反代，客户端只连 server |
| 生产 Kubernetes | ingress gateway 暴露沙箱端口，SDK 直连 gateway |
| 直连 endpoint | 依赖 `publish_host` 与端口范围可达，需保证客户端与 daemon 同网段 |

Docker 模式每沙箱需 2–3 个宿主端口（含 egress sidecar 时为 3 个），`port_range_min` / `port_range_max` 要按并发规模预留。

### 8.4 安全边界

- 必须为 server 设置 `api_key` 并与 `OPENSANDBOX_TOKEN` 一致；验证时错误 key 返回 `401`。
- 挂载 `docker.sock` 的 server 不要暴露到公网，优先部署在专用节点或改用 Kubernetes 模式。
- 通过私有仓库或节点级镜像白名单限制可拉取的运行时镜像，避免任意镜像进入沙箱。
- 出网默认拒绝、按需放行；每次执行强制 TTL 并无条件删除，集成测试会校验沙箱无残留。
- `local` 后端与宿主共享命名空间，只允许在开发环境启用。

### 8.5 本地端到端验证

用 Docker 模式启动 server（需要挂载 docker.sock，并使用已设置 `api_key` 的配置）：

```bash
mkdir -p /tmp/opensandbox
cp deploy/opensandbox/config.example.toml /tmp/opensandbox/config.toml   # 修改 api_key
docker run -d --name opensandbox-server -p 28080:8080 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /tmp/opensandbox:/etc/opensandbox:ro \
  sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/server:latest

OPENSANDBOX_TEST_URL=http://127.0.0.1:28080 \
OPENSANDBOX_TEST_TOKEN=<api_key> \
OPENSANDBOX_TEST_IMAGE=opensandbox/code-interpreter:latest \
  go test ./backend/internal/sandbox -run Remote -v
```

集成测试会走完整链路：创建沙箱 → 上传代码与输入 → execd 执行 → 回读 `/tmp/output.json` → 删除，并校验沙箱没有残留。

## 9. 回滚与升级

- 滚动升级：逐个更新 `image` 标签后 `kubectl apply`，Deployment 默认使用 RollingUpdate。
- 数据库/模式：身份表（含 `knowledge_bases` / `kb_permissions` 与旧表迁移）在服务启动时以 advisory lock 自动迁移；多副本并发启动是安全的。
- 从 v1.1.0 之前升级：历史任务没有 `markdown_key`，重建索引会返回 `400`，需重新上传文件；既有向量与检索不受影响。
- 长期记忆存储迁移：先设置 `MEMORY_DRIVER=qdrant` + `MEMORY_QDRANT_URL` 并运行 `python -m app.backfill` 回填旧 pgvector 向量，再切换流量；回滚 `MEMORY_DRIVER=postgres` 后，qdrant 模式期间新增的记忆需要重新抽取才能恢复语义召回。
- 回滚：`kubectl -n nexusflow rollout undo deploy/<name>`。PVC 中的数据不会随回滚删除。
