from __future__ import annotations

from typing import Literal

from pydantic import BaseModel, Field

SummaryStrategy = Literal["llm", "extractive"]


class HistoryMessage(BaseModel):
    role: Literal["user", "assistant"]
    content: str = Field(default="", max_length=8000)


class SummaryRequest(BaseModel):
    previous_summary: str = Field(default="", max_length=8000)
    messages: list[HistoryMessage] = Field(default_factory=list)
    budget_chars: int | None = Field(default=None, ge=100, le=8000)


class SummaryResult(BaseModel):
    summary: str
    strategy: SummaryStrategy = "extractive"
    dropped_messages: int = 0
    budget_chars: int = 1200


class HealthResponse(BaseModel):
    status: str
    llm_enabled: bool
