# NexusFlow 全流程验证证据

> 由 `python scripts/e2e_demo.py --output docs/e2e-evidence.md` 生成；以下 ID 为本地环境实测值。

- 执行时间：2026-11-28 20:22:55
- Gateway：`http://127.0.0.1:18080`
- 验证知识库：`全流程验证`（`d1bf6a8d-85ee-47ca-8252-73dc7c407a38`）
- 上传文档：`e2e-NF-E2E-91b9ddb09b.pdf`
- 任务 ID：`52e67acd-c541-4a4b-8d7e-8ca9d2c3ca71`
- 解析结果：`COMPLETED`，layout：`default/d1bf6a8d-85ee-47ca-8252-73dc7c407a38/parsed/52e67acd-c541-4a4b-8d7e-8ca9d2c3ca71/layout.json`（641 字节）
- Qdrant：`1` 个点，向量维度 `[1024]`，租户 `['default']`
- 路由召回：vector=1，bm25=1，graph=1（retriever=lightrag）
- Gateway 混合检索：返回 `2` 条，recall_count=`3`
- 问答引用：`1` 条，命中上传文档：`True`
- 回答摘录：根据资料 [1]，文档中出现的验证标记是 **NF-E2E-91 b9ddb09b**（即“NexusFlow end to end evidence NF-E2E-91 b9ddb09b”中的标记部分）。  资料仅能确认该标记内容，未提供更多关于该文档或标记用途的说明。

## 检查项

| 结果 | 检查项 | 细节 |
| --- | --- | --- |
| PASS | admin login | status=200 |
| PASS | knowledge base ready | name=全流程验证 id=d1bf6a8d-85ee-47ca-8252-73dc7c407a38 |
| PASS | upload accepted | task=52e67acd-c541-4a4b-8d7e-8ca9d2c3ca71 |
| PASS | MinerU parse completed | status=COMPLETED |
| PASS | layout.json persisted | key=default/d1bf6a8d-85ee-47ca-8252-73dc7c407a38/parsed/52e67acd-c541-4a4b-8d7e-8ca9d2c3ca71/layout.json |
| PASS | layout.json readable | bytes=641 |
| PASS | Qdrant vectors written | points=1 dims=[1024] |
| PASS | vectors carry tenant | tenants=['default'] |
| PASS | llama vector route | chunks=1 |
| PASS | llama bm25 route | chunks=1 |
| PASS | graph route served by LightRAG | chunks=1 retriever=lightrag |
| PASS | gateway hybrid search | returned=2 recall_count=3 |
| PASS | RAG chat answers with citations | citations=1 hits_uploaded_doc=True |
| PASS | workflow loop run | run=run-1795868587159070849 status=COMPLETED |
| PASS | loop child runs persisted | children=3 |
| PASS | approval run suspends | run=run-1795868587181484430 |
| PASS | approval resumes to COMPLETED | status=COMPLETED |
| PASS | duplicate decision returns 409 | status=409 |
| PASS | MCP initialize | session=9aaef54e2e374499aea4af428a32a9d9 |
| PASS | MCP tools/list | tools=['search.query', 'workflow.run'] |
