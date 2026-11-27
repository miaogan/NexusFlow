from __future__ import annotations

import logging
import threading
import time
import uuid
from dataclasses import dataclass, field
from functools import lru_cache
from typing import Any

logger = logging.getLogger(__name__)


@dataclass
class AuditEvent:
    id: str
    ts: float
    action: str
    tenant_id: str = ""
    user_id: str = ""
    trace_id: str = ""
    resource: str = ""
    risk: str = "read"
    decision: str = "allowed"
    status: str = "ok"
    detail: dict[str, Any] = field(default_factory=dict)

    def snapshot(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "ts": self.ts,
            "action": self.action,
            "tenant_id": self.tenant_id,
            "user_id": self.user_id,
            "trace_id": self.trace_id,
            "resource": self.resource,
            "risk": self.risk,
            "decision": self.decision,
            "status": self.status,
            "detail": self.detail,
        }


class AuditLog:
    """Bounded in-memory audit trail for tools, approvals and workflow calls."""

    def __init__(self, max_entries: int = 2000) -> None:
        self._lock = threading.RLock()
        self._events: list[AuditEvent] = []
        self._max = max(64, max_entries)

    def record(
        self,
        action: str,
        *,
        tenant_id: str = "",
        user_id: str = "",
        trace_id: str = "",
        resource: str = "",
        risk: str = "read",
        decision: str = "allowed",
        status: str = "ok",
        detail: dict[str, Any] | None = None,
    ) -> AuditEvent:
        event = AuditEvent(
            id="aud-" + uuid.uuid4().hex[:12],
            ts=time.time(),
            action=action,
            tenant_id=tenant_id,
            user_id=user_id,
            trace_id=trace_id,
            resource=resource,
            risk=risk,
            decision=decision,
            status=status,
            detail=detail or {},
        )
        with self._lock:
            self._events.append(event)
            if len(self._events) > self._max:
                self._events = self._events[-self._max :]
        logger.info(
            "audit action=%s resource=%s decision=%s status=%s tenant=%s trace=%s",
            action,
            resource,
            decision,
            status,
            tenant_id,
            trace_id,
        )
        return event

    def list(
        self,
        limit: int = 100,
        *,
        tenant_id: str = "",
        action: str = "",
        decision: str = "",
    ) -> list[dict[str, Any]]:
        with self._lock:
            events = list(reversed(self._events))
        out = [
            event.snapshot()
            for event in events
            if (not tenant_id or event.tenant_id == tenant_id)
            and (not action or event.action == action)
            and (not decision or event.decision == decision)
        ]
        return out[: max(1, min(limit, 500))]

    def clear(self) -> None:
        with self._lock:
            self._events.clear()


@lru_cache
def get_audit() -> AuditLog:
    return AuditLog()
