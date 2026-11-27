from __future__ import annotations

from typing import Literal

from pydantic import BaseModel, Field

IntentLabel = Literal["knowledge_qa", "chitchat", "clarify"]


class HistoryMessage(BaseModel):
    role: Literal["user", "assistant"]
    content: str = Field(default="", max_length=8000)


class IntentRequest(BaseModel):
    message: str = Field(min_length=1, max_length=8000)
    history: list[HistoryMessage] = Field(default_factory=list)


class IntentResult(BaseModel):
    intent: IntentLabel
    confidence: float = Field(default=0.7, ge=0.0, le=1.0)
    need_retrieval: bool
    reason: str = ""
    source: Literal["llm", "rules"] = "rules"


class HealthResponse(BaseModel):
    status: str
    llm_enabled: bool
