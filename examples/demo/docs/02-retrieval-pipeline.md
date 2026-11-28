# 混合检索与问答链路

NexusFlow 的检索不是单路向量搜索，而是多路召回后融合。

## 召回路由

- **vector**：基于 embedding 的语义召回，存储在 Qdrant。
- **bm25**：稀疏向量与关键词召回，适合专有名词与编号。
- **filter**：按元数据（租户、知识库、任务、文件名）过滤。
- **parent**：命中子块后回溯父块，保留上下文。
- **graph**：图谱召回。启用 LightRAG 时使用真实实体关系图，否则使用内置共现图。

多路结果通过 RRF（Reciprocal Rank Fusion）融合，再按需调用重排序模型，最后截取 `final_top_n` 条返回。

## 检索请求示例

```json
{
  "query": "NexusFlow 如何融合多路召回？",
  "routes": [
    {"type": "vector", "top_k": 20},
    {"type": "bm25", "top_k": 20},
    {"type": "graph", "top_k": 10}
  ],
  "final_top_n": 5,
  "kb_id": "default"
}
```

返回的每个 `chunk` 都带有 `chunk_id`、`content`、`score` 与 `meta`。`meta.retriever` 标明来源（例如 `lightrag` 表示该片段由 LightRAG 图谱路由产出）。

## 问答与引用

`POST /v1/chat` 会在检索结果之上生成回答，并返回 `citations`。控制台会把引用编号与正文中的角标关联，可一键跳转查看原文。

回答不满意时可以提交反馈：

- `user.rating`：点赞/点踩。
- 带 `corrected_answer` 的反馈会进入“人工纠错”池，后续相似问题会优先参考纠错答案。

## 空知识库行为

对尚未上传任何文档的知识库执行检索会返回 `200` 与空 `chunks`，而不是报错。检索服务会区分“路由可用但无结果”和“所有路由都失败”两种情况。
