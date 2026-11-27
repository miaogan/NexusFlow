---
title: 分布式文件向量化工作流 · 系统架构设计文档
version: V4.0
type: 技术架构设计
classification: 内部
stack: Go 调度 + MinerU 解析 + LlamaIndex 数据加工 + OpenSandbox 执行
---

# 分布式文件向量化工作流

**系统架构设计文档（V4.0）**

> 基于 Go 调度 + MinerU 解析 + LlamaIndex 数据加工 + OpenSandbox 执行
>
> 版本 V4.0 ｜ 文档类型 技术架构设计 ｜ 密级 内部

## 目录

- [1. 项目概述](#1-项目概述)
- [2. 总体架构](#2-总体架构)
- [3. 核心微服务拆分](#3-核心微服务拆分)
- [4. LlamaIndex 数据处理服务设计](#4-llamaindex-数据处理服务设计)
- [5. 核心数据流转与状态机](#5-核心数据流转与状态机)
- [6. 关键组件设计](#6-关键组件设计)
- [7. 动态节点弹性伸缩](#7-动态节点弹性伸缩)
- [8. 自定义节点与 DAG 编排引擎](#8-自定义节点与-dag-编排引擎)
- [9. OpenSandbox 沙箱集成方案](#9-opensandbox-沙箱集成方案)
- [10. 接口定义（Protocol Buffers）](#10-接口定义protocol-buffers)
- [11. 沙箱执行服务核心代码框架（Go）](#11-沙箱执行服务核心代码框架go)
- [12. 工程落地与可靠性建议](#12-工程落地与可靠性建议)
- [13. 查询链路设计（多路召回与 Rerank）](#13-查询链路设计多路召回与-rerank)
- [14. 反馈闭环（Feedback Loop）设计](#14-反馈闭环feedback-loop设计)
- [15. 多租户与权限治理](#15-多租户与权限治理)
- [16. LightRAG 图增强检索](#16-lightrag-图增强检索)
- [17. NL2SQL 结构化问答引擎](#17-nl2sql-结构化问答引擎)
- [18. 多租户长期记忆层](#18-多租户长期记忆层)
- [19. Langfuse 可观测性与治理](#19-langfuse-可观测性与治理)
- [20. 技术栈选型与关键取舍](#20-技术栈选型与关键取舍)

---

## 1. 项目概述

本项目旨在构建一个高并发、可水平扩展、支持自定义编排的分布式文件向量化数据管道，用于将海量异构文件（PDF、Word、TXT 等）转化为可检索的向量数据，服务于下游 RAG 与语义检索场景。V2.0 在 V1.0 的基础上完成了关键重构：将原本拟由 Go 侧承担的文本切块、索引构建与 Embedding 调用逻辑，整体下沉为一个独立的 LlamaIndex 数据处理微服务，形成“Go 编排调度 + Python 专业加工”的混合架构，从而在保住 Go 高并发调度优势的同时，充分复用 LlamaIndex 生态在切块、索引与检索策略上的成熟能力。

V2.0 技术选型的核心考量如下：

- Go 语言：编译为单一二进制，云原生部署友好；Goroutine 与 Channel 天然契合生产者-消费者模型，单机可支撑数万并发。V2.0 中 Go 专注于任务流转、DAG 编排、外部引擎调用与状态管理，不再承载复杂的数据加工逻辑。
- MinerU：作为专业文档解析引擎，弥补 Go 生态在复杂 PDF / Word 深度解析上的短板，输出结构化 Markdown / JSON，作为 LlamaIndex 切块的高质量标准输入。
- LlamaIndex（Python 微服务）：承担语义切块、层级切块、多路索引构建与 Embedding 编排，替代 Go 侧简单切块，显著提升 RAG 检索精度。它是 V2.0 新增的数据加工核心。
- 外部 Embedding API：向量化推理外置，由 LlamaIndex 内部以 Client 方式调用，系统自身无需维护沉重的模型推理服务。
- OpenSandbox：为企业级 AI 代码执行提供安全隔离沙箱，是系统支持“自定义代码节点”的基础设施，使平台从固定管道升级为可编排的工作流平台。

特别说明：LlamaIndex 官方仅提供 Python 与 TypeScript 版本 SDK，不存在官方 Go 原生实现。因此本架构将其作为独立服务通过 gRPC/HTTP 接入，而非嵌入 Go 进程，这是保证能力上限与生态兼容性的正确工程决策。

## 2. 总体架构

系统整体采用微服务 + 消息驱动的分层结构，自上而下划分为接入层、编排调度层、执行加工层、检索问答层与存储层。各阶段以消息队列解耦，实现故障隔离与独立弹性伸缩。V2.0 的关键变化在于执行加工层引入了以 Python 实现的 LlamaIndex 服务，与 Go 服务通过 gRPC 协同；后续版本又补齐了独立鉴权、租户计量、检索问答与文档重建链路。

```text
+----------------------------------------------------------------------------------+
| 接入层                                                                           |
|  客户端/画布 --HTTP/gRPC--> API Gateway（JWT 校验 · 限流 · 权限路由 · trace 透传）   |
|    ├─ Auth/User（JWT 签发 · 用户/角色 · kb RBAC） --> Redis / PostgreSQL            |
|    └─ Metering（租户用量与配额）                    --> Redis                       |
+----------------------------------------------------------------------------------+
        | 上传 / 删除 / Reindex      | DAG 调度                | 检索 / 问答
        v                            v                         v
+----------------------------------------------------------------------------------+
| 编排调度层                                                                       |
|  Ingestion（元数据 · 任务 · 级联删除 · Reindex）        Workflow Engine（DAG 调度）   |
+----------------------------------------------------------------------------------+
        | 任务事件 / 解析产物         | 节点执行
        v                             v
+----------------------------------------------------------------------------------+
| 执行加工层                                                                       |
|  Processing Worker --解析--> MinerU --> LlamaIndex（切块 · Embedding · 索引）       |
|    │--parsed/<task>.md--> MinIO/S3                  │--dense+sparse--> Qdrant      |
|  Sandbox Execution --> OpenSandbox（自定义代码节点）                               |
+----------------------------------------------------------------------------------+
        | 索引可检索                  | 问答 / 记忆
        v                             v
+----------------------------------------------------------------------------------+
| 检索问答层                                                                       |
|  Query Service（多路召回 · RRF · Cross-Encoder · 负反馈降权） --召回--> Qdrant      |
|    └─ 负反馈计数（rag_feedback，降权用） --> PostgreSQL                            |
|  RAG Agent + intent/summary/memory --/v1/search--> Gateway / Query Service        |
|    ├─ 记忆正文 --> PostgreSQL      └─ 记忆向量 --> Qdrant                          |
+----------------------------------------------------------------------------------+
        |
        v
+----------------------------------------------------------------------------------+
| 存储层（职责固定）                                                                |
|  PostgreSQL：身份 · RBAC · 反馈 · 会话 · 记忆正文（事实源，不存向量）                 |
|  Qdrant：文档 dense/sparse · 记忆 dense（派生索引）                                 |
|  MinIO/S3：原文件 · parsed/<task>.md（事实源）    Redis：任务状态 · 吊销 · 计量        |
|  RabbitMQ/Kafka：任务与事件 · DLQ                                                   |
+----------------------------------------------------------------------------------+
```

架构的核心特征是“计算特征与生态优势互补”：Go 负责高并发编排与解析调度，Python 的 LlamaIndex 负责复杂的数据加工，不可信代码被隔离到独立沙箱资源池，三者分别投放到独立服务中，配合 Kubernetes 的弹性调度实现精准扩缩容。

V4.0 在接入层进一步拆出 Auth Service 与 User Service：JWT 的签发与校验、用户账号与角色权限管理从网关剥离为独立服务，网关仅负责令牌校验、路由级权限判定与限流，降低接入层耦合、便于独立伸缩。检索问答层由 Query Service 承担多路召回、RRF 融合与重排（第 13 章），RAG Agent 及其 intent / summary / memory 子服务负责多轮问答与记忆（README「RAG 对话服务」），二者均经 Gateway 暴露给客户端。

## 3. 核心微服务拆分

遵循单一职责原则，系统当前拆分为以下核心微服务。相对早期版本，Processing Worker 职责收窄为解析调度，新增独立的 LlamaIndex Processing Service 承担切块与向量化加工；V4.0 又从接入层拆出 Auth / User 服务，专门承载认证签发与 RBAC：

| 服务 | 语言 | 职责 | 资源特征 |
| --- | --- | --- | --- |
| API Gateway | Go | 统一入口，鉴权、限流、路由转发 | I/O 密集，无状态 |
| Auth Service | Go | 注册 / 登录 / 刷新 / 校验，签发与验证 JWT | 轻量无状态 |
| User Service | Go | 租户隔离的用户 CRUD、角色与权限点管理（RBAC） | 轻量，需共享存储 |
| Workflow Engine | Go | 解析 DAG，按依赖调度节点，维护执行上下文 | CPU 中等，需状态存储 |
| Ingestion Service | Go | 接收上传，写对象存储与元数据，投递任务 | I/O 密集 |
| Processing Worker | Go | 消费任务，调用 MinerU 解析，产出 Markdown | I/O+等待，异步解析 |
| LlamaIndex Service | Python | 语义/层级切块、索引构建、调用 Embedding API 并写向量库 | CPU+网络，可水平扩展 |
| Query Service | Go | 多路召回并行编排、RRF 融合、Cross-Encoder 重排与问答入口 | I/O+网络，无状态，可水平扩展 |
| Sandbox Execution Service | Go | 经 OpenSandbox 执行自定义代码节点 | 隔离执行，短时生命周期 |

说明：将切块与向量化下沉到 LlamaIndex（Python）服务，是 V2.0 最重要的架构调整。它避免了用 Go 从零复刻语义切块与高级索引策略，直接复用 LlamaIndex 成熟的 Node Parser、Ingestion Pipeline 与向量库集成，同时保持了 Go 侧的轻量与高并发。

## 4. LlamaIndex 数据处理服务设计

本章为 V2.0 新增，专门说明 LlamaIndex 作为独立微服务的定位、职责边界与工程实现要点。它是本架构区别于传统 Go 硬编码管道的核心。

### 4.1 职责边界

LlamaIndex Service 由 FastAPI 封装为一组 RESTful / gRPC 接口，接收 Go 侧 Processing Worker 通过消息队列或同步调用下发的解析结果，负责以下能力：

- 高级切块：使用 SemanticSplitterNodeParser（语义切块）或 MarkdownNodeParser（按标题层级切块），基于 MinerU 输出的结构化 Markdown 进行智能切分，取代 Go 侧简单的滑动窗口切块。
- 多索引构建：针对 MinerU 输出的版面信息，同时构建稠密向量与稀疏（Qdrant sparse，IDF）索引，为 Hybrid Search 混合检索打基础；表格、公式、图片作为独立节点携带元数据。
- Embedding 编排：内部通过 LlamaIndex 的 Embedding Client 调用外部 Embedding API（如 OpenAI），完成向量化后批量写入 Milvus / Qdrant，Go 侧不再直接处理向量化。

### 4.2 与 Go 侧的协同关系

| 组件 | 角色 | 与 LlamaIndex 的关系 |
| --- | --- | --- |
| MinerU | 复杂文档解析 | 互补：MinerU 负责“文件→文本”，LlamaIndex 负责“文本→索引” |
| Go Workflow Engine | DAG 编排/调度/状态机 | 调用方：Go 是大脑，LlamaIndex 是数据加工车间 |
| OpenSandbox | 自定义代码执行 | 载体：LlamaIndex 可作为沙箱预装库供高级用户调用 |
| 外部 Embedding API | 文本向量化 | 被调用：由 LlamaIndex 内部 Client 调用，无本地推理 |

### 4.3 服务化封装要点

服务采用 FastAPI + Ingestion Pipeline 实现，对外暴露 ProcessParseResult 与 Query 两类核心接口。部署为独立容器，通过 K8s HPA 按 CPU/内存及自定义队列长度自动扩缩容。

- 轻量化依赖：仅安装 llama-index-core 及实际需要的集成包（如 llama-index-vector-stores-milvus），避免镜像臃肿与依赖冲突。
- 版本锁定：LlamaIndex 迭代极快，须在 pyproject.toml / requirements.txt 锁定精确版本号，并在 CI 中加入兼容性测试。
- 避免重复造轮子：引入 LlamaIndex 切块后，Go 侧的 Chunking 逻辑应退役或仅保留为兜底，防止两套切块策略导致数据不一致。
- 效果可量化：切换切块策略前，用 LlamaIndex 内置评估模块（Faithfulness、ContextRelevancy）对比新旧方案，确保改进可度量。

## 5. 核心数据流转与状态机

系统采用事件驱动与状态机模式，确保长事务的可靠性与可追溯性。V2.0 中，CHUNKED 之后的加工由 LlamaIndex 服务完成，任务在全链路中依次流转：

```text
PENDING -> FETCHED -> PARSING -> PARSED
   -> CHUNKED (LlamaIndex 切块) -> EMBEDDED (LlamaIndex 调用 API)
   -> INDEXED -> COMPLETED
                                     \-> FAILED (重试耗尽进 DLQ)

原地重建：COMPLETED -> REINDEXING -> CHUNKED -> ... -> COMPLETED
```

自定义代码节点在执行上下文中引入独立的运行态：SANDBOX_CREATING -> SANDBOX_RUNNING -> SANDBOX_DONE，并在沙箱销毁后并入主状态机。

重建索引（Reindex）复用解析阶段持久化的 Markdown，不重新调用 MinerU；分片使用确定性 UUID，先写入新点再删除旧点，检索侧无空窗。级联删除按 `DELETE /v1/files/{id}` 依次清理检索索引、对象存储与任务记录，索引清理失败时保留任务以便重试。

幂等性约束：消费端以 task_id 结合 content_hash 作为幂等键，防止消息重复投递导致重复调用 MinerU、LlamaIndex 或 Embedding API，避免资源浪费与脏数据。

## 6. 关键组件设计

### 6.1 MinerU 异步解析

当前实现对接 MinerU 4.x 官方 V1 Tasks 协议：`POST /v1/tasks` 创建单文件任务，按响应中的 `files[].upload` 流式 PUT 原始字节，轮询 `GET /v1/tasks/{task_id}`，完成后从 `files[].result_url` 下载 ZIP 并提取 Markdown。Processing Worker 实现有界轮询与最大重试次数，防止 Goroutine 泄漏；父任务失败时优先透传逐文件错误详情。

上传事件中的对象 URL 仅允许映射到 `OBJECT_STORE_BASE` 下的 key，再从共享 `OBJECT_STORE_DIR` 读取，包含来源与目录穿越校验，因此解析服务不依赖 MinIO 可达性。PDF/DOCX/HTML 由 MinerU 原生解析；MinerU 4.x 不接收的 TXT/Markdown 直接保留原文，避免真实解析模式下破坏既有文本上传能力。最终 Markdown 统一作为 LlamaIndex 切块的输入。

### 6.2 切块与索引（下沉至 LlamaIndex）

V2.0 将切块从 Go 迁移至 LlamaIndex 服务。LlamaIndex 按标题层级与语义边界进行结构化切分，并对表格、公式、图片等特殊块单独抽取为独立节点与元数据，使 RAG 检索时可精准命中表格或公式，而非返回混乱文本。

### 6.3 限流与背压

调用 MinerU 与外部 Embedding API 时必须引入令牌桶限流（Go 侧用 golang.org/x/time/rate，Python 侧用 asyncio + 信号量或 aiolimiter）。当外部接口返回 429 时，触发背压机制暂停从队列拉取新任务，避免雪崩与更严厉的限流。

## 7. 动态节点弹性伸缩

当某阶段阻塞时自动扩容，是本系统的关键工程能力。V2.0 中 LlamaIndex 服务因其 CPU+网络混合负载特征，同样纳入统一扩缩容体系。方案分为集群级与应用级两层。

### 7.1 集群级：K8s HPA + 自定义指标

传统 HPA 依赖 CPU/内存，而向量化常受外部 API 限制、CPU 未必高。故需暴露自定义指标驱动扩缩容：

- Go 与 Python 服务均暴露 queue_length（队列积压）与 processing_latency（处理延迟）指标（Go 用 prometheus client_golang，Python 用 prometheus_client）。
- 部署 prometheus-adapter，将指标转换为 K8s Custom Metrics API。
- HPA 按 AverageValue 目标（如平均每 Pod 队列积压 > 50）自动调整副本数，设定 min/max 副本与冷却期防抖。

### 7.2 应用级：自适应弹性协程池

在 Go 进程内部，以“保底 Worker + 临时 Worker”的弹性协程池应对突发：当空闲 Worker 极少且未达上限时按批扩容；临时 Worker 处理完任务后若 keepAlive 超时且数量大于最小值则自我注销，实现无负载时的内存回收。

### 7.3 按阻塞类型差异化扩缩

| 阻塞类型 | 特征 | 扩缩策略 |
| --- | --- | --- |
| CPU 密集（解析/切块） | CPU 利用率高 | 依赖 K8s CPU 指标扩容 Pod |
| I/O/API 阻塞（Embedding） | CPU 低但任务卡在等待 | 按队列积压扩容 + 客户端限流 + 背压 |
| 内存密集（超大文件） | 内存飙升，易 OOM | 设 resources.limits + 流式分块 + 内存指标扩容 |

## 8. 自定义节点与 DAG 编排引擎

若需支持类 Dify 的自定义节点，系统需从“固定线性管道”升级为“动态可编排的图执行引擎”。核心改造包括：

- 引入 DAG 执行引擎：由前端画布生成 JSON/YAML 描述的有向无环图，后端解析并按依赖动态调度节点。
- 节点抽象与注册：解析、LlamaIndex 切块/索引、Embedding、代码执行均封装为统一契约的标准节点，含输入/输出 Schema 与执行器。
- 执行上下文传递：节点间通过全局 Context 共享数据，上游输出写入 Context，下游按变量名提取。

自定义逻辑有两种实现路径：

| 方案 | 实现方式 | 优点 | 缺点 |
| --- | --- | --- | --- |
| HTTP/Webhook 节点 | 引擎 POST 上游数据至用户外部服务 | 完全隔离、语言无关 | 需用户自行部署、有网络延迟 |
| 代码沙箱节点 | 引擎经沙箱执行用户代码片段 | 即写即用、体验佳、强隔离 | 需沙箱基础设施、安全要求高 |

得益于 OpenSandbox 的成熟能力，代码沙箱节点的实施成本大幅降低，因此本系统采用以代码沙箱节点为主的方案。高级用户还可在沙箱内直接调用预装的 LlamaIndex 库，编写自定义检索策略。

## 9. OpenSandbox 沙箱集成方案

OpenSandbox 作为控制平面，承担沙箱的创建、调度、隔离与回收。系统新增独立的 Sandbox Execution Service，通过其 Go SDK 与 OpenSandbox Server 交互。核心特性对应如下：

| 能力 | 价值 |
| --- | --- |
| 多档隔离（runc / Firecracker microVM） | 用户代码恶意操作被限制在沙箱内，不波及宿主集群 |
| 秒级/毫秒级冷启动（约 64~125ms） | 自定义节点几乎无感冷启动，体验接近本地执行 |
| Egress 出口控制（FQDN 白名单） | 默认拒绝或仅放行特定出站，防止恶意外联与横向渗透 |
| 凭据保险箱（不落盘注入） | 以环境变量注入 API Key，用户代码读不到明文，杜绝泄露 |

安全护栏：每次执行强制设置沙箱 TTL（如 5 分钟）防死循环；无论成功失败均在 defer 中确保销毁；输入数据经 SDK Filesystem API 写入沙箱内约定路径（如 /tmp/input.json），输出以约定 JSON 结构经 Stdout 或指定文件回收。

运行形态：OpenSandbox 分为三层 —— Lifecycle Server（`/v1` 控制面，负责鉴权、调度与回收）、Runtime 后端（`docker` 或 `kubernetes`）以及沙箱内 execd（命令执行、文件与指标）。Docker 模式下 server 需要访问 Docker daemon，容器化部署时挂载 `/var/run/docker.sock`，由宿主 daemon 创建与 server 平级的沙箱容器（DOOD，非 DinD）；server 不启用 privileged，但持有该 daemon 的控制权。Kubernetes 模式下 server 以 ServiceAccount 创建 `BatchSandbox` 等 CRD，controller 拉起沙箱 Pod，execd 经 init container 注入、数据面流量走 ingress gateway，无需 Docker socket。Sandbox Execution Service 自身仅作为 SDK 客户端，不持有 Docker 权限。部署细节、数据面网络与安全边界见 [deployment.md](deployment.md) 第 8 节。

## 10. 接口定义（Protocol Buffers）

以下为系统核心服务的 Protobuf 契约，含 V2.0 新增的 LlamaIndexService（数据加工服务）、SandboxExecutionService（自定义代码节点），以及 V4.0 的 AuthService / UserService（认证与 RBAC，完整定义见 `proto/vectorization/v1/identity.proto`）。跨服务、跨语言调用以此为准。

```protobuf
syntax = "proto3";
package vectorization.v1;
option go_package = "./pb/vectorization/v1;vectorization_v1";
// ========== 摄入服务（Go）==========
service IngestionService {
  rpc UploadFile(UploadFileRequest) returns (UploadFileResponse);
  rpc GetTaskStatus(GetTaskStatusRequest) returns (GetTaskStatusResponse);
}
message UploadFileRequest {
  string file_name = 1;
  string file_type = 2;        // pdf / docx / txt
  int64  file_size = 3;
  string presigned_url = 4;    // 预签名直传地址
  map<string,string> metadata = 5;
}
message UploadFileResponse {
  string task_id = 1;  string status = 2;  string message = 3;
}
message GetTaskStatusRequest  { string task_id = 1; }
message GetTaskStatusResponse {
  string task_id = 1;  string status = 2;  int32 progress = 3;  string error_msg = 4;
}
// ========== 解析服务（Go: 调用 MinerU）==========
service ProcessingService {
  rpc SubmitParseTask(SubmitParseTaskRequest) returns (SubmitParseTaskResponse);
}
message SubmitParseTaskRequest {
  string task_id = 1;  string file_url = 2;  string file_type = 3;
}
message SubmitParseTaskResponse {
  bool   success = 1;  string mineru_task_id = 2;  string markdown = 3;
}
// ========== 数据加工服务（Python: LlamaIndex 切块/索引/向量化）==========
service LlamaIndexService {
  rpc ProcessAndIndex(IndexRequest) returns (IndexResponse);
}
message IndexRequest {
  string task_id = 1;
  string markdown = 2;             // MinerU 解析产出的结构化文本
  string split_strategy = 3;       // semantic / markdown / fixed
  string embedding_model = 4;      // 外部 Embedding 模型名
  string vector_store = 5;         // milvus / qdrant
  map<string,string> metadata = 6;
}
message IndexResponse {
  bool   success = 1;
  int32  chunk_count = 2;
  int32  indexed_count = 3;
  string collection = 4;
  string error_msg = 5;
}
// ========== 沙箱执行服务（自定义代码节点）==========
service SandboxExecutionService {
  rpc Execute(CodeExecuteRequest) returns (CodeExecuteResponse);
}
enum Runtime {
  RUNTIME_UNSPECIFIED = 0;  PYTHON_311 = 1;  NODE_18 = 2;
}
message CodeExecuteRequest {
  string task_id = 1;  string node_id = 2;  Runtime runtime = 3;
  string code = 4;  repeated string packages = 5;
  map<string,string> env_vars = 6;  string input_json = 7;
  int32  timeout_seconds = 8;
}
message CodeExecuteResponse {
  bool   success = 1;  string output_json = 2;  string stdout = 3;
  string stderr = 4;  string sandbox_id = 5;  string error_msg = 6;
}
// ========== 认证服务（注册 / 登录 / 刷新 / 校验，签发 JWT）==========
service AuthService {
  rpc Register(RegisterRequest) returns (RegisterResponse);
  rpc Login(LoginRequest)       returns (LoginResponse);
  rpc Refresh(RefreshRequest)   returns (LoginResponse);
  rpc Logout(LogoutRequest)     returns (LogoutResponse);
  rpc Validate(ValidateRequest) returns (ValidateResponse);
}
message UserInfo {
  string id = 1;  string tenant_id = 2;  string username = 3;  string email = 4;
  repeated string roles = 5;  repeated string departments = 6;  string status = 7;
}
message TokenPair {
  string access_token = 1;  string refresh_token = 2;  int64 expires_in = 3;
}
message RegisterRequest  { string tenant_id = 1;  string username = 2;  string password = 3;  string email = 4; }
message RegisterResponse { UserInfo user = 1; }
message LoginRequest     { string tenant_id = 1;  string username = 2;  string password = 3; }
message LoginResponse    { TokenPair tokens = 1;  UserInfo user = 2; }
message RefreshRequest   { string refresh_token = 1; }
message LogoutRequest    { string refresh_token = 1; }
message LogoutResponse   { bool success = 1; }
message ValidateRequest  { string access_token = 1; }
message ValidateResponse { bool valid = 1;  UserInfo user = 2; }
// ========== 用户与 RBAC 服务 ==========
service UserService {
  rpc CreateUser(CreateUserRequest) returns (UserResponse);
  rpc GetUser(GetUserRequest)       returns (UserResponse);
  rpc ListUsers(ListUsersRequest)   returns (ListUsersResponse);
  rpc UpdateUser(UpdateUserRequest) returns (UserResponse);
  rpc DeleteUser(DeleteUserRequest) returns (DeleteUserResponse);
  rpc ListRoles(ListRolesRequest)   returns (ListRolesResponse);
  rpc UpsertRole(UpsertRoleRequest) returns (RoleResponse);
  rpc DeleteRole(DeleteRoleRequest) returns (DeleteRoleResponse);
  rpc ListPermissions(ListPermissionsRequest) returns (ListPermissionsResponse);
}
message RoleInfo       { string name = 1;  repeated string permissions = 2;  bool builtin = 3;  string description = 4; }
message PermissionInfo { string key = 1;  string description = 2; }
// CreateUser / GetUser / ListUsers / UpdateUser / DeleteUser 的请求以 tenant_id 隔离，字段从略
```

## 11. 沙箱执行服务核心代码框架（Go）

以下为 Sandbox Execution Service 的核心骨架，演示“创建沙箱 — 注入输入 — 执行代码 — 回收结果 — 确保销毁”的完整流程。示例基于 OpenSandbox Go SDK 的典型接口风格，实际方法名以所用 SDK 版本为准。

```go
package main
import (
    "context"
    "encoding/json"
    "time"
    pb  "your/module/pb/vectorization/v1"
    sbx "your/module/opensandbox/sdk"
)
type SandboxExecutor struct {
    client *sbx.Client
    pb.UnimplementedSandboxExecutionServiceServer
}
// Execute 执行自定义代码节点：强隔离 + 强制 TTL + 保证回收
func (e *SandboxExecutor) Execute(ctx context.Context, req *pb.CodeExecuteRequest) (*pb.CodeExecuteResponse, error) {
    ttl := time.Duration(req.TimeoutSeconds) * time.Second
    if ttl <= 0 { ttl = 5 * time.Minute }
    // 1) 创建沙箱：绑定运行时镜像、出口白名单、TTL
    sb, err := e.client.CreateSandbox(ctx, &sbx.CreateOptions{
        Runtime:     mapRuntime(req.Runtime),
        Packages:    req.Packages,
        TTLSeconds:  int64(ttl.Seconds()),
        EgressAllow: []string{"api.internal.svc"}, // 默认拒绝其它出站
        Env:         req.EnvVars,                   // 凭据注入，不落盘
    })
    if err != nil {
        return &pb.CodeExecuteResponse{Success:false, ErrorMsg:err.Error()}, nil
    }
    // 无论成功失败，确保销毁沙箱，防止资源泄漏
    defer func() { _ = sb.Destroy(context.Background()) }()
    // 2) 注入输入：将上游 Context 序列化写入沙箱内约定路径
    if req.InputJSON != "" {
        if err := sb.WriteFile(ctx, "/tmp/input.json", []byte(req.InputJSON)); err != nil {
            return &pb.CodeExecuteResponse{Success:false, SandboxID:sb.ID, ErrorMsg:err.Error()}, nil
        }
    }
    // 3) 执行用户代码，施加执行超时保护
    execCtx, cancel := context.WithTimeout(ctx, ttl)
    defer cancel()
    res, err := sb.RunCode(execCtx, &sbx.RunOptions{
        Code:      req.Code,
        EntryFile: "/tmp/main.py",
        Args:      []string{"/tmp/input.json"},
    })
    resp := &pb.CodeExecuteResponse{
        SandboxID: sb.ID, Stdout: res.Stdout, Stderr: res.Stderr,
    }
    if err != nil {
        resp.Success = false; resp.ErrorMsg = err.Error(); return resp, nil
    }
    // 4) 回收结构化输出：约定代码将结果写入 /tmp/output.json
    out, err := sb.ReadFile(execCtx, "/tmp/output.json")
    if err != nil {
        resp.OutputJSON = res.Stdout          // 回退到 stdout 采集
    } else {
        var probe map[string]interface{}
        if json.Unmarshal(out, &probe) == nil {
            resp.OutputJSON = string(out)
        } else { resp.OutputJSON = res.Stdout }
    }
    resp.Success = true
    return resp, nil
}
func mapRuntime(r pb.Runtime) string {
    switch r {
    case pb.Runtime_PYTHON_311: return "python:3.11"
    case pb.Runtime_NODE_18:    return "node:18"
    default:                    return "python:3.11"
    }
}
```

设计要点：TTL 兜底防止用户死循环占用资源；defer 中无条件销毁保证回收；输入/输出均走沙箱内约定文件而非环境变量，规避参数注入与截断风险；出口白名单 + 凭据不落盘双重护栏，确保不可信代码无法外联或窃取密钥。

## 12. 工程落地与可靠性建议

- 死信队列（DLQ）：MinerU 解析失败或 LlamaIndex/Embedding 持续报错的任务进入 DLQ 并触发告警，配合回调将任务标记为失败并清理临时文件。
- 分布式事务补偿（Saga）：长链路失败按已执行阶段逆序补偿，避免半完成状态产生脏数据。
- 可观测性：以 TraceID 贯穿上传、解析、LlamaIndex 切块、向量化、沙箱执行全链路，接入 OpenTelemetry/Jaeger 与 Prometheus/Grafana，Go 与 Python 服务统一埋点。
- 大文件流式处理：超大文件采用分块下载与流式切块，配合内存指标控制，防止 OOM 拖垮节点。
- LlamaIndex 治理：锁定精确版本、精简依赖、上线前用内置评估模块验证切块与检索效果，避免盲目升级破坏兼容性。
- 渐进式演进：先落地固定管道跑通业务闭环，再抽象节点注册中心，最后引入 DAG 引擎与沙箱代码节点，平滑升级为平台。

## 13. 查询链路设计（多路召回与 Rerank）

写入链路负责把文件加工为可检索的向量与索引，查询链路则负责在检索时以最高召回质量返回上下文。二者共用同一套向量库与元数据结构。与写入侧同理，查询侧遵循“Go 编排、LlamaIndex 承载检索策略”的分工：Go 侧的 Query Service 只做鉴权、路由、结果拼装与流式返回；多路召回、融合与重排等重逻辑由 LlamaIndex 的 Retriever / QueryEngine 在 Python 微服务内完成。

### 13.1 多路召回策略

单一向量召回对专有名词、精确编号、长尾关键词召回不足，因此查询侧采用多路并行召回再融合的策略，各路独立产出候选集后统一进入融合与重排阶段。关键词路优先使用 Qdrant 原生 sparse（每 KB 伴生集合 `<collection>__sparse`，`modifier=idf`），写入时与稠密向量共享稳定 ID；sparse 未覆盖或不可用时回退到 scroll + BM25。

| 召回路 | 检索引擎 | 擅长场景 | 关键参数 |
| --- | --- | --- | --- |
| 向量语义召回 | Milvus / Qdrant ANN | 语义相近、同义改写 | top_k、nprobe、ef |
| 关键词召回 | Qdrant sparse（IDF）/ BM25 兜底 | 专有名词、精确短语、编号 | top_k、字段权重 |
| 标量过滤召回 | 向量库 metadata 过滤 | 按来源、时间、权限裁剪 | filter 表达式 |
| 图谱/层级召回 | 父文档回溯 | 表格、章节级长上下文 | parent_expand |

### 13.2 结果融合与重排（Rerank）

各路召回结果的评分尺度不可直接比较，先用 RRF（倒数排名融合）按名次融合，规避分数不可比问题；再对融合后的候选用交叉编码器（Cross-Encoder，如 bge-reranker 或外部 Rerank API）逐条与查询精算相关性，取重排后 top-N 作为最终上下文。重排是精排，召回是粗排，二者组成“粗排广撒网、精排准定位”的两级漏斗。

- 融合层：对每路结果计算 RRF 得分 = Σ 1/(k + rank)，k 常取 60，合并去重后形成统一候选池。
- 去重层：按 chunk_id 与内容哈希去重，保留同一片段的最高召回来源标记。
- 重排层：Cross-Encoder 精算 (query, chunk) 相关性，输出重排分，取 top-N（N 通常 3–8）。
- 降级层：Rerank 超时或不可用时，回退到 RRF 融合结果，保证链路可用（Fail-open）。

### 13.3 查询改写与意图路由

检索质量很大程度取决于查询本身。进入召回前，先做一层轻量查询处理：多查询扩展（Multi-Query）把一条提问改写为若干互补子查询并行召回；HyDE 用模型生成的假设性文档去检索，缓解查询与文档的表述鸿沟；对含明确来源/时间约束的查询，抽取标量条件转成 metadata 过滤。意图路由决定走纯检索、检索增强生成还是结构化查询（如 SQL/表格问答）分支。

### 13.4 查询时序图

```text
[用户 Query]
   |  (gRPC)
   v
[Query Service (Go)] --鉴权/限流--> [Query Rewrite (LLM: 多查询/HyDE)]
   |
   +---------------------+---------------------+
   v (并行)              v                     v
[向量召回]         [Sparse/BM25 召回]       [标量过滤]
   |                     |                     |
   +----------+----------+----------+----------+
              v                                   
        [RRF 融合 + 去重 (LlamaIndex)]            
              v                                   
        [Cross-Encoder Rerank 精排]               
              v                                   
        [组装上下文 -> 生成 / 返回引用]
```

### 13.5 查询服务接口（Protocol Buffers）

```protobuf
syntax = "proto3";
package vectorization.v1;
option go_package = "./pb/vectorization/v1;vectorization_v1";
service QueryService {
  rpc Search(SearchRequest) returns (SearchResponse);
  rpc Answer(AnswerRequest) returns (stream AnswerChunk);
}
message RecallRoute {
  string type = 1;          // vector | bm25 | filter | parent
  int32  top_k = 2;
  string filter_expr = 3;   // metadata 过滤表达式
}
message SearchRequest {
  string query = 1;
  repeated RecallRoute routes = 2;   // 多路召回配置
  string rerank_model = 3;           // 空则不重排，Fail-open
  int32  final_top_n = 4;
  string kb_id = 5;                  // 知识库/命名空间
  map<string,string> filters = 6;
}
message RetrievedChunk {
  string chunk_id = 1;
  string content = 2;
  float  score = 3;                  // 重排后相关性
  map<string,string> meta = 4;       // 来源/页码/标题层级
}
message SearchResponse {
  repeated RetrievedChunk chunks = 1;
  string  trace_id = 2;
  int32   recall_count = 3;
  int32   rerank_applied = 4;
}
```

### 13.6 关键检索参数

| 参数 | 建议取值 | 说明 |
| --- | --- | --- |
| 单路召回 top_k | 20–50 | 粗排候选量，越大越全但越慢 |
| RRF 常数 k | 60 | 融合平滑系数，业界经验值 |
| 重排输入量 | 30–80 | 送入 Cross-Encoder 的候选数 |
| 最终上下文 top_n | 3–8 | 进入生成的片段数，防噪声 |
| Rerank 超时 | 1–2s | 超时降级为 RRF 结果 |

## 14. 反馈闭环（Feedback Loop）设计

查询链路的输出并非终点。一个可持续变强的 RAG 系统必须把“检索得好不好、答案对不对”的信号回流到索引与策略层，形成数据飞轮。反馈闭环把线上真实的用户行为与质量评估沉淀为可训练、可调参、可回灌的数据资产，驱动切块粒度、召回权重、Rerank 模型与 Prompt 的持续优化。

### 14.1 反馈信号采集

反馈分为显式与隐式两类。显式来自用户直接评价，隐式来自行为与自动评估。采集时统一带上 trace_id，使其可回溯到具体的查询、召回集与生成结果，这是后续归因分析的前提。

| 反馈信号 | 类型 | 含义 | 回灌用途 |
| --- | --- | --- | --- |
| 点赞/点踩 | 显式 | 答案整体满意度 | 质量标签、难例挖掘 |
| 引用点击 | 隐式 | 哪段上下文被采信 | 调整 chunk 权重/排序 |
| 答案纠错 | 显式 | 人工给出正确表述 | 微调/构建评测集 |
| 无结果/低分查询 | 隐式 | 召回不足或索引缺口 | 补数据、调切块 |
| RAGAS 自动评估 | 系统 | 忠实度/相关性打分 | 回归评测、上线门禁 |

> 落地状态：点赞/点踩（检索页与问答页）、引用点击已实现并落库；问答页「没帮助」后可提交纠正答案（`corrected_answer`）并随评分落库；无结果查询由 `eval/rag_eval.py` 与低分查询 API 覆盖；RAGAS / LLM-as-judge 语义评分由 `eval/judge.py` 提供（需配置 LLM Key）。

### 14.2 反馈数据模型

```protobuf
syntax = "proto3";
package vectorization.v1;
// 由 Query Service 异步落库，经 MQ 进入反馈流水线
message FeedbackEvent {
  string trace_id = 1;        // 关联一次查询全链路
  string query = 2;
  string answer = 3;
  repeated string cited_chunk_ids = 4;  // 被引用片段
  int32  rating = 5;          // +1 赞 / -1 踩 / 0 无
  string corrected_answer = 6;// 人工纠错文本，可空
  float  faithfulness = 7;    // RAGAS 忠实度，可空
  int64  ts = 8;
  map<string,string> meta = 9;
}
```

实际实现位于 `backend/internal/feedback`，使用 Postgres `rag_feedback` 表（`FEEDBACK_DRIVER=postgres`，开发环境默认内存实现）。字段包括 `tenant_id / user_id / subject_id / trace_id / conversation_id / message_id / name / rating / comment / corrected_answer / query_text / answer_text / cited_chunk_ids / meta / created_at / updated_at`；`subject_id` 取 `message_id`，为空时回退 `trace_id`，按 `(tenant_id, user_id, subject_id, name)` 覆盖更新，同一消息只保留最新评分。反馈与 tracing 解耦：先落库，再旁路写 trace score，tracing 为 Noop 时也不会丢数据。

### 14.3 闭环处理流程（在线/离线分层）

闭环分在线与离线两层。在线层做实时、轻量的反馈落库与快速特征更新（如 chunk 点击计数、被踩答案的临时降权）；离线层做重计算，天/周级跑批，产出评测集、难例库与调参建议，经灰度验证后再回灌线上，避免抖动。

```text
[线上查询] --> [Query Service] --异步--> [FeedbackEvent 落库]
                                            |
        +-----------------------------------+-----------------+
        v (在线, 秒/分钟级)                v (离线, 天/周级批处理)
[实时降权/点击计数]                 [难例挖掘 + 评测集构建]
                                            |
                                            v
[线上检索/生成] <--[灰度验证+门禁]<- [调参/重训/Rerank更新]
                                            |
                                   [更新切块策略 / 召回权重]
```

### 14.4 难例挖掘与策略回灌

- 难例挖掘：聚聚类低分、被踩、无命中的查询，定位“检索失败根因”——切块过粗/过碎、向量召不回、缺数据、需标量过滤。
- 切块策略回灌：对因切块导致的召回失败，调整对应文档的 chunk 粒度或改用层级/语义切块并重建索引。
- 召回权重回灌：按反馈统计调 RRF 各路权重或向量/BM25 配比，提升高价值召回路的贡献。
- Rerank 回灌：用纠错样本构建标注对，微调或替换 Rerank 模型，提升精排准确性。
- 评测集沉淀：把人工纠错与高分答案固化为 Golden Set，作为每次策略变更的回归基线。

### 14.5 效果评估指标

闭环必须有可量化指标守护，否则无法判断某次回灌是变好还是变坏。检索侧关注召回质量，生成侧关注忠实与相关，闭环层关注飞轮效率。上线任何检索/切块/模型变更前，须在 Golden Set 上通过回归门禁。

| 指标 | 层面 | 含义 | 方向 |
| --- | --- | --- | --- |
| Hit Rate@k | 检索 | 前 k 命中正确来源比例 | 越高越好 |
| MRR | 检索 | 首个正确结果排名倒数均值 | 越高越好 |
| nDCG@k | 检索 | 考虑排名位置的归一化增益 | 越高越好 |
| Faithfulness | 生成 | 答案对上下文忠实度(RAGAS) | 越高越好 |
| 答案采纳率 | 业务 | 点赞/(赞+踩) | 越高越好 |
| 闭环周转时长 | 飞轮 | 反馈到策略生效时间 | 越短越好 |

### 14.6 工程注意事项

- 采样与去偏：显式反馈天然稀疏且偏负面，需按流量分层采样并按曝光归一，避免被少数活跃用户带偏。
- 在线/离线一致：离线评测复现线上真实召回配置（同样的路由/权重），否则离线指标失真。
- 灰度与回滚：策略变更先小流量灰度，指标劣化即自动回滚，不直接全量。
- 隐私与合规：反馈含用户问答原文，落库需脱敏与访问控制，保留期与合规要求对齐。
- 冷启动：无反馈初期以系统自动评估（RAGAS/规则）驱动，随真实反馈累积逐步切换权重。

### 14.7 当前落地情况

| 能力 | 状态 | 入口 |
| --- | --- | --- |
| 显式评分（赞/踩） | 已实现 | `POST /v1/feedback`；检索页与问答页 |
| 隐式引用点击 | 已实现 | 问答页引用角标上报 `user.citation_click` |
| 反馈持久化 | 已实现 | Postgres `rag_feedback`；开发环境内存实现 |
| 低分查询 | 已实现 | `GET /v1/feedback?rating=-1`（`user:read`） |
| 离线检索评测 | 已实现 | `eval/rag_eval.py`：Recall@k / MRR / nDCG@k |
| 引用规则校验 | 已实现 | `--with-chat`：citation_validity / hit_rate / inline_marker_rate |
| 阈值门禁 | 已实现 | `eval/thresholds.json` + `make eval-rag`，低于阈值退出码 1 |
| 低分归因 / 回归集导出 | 已实现 | `eval/analyze_feedback.py`：五类归因 + `eval/regressions.jsonl` |
| RAGAS / LLM-as-judge | 已接入（需 LLM Key） | `eval/judge.py`：faithfulness / relevancy / completeness |
| 在线降权 / 灰度回灌 | 已实现 | 检索 `FeedbackPenalty`（累计负反馈降权）；`GET /v1/corrected-answers` 供 rag-agent 检索前置回灌 |

## 15. 多租户与权限治理

企业级部署必须把多租户隔离与细粒度权限贯穿数据管道与查询链路的每个环节。Go 控制平面作为统一入口承担鉴权与隔离职责，Python 加工与检索侧严格遵循下传的租户上下文，禁止跨租户串数据。

### 15.1 统一鉴权中心

鉴权中心已落地为两个独立微服务：**Auth Service** 负责注册、登录、令牌刷新与校验，**User Service** 负责租户隔离的用户 CRUD 与角色/权限点管理（契约见 `proto/vectorization/v1/identity.proto`，实现位于 `backend/internal/identity`）。

令牌为 HS256 JWT，claims 携带 `tenant_id`、`user_id`、`roles`、`permissions`、`token_type`；access token 默认 15 分钟、refresh token 默认 168 小时，`token_type` 阻止 refresh 令牌被当作 access 令牌使用。登录时由角色展开权限点写入令牌，实现"权限随令牌下发"。

网关层解析 JWT 提取 tenant_id 与权限点，注入下游请求上下文（gRPC metadata `x-tenant-id` / `x-user-id` / `x-roles` / `x-permissions`）随任务全程透传；路由级 `RequirePermission` 中间件据此零回调判定鉴权，并基于 tenant_id 实施 API 限流与配额，形成"一次鉴权、全链携带"的闭环。

权限模型为固定权限点目录（`user:read`、`file:write`、`kb:read`、`kb:write`、`usage:read`、`sandbox:execute`、`trace:read` 等 17 个 + 通配 `*`）与角色组合：内置 `admin`（全量）、`editor`（内容与知识库读写 + 检索/沙箱/工作流）、`viewer`（只读），并支持自定义角色自由组合权限点；内置角色不可修改删除。

在全局角色之外，系统还提供知识库级 RBAC：`kb_permissions` 按 `kb_id` 将用户或角色绑定到 `kb:admin` / `kb:editor` / `kb:viewer`。解析优先级为「显式用户绑定 > 角色绑定 > 租户 admin > default 库成员默认 viewer」，未绑定且非 default 的知识库对调用者不可见；网关在文件上传/删除/重建、检索问答与知识库管理入口按该角色校验。

### 15.2 ACL 元数据绑定与前置过滤

LlamaIndex 在切块写库时强行绑定多租户 ACL 元数据（tenant_id、department_ids、doc_acl），向量库与关键词库均以其作为标量字段。检索阶段执行权限前置过滤（Pre-filtering），在向量近邻计算前即下推过滤条件，既保证越权内容物理不可见，又避免"先召回后过滤"导致的召回量塌陷与越权泄露。

### 15.3 网盘增量同步与级联清理

数据源支持网盘/对象存储增量监听：以文件指纹（MD5 或强哈希）判断新增、变更与删除。文件更新或删除时，依据指纹定位旧向量切片与图谱节点做级联清理，防止陈旧切片污染检索；清理与重建在同一租户命名空间内原子完成。

### 15.4 多租户限流与资源隔离

在租户级配置 QPS、并发与每日算力配额，网关与 Worker 双层令牌桶兜底；重计算任务（解析/图谱构建）按租户公平调度，避免单租户大文件挤占全局资源。

## 16. LightRAG 图增强检索

纯向量检索难以覆盖跨文档、多跳推理与全局摘要类问题。系统引入 LightRAG 构建知识图谱索引，与稠密向量、BM25 稀疏检索并列，组成三路召回，显著提升多跳与主题聚合型问答的质量。

### 16.1 图谱索引与租户隔离图空间

LlamaIndex 加工链路在切块写向量的同时，抽取实体与关系写入 LightRAG；按租户划分独立图空间（Graph Namespace），图数据物理或逻辑隔离，与 ACL 元数据一致，杜绝跨租户图谱串联。

### 16.2 Local 与 Global 双模式查询

LightRAG 支持两种检索模式：Local 模式聚焦实体邻域的局部细节问答，Global 模式聚合高层主题做全局摘要与综述。查询路由按意图选择模式，复杂问题可双模式并发后融合。

### 16.3 三路召回融合

向量、BM25 与 LightRAG 三路结果统一在融合层归一：稠密+稀疏走 RRF 融合，图谱召回按图距离与相关度加权并入，再进入 Cross-Encoder 重排（衔接第 13 章两级漏斗）。图谱缺失或超时时 Fail-open 降级为纯向量+BM25，不阻断主链路。

## 17. NL2SQL 结构化问答引擎

除文档类非结构化问答外，企业常有对业务数据库的结构化查询诉求。系统提供自然语言转 SQL（NL2SQL）通道，让业务人员以自然语言获取指标与明细，并可与文档问答融合输出。

### 17.1 自然语言到只读安全 SQL

由意图路由识别结构化查询后，LLM 依据语义层与表 Schema 生成 SQL。所有生成 SQL 强制走只读账号、注入行级/租户级权限过滤条件，经 SQL 解析器做静态校验（禁 DDL/DML、禁跨库、限返回行数与超时），校验不通过直接拒绝执行，杜绝注入与越权。

### 17.2 语义层与 Schema Linking

维护指标口径与字段别名的语义层，提升列/表链接（Schema Linking）准确率；高频问题沉淀为受控模板，降低自由生成 SQL 的不确定性。

### 17.3 结构化与非结构化融合

对"既要有数、又要有据"的复合问题，NL2SQL 取回的指标可与文档检索召回的上下文在同一次生成中融合，统一由答案组装层拼装并标注来源。

## 18. 多租户长期记忆层

为提升个性化与连续性，系统提供跨会话的长期记忆。职责划分固定为：**PostgreSQL 保存记忆正文与元数据（事实源），Qdrant 保存记忆向量（派生索引）**，双层隔离并与租户 ACL 对齐；向量召回按 `tenant_id + user_id` 取 top-k 后回 Postgres 取正文，Qdrant 不可用时退化为词面召回。写入与合并先落 Postgres，成功后再按 `memory_id` 覆盖 Qdrant；删除同样以 Postgres 为准，索引清理失败可重试。

### 18.1 双层记忆模型

上层为租户级全局规则（统一术语、业务约束、合规话术），下层为用户级个人偏好（称呼、常用维度、历史结论）。读写均在召回时按 tenant_id 与 user_id 前置过滤，互不越界。

### 18.2 异步读写闭环

推理前并行召回相关记忆注入 Prompt；交互后经异步队列由 LLM 抽取新事实/偏好，去重与冲突消解后更新记忆库，避免同步写放大主链路延迟。记忆命中情况作为 Span 记入 Tracing（第 19 章），纳入反馈闭环评估。

### 18.3 存储职责

| 数据 | 存储 | 角色 |
| --- | --- | --- |
| 记忆正文、`normalized`、`kind`、时间戳 | PostgreSQL `rag_memories` | 事实源，去重与合并在此完成 |
| 记忆向量与 `tenant_id/user_id/memory_id` | Qdrant `nexusflow_memories` | 派生索引，按 owner 过滤召回 |
| 文档 chunk dense/sparse | Qdrant 每 KB collection 及 `__sparse` 伴生集合 | 派生索引 |
| 原始文件、`parsed/<task>.md` | MinIO/S3 | 事实源，支持重建索引 |

`MEMORY_DRIVER=qdrant` 时先写 Postgres 再同步 Qdrant；Qdrant 写失败不影响正文落库，召回退化为词面匹配。历史 pgvector 向量通过 `python -m app.backfill` 回填；`MEMORY_DRIVER=postgres` 仅作为回滚路径保留。

## 19. Langfuse 可观测性与治理

引入自建的 Langfuse 作为 LLM 应用治理底座，统一承载提示词版本、全链路追踪与自动化评审，为反馈闭环（第 14 章）提供数据与度量支撑。

> 实现状态：19.1 / 19.2 的核心能力已在 `backend/internal/platform/{prompt,tracing}` 落地——版本化 Prompt 注册表、检索/问答全链路 Tracing（`MemoryTracer` 供开发态查看，`LangfuseTracer` 批量导出且 fail-open），以及经 `POST /v1/feedback` 回传的赞/踩评分。19.3 的定时 LLM-as-a-Judge 自动化评审仍属目标设计。

### 19.1 Prompt 版本管理

所有提示词托管于 Langfuse，支持按租户/场景打标、动态热拉取与无感热更新；新版本以 A/B 实验灰度，指标劣化一键回滚。

### 19.2 全链路 Tracing

将工作流节点、检索 Context、图谱召回、长期记忆命中与 LLM 调用统一映射为 Span，记录输入输出、延迟与 token 耗算，支撑问题定位、成本归因与性能优化。

### 19.3 LLM-as-a-Judge 自动化评审闭环

结合前端"赞/踩"显式反馈与定时 LLM-as-a-Judge 任务（评估相关性、忠实度、完整性），评分回传 Langfuse 形成数据飞轮，驱动切块策略、召回权重、Rerank 与提示词的持续优化。

## 20. 技术栈选型与关键取舍

在融合外部成熟组件的同时，本方案在几处关键决策上做了与企业级实时性、隔离性相匹配的取舍，汇总如下。

| 层级 | 选型 | 说明 |
| --- | --- | --- |
| 前端交互 | React + TypeScript + Tailwind + React Flow | 可视化工作流画布，生成 DAG DSL |
| 网关与调度 | Go + Gin + RabbitMQ/Asynq + Kafka | 高并发网关、多租户鉴权、生产者/消费者削峰 |
| 认证与权限 | Go + JWT(HS256) + bcrypt + RBAC | 独立 Auth / User 服务，权限随令牌下发，路由级判定 |
| AI 计算与加工 | Python + FastAPI + LlamaIndex | 切块、多路索引、检索与 Rerank、NL2SQL、记忆读写 |
| 文档解析 | MinerU 4.x（V1 Tasks API） | 复杂版面/图表还原为结构化 Markdown |
| 检索增强 | Milvus/Qdrant + LightRAG + BGE-Reranker | 向量+稀疏+图谱三路召回、RRF+重排 |
| 执行沙箱 | OpenSandbox（主）/ RestrictedPython（轻量可选） | 强隔离执行用户代码；纯字符串转换类可退化为受限解释器 |
| 存储 | MinIO + Qdrant + PostgreSQL | 文件对象 / 文档与记忆向量 / 业务库与记忆正文 |
| 观测治理 | Langfuse（自建） | Prompt 版本、Tracing、LLM-as-a-Judge 评审 |

关键取舍说明：

- 沙箱：以 OpenSandbox 承担强隔离的自定义代码节点（microVM/容器级隔离、出口白名单、凭据不落盘）；仅对无副作用的纯文本转换类内联逻辑，才允许 RestrictedPython 这类轻量受限解释器，二者按风险分级选用。
- 编排：DAG 的解析、调度与状态机保留在 Go 控制平面，重度 AI 计算下沉 Python 微服务，避免长任务阻塞网关、也便于分别按 CPU/GPU 特征弹性伸缩。
- 检索：向量、BM25、LightRAG 三路并发 + RRF + Cross-Encoder，任一非核心路失败 Fail-open，保证可用性下限。
- 多租户：ACL 元数据全链透传并前置过滤，图谱与记忆按租户命名空间隔离，是"数据物理不可见"的硬约束而非仅逻辑过滤。

—— 文档结束 ——
