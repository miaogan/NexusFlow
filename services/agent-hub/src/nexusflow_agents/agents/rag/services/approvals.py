from __future__ import annotations

import logging
import threading
import time
import uuid
from collections.abc import Callable
from dataclasses import dataclass, field
from datetime import UTC, datetime
from functools import lru_cache
from typing import Any, Literal

from nexusflow_agents.core.audit import get_audit
from nexusflow_agents.core.config import get_hub_settings

logger = logging.getLogger(__name__)

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


class PostgresApprovalService(ApprovalService):
    """Durable approval tickets shared by every hub replica.

    The in-memory service cannot be reused across replicas: a request created
    on one pod would be invisible to the pod that receives the decision. Rows
    live in ``agent_approvals`` (Hub-specific; workflow approvals use
    ``workflow_approvals`` in the Go engine).
    """

    def __init__(self, dsn: str) -> None:
        self._dsn = dsn
        self._ttl = 600
        self._init_schema()

    def _connect(self):  # pragma: no cover - real database only
        import psycopg
        from psycopg.rows import dict_row

        return psycopg.connect(self._dsn, row_factory=dict_row)

    def _init_schema(self) -> None:  # pragma: no cover - real database only
        with self._connect() as conn:
            conn.execute("SELECT pg_advisory_xact_lock(731004)")
            conn.execute(
                """
                CREATE TABLE IF NOT EXISTS agent_approvals (
                    id          VARCHAR(64) PRIMARY KEY,
                    tenant_id   VARCHAR(64) NOT NULL,
                    tool        VARCHAR(128) NOT NULL,
                    args        JSONB NOT NULL DEFAULT '{}'::jsonb,
                    risk        VARCHAR(16) NOT NULL DEFAULT 'read',
                    user_id     VARCHAR(64) NOT NULL DEFAULT '',
                    trace_id    VARCHAR(64) NOT NULL DEFAULT '',
                    status      VARCHAR(16) NOT NULL DEFAULT 'pending',
                    reason      TEXT NOT NULL DEFAULT '',
                    decided_by  VARCHAR(64) NOT NULL DEFAULT '',
                    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
                    decided_at  TIMESTAMPTZ,
                    expires_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
                )
                """
            )
            conn.execute(
                """
                CREATE INDEX IF NOT EXISTS agent_approvals_pending_idx
                ON agent_approvals (tenant_id, status, created_at DESC)
                """
            )

    def create(
        self,
        tool: str,
        args: dict[str, Any],
        risk: str,
        ctx: dict[str, Any] | None = None,
        ttl_seconds: int | None = None,
    ) -> ApprovalRecord:
        from psycopg.types.json import Jsonb

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
        with self._connect() as conn:
            conn.execute(
                """
                INSERT INTO agent_approvals (
                    id, tenant_id, tool, args, risk, user_id, trace_id,
                    status, created_at, expires_at
                ) VALUES (%s, %s, %s, %s, %s, %s, %s, 'pending', %s, %s)
                """,
                (
                    record.id,
                    record.tenant_id,
                    record.tool,
                    Jsonb(record.args),
                    record.risk,
                    record.user_id,
                    record.trace_id,
                    _utc(record.created_at),
                    _utc(record.expires_at),
                ),
            )
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
        with self._connect() as conn:
            row = conn.execute(
                """
                SELECT * FROM agent_approvals
                WHERE id = %s AND expires_at > NOW()
                """,
                (approval_id,),
            ).fetchone()
        return _record_from_row(row) if row else None

    def list(self, status: str = "", tenant_id: str = "") -> list[dict[str, Any]]:
        clauses = ["expires_at > NOW()"]
        params: list[Any] = []
        if status:
            clauses.append("status = %s")
            params.append(status)
        if tenant_id:
            clauses.append("tenant_id = %s")
            params.append(tenant_id)
        sql = "SELECT * FROM agent_approvals WHERE " + " AND ".join(clauses)
        sql += " ORDER BY created_at DESC"
        with self._connect() as conn:
            rows = conn.execute(sql, tuple(params)).fetchall()
        return [_record_from_row(row).snapshot() for row in rows]

    def decide(
        self,
        approval_id: str,
        decision: Decision,
        *,
        decided_by: str,
        reason: str = "",
        tenant_id: str = "",
    ) -> ApprovalRecord:
        status = "approved" if decision == "approve" else "rejected"
        with self._connect() as conn:
            row = conn.execute(
                """
                UPDATE agent_approvals SET
                    status = %s, reason = %s, decided_by = %s, decided_at = NOW()
                WHERE id = %s AND status = 'pending' AND expires_at > NOW()
                  AND (%s = '' OR tenant_id = %s)
                RETURNING *
                """,
                (status, reason, decided_by, approval_id, tenant_id, tenant_id),
            ).fetchone()
            if row is None:
                existing = conn.execute(
                    "SELECT tenant_id, status, expires_at FROM agent_approvals WHERE id = %s",
                    (approval_id,),
                ).fetchone()
                if (
                    existing is None
                    or (tenant_id and existing["tenant_id"] != tenant_id)
                    or _is_expired(existing)
                ):
                    raise ApprovalNotFound(approval_id)
                raise ApprovalConflict(f"approval already {existing['status']}")
        record = _record_from_row(row)
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
        with self._connect() as conn:
            row = conn.execute(
                """
                SELECT 1 FROM agent_approvals
                WHERE id = %s AND tool = %s AND status = 'approved'
                  AND expires_at > NOW()
                """,
                (approval_id, tool),
            ).fetchone()
        return row is not None

    def clear(self) -> None:
        """Test helper: removes every persisted ticket."""
        with self._connect() as conn:
            conn.execute("DELETE FROM agent_approvals")


def _utc(ts: float) -> datetime:
    return datetime.fromtimestamp(ts, tz=UTC)


def _epoch(value: Any) -> float:
    return value.timestamp() if isinstance(value, datetime) else 0.0


def _is_expired(row: dict[str, Any]) -> bool:
    expires = row.get("expires_at")
    return isinstance(expires, datetime) and expires.timestamp() <= time.time()


def _record_from_row(row: dict[str, Any]) -> ApprovalRecord:
    return ApprovalRecord(
        id=str(row["id"]),
        tool=str(row["tool"]),
        args=dict(row.get("args") or {}),
        risk=str(row.get("risk") or "read"),
        tenant_id=str(row.get("tenant_id") or ""),
        user_id=str(row.get("user_id") or ""),
        trace_id=str(row.get("trace_id") or ""),
        status=str(row.get("status") or "pending"),  # type: ignore[arg-type]
        reason=str(row.get("reason") or ""),
        decided_by=str(row.get("decided_by") or ""),
        created_at=_epoch(row.get("created_at")),
        decided_at=_epoch(row.get("decided_at")),
        expires_at=_epoch(row.get("expires_at")),
    )


@lru_cache
def get_approvals() -> ApprovalService:
    settings = get_hub_settings()
    if settings.approval_driver == "postgres" and settings.state_database_url:
        try:
            return PostgresApprovalService(settings.state_database_url)
        except Exception as exc:  # noqa: BLE001 - keep the hub available
            logger.warning("approval database unavailable, using memory: %s", exc)
    return ApprovalService()


def approval_checker() -> Callable[[str, str], bool]:
    service = get_approvals()
    return service.is_approved
