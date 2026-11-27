from __future__ import annotations

import threading
import time
import uuid
from collections.abc import Callable
from dataclasses import dataclass, field
from functools import lru_cache
from typing import Any, Literal

from nexusflow_agents.core.audit import get_audit

Decision = Literal["approve", "reject"]


class ApprovalNotFound(KeyError):
    pass


class ApprovalConflict(RuntimeError):
    pass


@dataclass
class ApprovalRecord:
    id: str
    tool: str
    args: dict[str, Any]
    risk: str
    tenant_id: str
    user_id: str
    trace_id: str
    status: Literal["pending", "approved", "rejected"] = "pending"
    reason: str = ""
    decided_by: str = ""
    created_at: float = field(default_factory=time.time)
    decided_at: float = 0.0
    expires_at: float = 0.0

    def snapshot(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "tool": self.tool,
            "args": self.args,
            "risk": self.risk,
            "tenant_id": self.tenant_id,
            "user_id": self.user_id,
            "trace_id": self.trace_id,
            "status": self.status,
            "reason": self.reason,
            "decided_by": self.decided_by,
            "created_at": self.created_at,
            "decided_at": self.decided_at,
            "expires_at": self.expires_at,
        }


class ApprovalService:
    """In-memory approval checkpoints for high-risk tool calls.

    Records expire so a forgotten request cannot be replayed later. Only the
    first decision wins; a second decision is a conflict.
    """

    def __init__(self, ttl_seconds: int = 600, max_records: int = 2000) -> None:
        self._lock = threading.RLock()
        self._records: dict[str, ApprovalRecord] = {}
        self._ttl = max(30, ttl_seconds)
        self._max = max(16, max_records)

    def create(
        self,
        tool: str,
        args: dict[str, Any],
        risk: str,
        ctx: dict[str, Any] | None = None,
        ttl_seconds: int | None = None,
    ) -> ApprovalRecord:
        context = ctx or {}
        now = time.time()
        record = ApprovalRecord(
            id="apr-" + uuid.uuid4().hex[:16],
            tool=tool,
            args=dict(args),
            risk=risk,
            tenant_id=str(context.get("tenant_id") or "default"),
            user_id=str(context.get("user_id") or "anonymous"),
            trace_id=str(context.get("trace_id") or ""),
            created_at=now,
            expires_at=now + (ttl_seconds or self._ttl),
        )
        with self._lock:
            self._prune_locked(now)
            if len(self._records) >= self._max:
                oldest = min(self._records.values(), key=lambda r: r.created_at)
                self._records.pop(oldest.id, None)
            self._records[record.id] = record
        get_audit().record(
            "approval.create",
            tenant_id=record.tenant_id,
            user_id=record.user_id,
            trace_id=record.trace_id,
            resource=record.tool,
            risk=record.risk,
            decision="pending",
            status="created",
            detail={"approval_id": record.id},
        )
        return record

    def get(self, approval_id: str) -> ApprovalRecord | None:
        with self._lock:
            self._prune_locked()
            return self._records.get(approval_id)

    def list(self, status: str = "", tenant_id: str = "") -> list[dict[str, Any]]:
        with self._lock:
            self._prune_locked()
            return [
                record.snapshot()
                for record in sorted(
                    self._records.values(), key=lambda r: r.created_at, reverse=True
                )
                if (not status or record.status == status)
                and (not tenant_id or record.tenant_id == tenant_id)
            ]

    def decide(
        self,
        approval_id: str,
        decision: Decision,
        *,
        decided_by: str,
        reason: str = "",
        tenant_id: str = "",
    ) -> ApprovalRecord:
        with self._lock:
            self._prune_locked()
            record = self._records.get(approval_id)
            if record is None:
                raise ApprovalNotFound(approval_id)
            if tenant_id and record.tenant_id != tenant_id:
                raise ApprovalNotFound(approval_id)
            if record.status != "pending":
                raise ApprovalConflict(f"approval already {record.status}")
            record.status = "approved" if decision == "approve" else "rejected"
            record.reason = reason
            record.decided_by = decided_by
            record.decided_at = time.time()
        get_audit().record(
            "approval.decide",
            tenant_id=record.tenant_id,
            user_id=decided_by,
            trace_id=record.trace_id,
            resource=record.tool,
            risk=record.risk,
            decision=record.status,
            status="decided",
            detail={"approval_id": record.id, "reason": reason[:200]},
        )
        return record

    def is_approved(self, approval_id: str, tool: str) -> bool:
        if not approval_id:
            return False
        with self._lock:
            self._prune_locked()
            record = self._records.get(approval_id)
            return bool(
                record
                and record.status == "approved"
                and record.tool == tool
                and record.expires_at > time.time()
            )

    def _prune_locked(self, now: float | None = None) -> None:
        current = time.time() if now is None else now
        expired = [
            key for key, record in self._records.items() if record.expires_at <= current
        ]
        for key in expired:
            del self._records[key]

    def clear(self) -> None:
        with self._lock:
            self._records.clear()


@lru_cache
def get_approvals() -> ApprovalService:
    return ApprovalService()


def approval_checker() -> Callable[[str, str], bool]:
    service = get_approvals()
    return service.is_approved
