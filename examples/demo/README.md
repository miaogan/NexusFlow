# 示例数据

配合 [快速上手](../../docs/quickstart.md) 使用。

## 目录

```
examples/demo/
├── docs/                     # 可上传到知识库的示例文档
│   ├── 01-product-overview.md
│   ├── 02-retrieval-pipeline.md
│   └── 03-api-and-operations.md
└── workflows/                # 可直接提交给 /v1/workflows/run 的请求体
    ├── conditional-routing.json
    ├── loop-batch.json
    ├── approval-gate.json
    └── agent-intent-loop.json
```

## 一键导入

```bash
python scripts/seed_demo_data.py
```

脚本会创建「示例知识库」并上传 `docs/` 下的文档，同时生成：

- `demo-viewer / Demo@12345`（只读，`kb:viewer`）
- `demo-editor / Demo@12345`（可编辑，`kb:editor`）
- `demo-openai` API Key（明文只显示一次）

重复运行是安全的：已存在的知识库、文档、用户会被跳过，权限会重新确认。

## 工作流示例

工作流文件是完整的 `POST /v1/workflows/run` 请求体（含 `dag` 与 `input`）：

```bash
GATEWAY=http://127.0.0.1:18080
TOKEN=$(curl -s $GATEWAY/v1/auth/login -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin123"}' \
  | python -c 'import sys,json;print(json.load(sys.stdin)["access_token"])')

curl -s $GATEWAY/v1/workflows/run -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d @examples/demo/workflows/loop-batch.json
```

| 文件 | 演示内容 | 前置条件 |
| --- | --- | --- |
| `conditional-routing.json` | `set` + `condition` 条件分流，未选分支标记 skipped | 无 |
| `loop-batch.json` | `loop` 并行子 DAG，子运行可按 `parent_run_id` 查询 | 无 |
| `approval-gate.json` | `human_approval` 挂起，批准后恢复；重复决策返回 409 | `HITL_ENABLED=true` |
| `agent-intent-loop.json` | 循环调用 `intent.classify` | `WORKFLOW_AGENT_NODE=true`，且意图 LLM 可用 |

审批示例：

```bash
# 运行后返回 202 与 run_id
curl -s $GATEWAY/v1/workflows/run -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d @examples/demo/workflows/approval-gate.json

curl -s -X POST $GATEWAY/v1/workflows/runs/<run_id>/approve \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"decision":"approve","reason":"demo"}'
```

## 清理

在控制台「文件管理」删除示例文档（级联清理对象存储、向量与 LightRAG 状态），再删除「示例知识库」。删除知识库本身不会自动清理其历史文档。
