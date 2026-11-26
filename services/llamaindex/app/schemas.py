from typing import Literal

from pydantic import BaseModel, Field


class IndexRequest(BaseModel):
    task_id: str
    markdown: str
    split_strategy: Literal["semantic", "markdown", "fixed"] = "markdown"
    embedding_model: str | None = None
    vector_store: str | None = None
    metadata: dict[str, str] = Field(default_factory=dict)


class IndexResponse(BaseModel):
    success: bool
    chunk_count: int = 0
    indexed_count: int = 0
    collection: str = ""
    error_msg: str = ""


class RecallRoute(BaseModel):
    type: Literal["vector", "bm25", "filter", "parent", "graph"] = "vector"
    top_k: int = 20
    filter_expr: str | None = None


class SearchRequest(BaseModel):
    query: str
    routes: list[RecallRoute] = Field(default_factory=list)
    rerank_model: str | None = None
    final_top_n: int = 5
    kb_id: str = "default"
    filters: dict[str, str] = Field(default_factory=dict)


class RetrievedChunk(BaseModel):
    chunk_id: str
    content: str
    score: float = 0.0
    meta: dict[str, str] = Field(default_factory=dict)


class SearchResponse(BaseModel):
    chunks: list[RetrievedChunk] = Field(default_factory=list)
    trace_id: str
    recall_count: int = 0
    rerank_applied: bool = False


class FeedbackEvent(BaseModel):
    trace_id: str
    query: str
    answer: str = ""
    cited_chunk_ids: list[str] = Field(default_factory=list)
    rating: int = 0
    corrected_answer: str = ""
    faithfulness: float | None = None
    ts: int = 0
    meta: dict[str, str] = Field(default_factory=dict)
