# 快速上手（10 分钟）

从零启动一套全功能 NexusFlow，导入示例数据，并亲手走完「上传 → 解析 → 检索 → 问答 → 工作流 → 审批」主链路。

- 想了解每个页面的细节：见 [用户使用指南](./user-guide.md)
- 想查看实测数据：见 [全流程验证证据](./e2e-evidence.md)
- Kubernetes 部署：见 [deployment.md](./deployment.md)

## 0. 准备

- Docker Desktop（Compose v2）
- Python 3.10+（仅运行示例与验证脚本）
- 空闲端口：`8088`（控制台）、`8080` 或自定义 `GATEWAY_PUBLISH_PORT`、`8081`、`8000`、`8001`、`6333`、`5432` 或自定义 `POSTGRES_PUBLISH_PORT`

```bash
git clone <repo-url> NexusFlow
cd NexusFlow
cp .env.example .env
```

按需在 `.env` 中填写 LLM / Embedding / MinerU 配置。只体验文档检索与上传时，可以不配 LLM；问答与 LightRAG 需要 OpenAI 兼容的模型服务。

## 1. 启动服务

```bash
make up
docker compose --env-file .env -f deploy/docker-compose.yml ps
```

等待所有服务显示 `healthy`，然后打开：

- 控制台：http://127.0.0.1:8088
- Gateway：http://127.0.0.1:18080（以 `.env` 的 `GATEWAY_PUBLISH_PORT` 为准）

用种子管理员登录：`admin / admin123`。

## 2. 导入示例数据

```bash
python scripts/seed_demo_data.py
```

脚本会创建「示例知识库」、上传 3 篇文档、生成演示账号与 API Key，并自动执行一次检索和问答自检。

可选：开启审批、MCP、Agent 节点与 LightRAG（写入 `.env` 后重新 `make up`）：

```dotenv
WORKFLOW_AGENT_NODE=true
HITL_ENABLED=true
AGENT_HUB_TOOLS_ENABLED=true
AGENT_HUB_REASONING_ENABLED=true
AGENT_HUB_MCP_ENABLED=true
AGENT_HUB_HITL_ENABLED=true
AGENT_HUB_WORKFLOW_ENABLED=true
AGENT_HUB_MCP_SESSION_DRIVER=postgres
RAG_PII_ENABLED=true
LLAMAINDEX_LIGHTRAG_ENABLED=true
LLAMAINDEX_LIGHTRAG_STORAGE_DRIVER=postgres
LLAMAINDEX_LIGHTRAG_EMBEDDING_DIM=1024   # 与 Embedding 模型维度一致
```

## 3. 控制台走一遍

| 步骤 | 页面 | 做什么 |
| --- | --- | --- |
| 1 | 概览 | 查看任务数、存储/文件/问答配额 |
| 2 | 文件管理 | 切换「示例知识库」，上传一个 PDF/Markdown，观察任务状态 |
| 3 | 任务中心 | 打开任务详情，确认解析完成、可读取 layout 与图片 |
| 4 | 检索测试 | 勾选 vector + bm25 + graph，查看融合结果与 `meta.retriever` |
| 5 | 智能问答 | 提问“NexusFlow 的混合检索包含哪些召回路由？”，点击引用跳转 |
| 6 | 工作流编排 | 从节点库拖入节点，校验并运行；或直接用下方示例 DAG |
| 7 | 审批中心 | 运行 `approval-gate.json`，批准后观察运行恢复 |

示例问题：

- NexusFlow 的混合检索包含哪些召回路由？
- 删除文档时会清理哪些数据？
- API Key 被吊销后多久生效？

## 4. API 走一遍

```bash
GATEWAY=http://127.0.0.1:18080

# 登录拿 token
TOKEN=$(curl -s $GATEWAY/v1/auth/login -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin123"}' \
  | python -c 'import sys,json;print(json.load(sys.stdin)["access_token"])')

# 检索
curl -s $GATEWAY/v1/search -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"query":"混合检索","routes":[{"type":"vector","top_k":10},{"type":"bm25","top_k":10}],"final_top_n":5}'

# 运行示例工作流（条件分流）
curl -s $GATEWAY/v1/workflows/run -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d @examples/demo/workflows/conditional-routing.json
```

使用种子脚本生成的 API Key 调用 OpenAI 兼容端点：

```bash
curl -s $GATEWAY/v1/openai/chat/completions \
  -H "Authorization: Bearer nf-sk-..." -H 'Content-Type: application/json' \
  -d '{"model":"nexusflow","messages":[{"role":"user","content":"NexusFlow 有哪些召回路由？"}]}'
```

> Python SDK 的 `base_url` 填 `http://127.0.0.1:18080/v1/openai`。

## 5. 验收与证据

```bash
python scripts/smoke_test.py --require-mineru   # 发布冒烟：32 项
python scripts/e2e_demo.py --output docs/e2e-evidence.md  # 全流程：20 项
```

`e2e_demo.py` 会新建验证知识库、上传带唯一标记的 PDF，并记录任务 ID、向量维度、三路召回、问答引用、工作流与审批结果，写入 [e2e-evidence.md](./e2e-evidence.md)。

## 6. 停止与清理

```bash
docker compose --env-file .env -f deploy/docker-compose.yml down        # 停止，保留数据卷
docker compose --env-file .env -f deploy/docker-compose.yml down -v     # 连同数据一起删除
```

示例数据可在控制台删除：先删「示例知识库」中的文档（级联清理向量与图谱），再删知识库本身。

## 常见问题

- **菜单看不到审批/审计/API Key**：页面按权限与开关显示，见 [用户使用指南 · 常见问题](./user-guide.md#5-常见问题)。
- **上传后解析失败**：检查 `MINERU_TARGET`；没有 MinerU 时可先上传 Markdown/文本验证链路。
- **问答没有引用**：确认 `RAG_LLM_*` 或 `LLAMAINDEX_LLM_*` 已配置；检索结果为空时先上传文档。
- **端口冲突**：修改 `.env` 的 `GATEWAY_PUBLISH_PORT` / `POSTGRES_PUBLISH_PORT` 后重启。
