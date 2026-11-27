from __future__ import annotations

from typing import Any

from pydantic import BaseModel, Field


class CallEnvelope(BaseModel):
    """Identity, tracing and budget context shared by agents, tools and MCP."""

    tenant_id: str = "default"
    user_id: str = "anonymous"
    trace_id: str = ""
    request_id: str = ""
    approval_id: str = ""
    deadline_ms: int = Field(default=0, ge=0)
    budget: dict[str, Any] = Field(default_factory=dict)

    def context(self) -> dict[str, Any]:
        return {
            "tenant_id": self.tenant_id,
            "user_id": self.user_id,
            "trace_id": self.trace_id,
            "request_id": self.request_id,
            "approval_id": self.approval_id,
            "deadline_ms": self.deadline_ms,
            "budget": self.budget,
        }
