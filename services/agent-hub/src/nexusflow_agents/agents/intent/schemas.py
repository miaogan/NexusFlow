from __future__ import annotations

from typing import Literal

from pydantic import BaseModel, Field

IntentLabel = Literal["knowledge_qa", "chitchat", "clarify", "skill"]


class HistoryMessage(BaseModel):
    role: Literal["user", "assistant"]
    content: str = Field(default="", max_length=8000)


class IntentRequest(BaseModel):
    message: str = Field(min_length=1, max_length=8000)
    history: list[HistoryMessage] = Field(default_factory=list)
    # Tenant-visible skills available for dynamic routing.
    skills: list[str] = Field(default_factory=list, max_length=32)


class IntentResult(BaseModel):
    intent: IntentLabel
    confidence: float = Field(default=0.7, ge=0.0, le=1.0)
    need_retrieval: bool
    # Set only when intent == "skill"; must be one of the offered skills.
    skill: str | None = Field(default=None, max_length=64)
    reason: str = ""
    source: Literal["llm", "rules"] = "rules"


class HealthResponse(BaseModel):
    status: str
    llm_enabled: bool
