from __future__ import annotations

from typing import Literal

from pydantic import BaseModel, Field

MemoryKind = Literal["profile", "preference", "fact"]


class HistoryMessage(BaseModel):
    role: Literal["user", "assistant"]
    content: str = Field(default="", max_length=8000)


class MemoryItem(BaseModel):
    id: str = ""
    content: str
    kind: MemoryKind = "fact"
    score: float = 0.0


class SearchRequest(BaseModel):
    tenant_id: str = Field(min_length=1, max_length=128)
    user_id: str = Field(min_length=1, max_length=128)
    query: str = Field(min_length=1, max_length=8000)
    top_k: int | None = Field(default=None, ge=1, le=20)


class SearchResponse(BaseModel):
    memories: list[MemoryItem] = Field(default_factory=list)
    source: Literal["semantic", "lexical", "empty"] = "empty"


class ExtractRequest(BaseModel):
    tenant_id: str = Field(min_length=1, max_length=128)
    user_id: str = Field(min_length=1, max_length=128)
    messages: list[HistoryMessage] = Field(default_factory=list)


class ExtractResponse(BaseModel):
    created: int = 0
    merged: int = 0
    memories: list[MemoryItem] = Field(default_factory=list)


class HealthResponse(BaseModel):
    status: str
    llm_enabled: bool
    embedding_enabled: bool
    store: str
    vector_index: str = "none"
