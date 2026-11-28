from __future__ import annotations

import logging
import threading
import time
import uuid
from dataclasses import dataclass, field
from datetime import UTC, datetime, timedelta
from functools import lru_cache
from typing import Any

from nexusflow_agents.core.config import get_hub_settings

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
        self._write(event)
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

    def _write(self, event: AuditEvent) -> None:
        with self._lock:
            self._events.append(event)
            if len(self._events) > self._max:
                self._events = self._events[-self._max :]

    def list(
        self,
        limit: int = 100,
        *,
        tenant_id: str = "",
        action: str = "",
        decision: str = "",
        since: float = 0.0,
        until: float = 0.0,
    ) -> list[dict[str, Any]]:
        with self._lock:
            events = list(reversed(self._events))
        out = [
            event.snapshot()
            for event in events
            if (not tenant_id or event.tenant_id == tenant_id)
            and (not action or event.action == action)
            and (not decision or event.decision == decision)
            and (since <= 0 or event.ts >= since)
            and (until <= 0 or event.ts <= until)
        ]
        return out[: max(1, min(limit, 500))]

    def clear(self) -> None:
        with self._lock:
            self._events.clear()


class PostgresAuditLog(AuditLog):
    """Compliance audit trail in the shared PostgreSQL ``audit_logs`` table.

    Writes are best-effort: an unavailable audit database must not fail the
    request being audited, so failures are logged and dropped.

    New tables are created with monthly RANGE partitions on ``created_at``;
    partitions are created ahead of time and old ones dropped once the whole
    month falls outside ``retention_days`` (0 keeps everything). Legacy
    non-partitioned tables keep working and fall back to deleting expired rows.
    """

    MAINTENANCE_INTERVAL_SECONDS = 3600

    def __init__(self, dsn: str, retention_days: int = 0) -> None:
        self._dsn = dsn
        self._retention_days = max(0, retention_days)
        self._partitioned = False
        self._maintenance_lock = threading.Lock()
        self._last_maintenance = time.monotonic()
        self._init_schema()

    def _connect(self):  # pragma: no cover - real database only
        import psycopg
        from psycopg.rows import dict_row

        return psycopg.connect(self._dsn, row_factory=dict_row)

    def _init_schema(self) -> None:  # pragma: no cover - real database only
        with self._connect() as conn:
            conn.execute("SELECT pg_advisory_xact_lock(731003)")
            relkind = _table_relkind(conn, "audit_logs")
            if relkind is None:
                conn.execute(
                    """
                    CREATE TABLE audit_logs (
                        id               VARCHAR(64) NOT NULL,
                        tenant_id        VARCHAR(64) NOT NULL,
                        user_id          VARCHAR(64) NOT NULL,
                        action           VARCHAR(64) NOT NULL,
                        resource_type    VARCHAR(32) NOT NULL,
                        resource_id      VARCHAR(64) DEFAULT '',
                        client_ip        VARCHAR(64) DEFAULT '',
                        trace_id         VARCHAR(64) DEFAULT '',
                        pii_masked_count INT DEFAULT 0,
                        status_code      INT NOT NULL,
                        metadata         JSONB DEFAULT '{}'::jsonb,
                        created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
                        PRIMARY KEY (id, created_at)
                    ) PARTITION BY RANGE (created_at)
                    """
                )
                self._partitioned = True
            else:
                self._partitioned = relkind == "p"
                if not self._partitioned:
                    logger.warning(
                        "audit_logs is not partitioned; monthly retention falls "
                        "back to DELETE and needs a manual migration"
                    )
            conn.execute(
                """
                CREATE INDEX IF NOT EXISTS idx_audit_logs_tenant_time
                ON audit_logs (tenant_id, created_at DESC)
                """
            )
            conn.execute(
                """
                CREATE INDEX IF NOT EXISTS idx_audit_logs_action
                ON audit_logs (tenant_id, action)
                """
            )
            self._maintain(conn)

    def _maintain(self, conn) -> None:  # pragma: no cover - real database only
        cutoff = None
        if self._retention_days > 0:
            cutoff = datetime.now(UTC) - timedelta(days=self._retention_days)
        if self._partitioned:
            month = _month_start(datetime.now(UTC))
            _ensure_partition(conn, month)
            _ensure_partition(conn, _next_month(month))
            if cutoff is None:
                return
            for name in _partition_names(conn):
                part_month = _partition_month(name)
                if part_month is not None and _next_month(part_month) <= cutoff:
                    conn.execute(f'DROP TABLE IF EXISTS "{name}"')
                    logger.info("audit partition dropped name=%s", name)
        elif cutoff is not None:
            conn.execute("DELETE FROM audit_logs WHERE created_at < %s", (cutoff,))

    def _maybe_maintain(self) -> None:
        if time.monotonic() - self._last_maintenance < self.MAINTENANCE_INTERVAL_SECONDS:
            return
        self._run_maintenance()

    def _run_maintenance(self, *, force: bool = False) -> None:
        with self._maintenance_lock:
            if (
                not force
                and time.monotonic() - self._last_maintenance
                < self.MAINTENANCE_INTERVAL_SECONDS
            ):
                return
            try:
                with self._connect() as conn:
                    self._maintain(conn)
                self._last_maintenance = time.monotonic()
            except Exception as exc:  # noqa: BLE001 - best-effort housekeeping
                logger.warning("audit maintenance failed: %s", exc)

    def _write(self, event: AuditEvent) -> None:
        self._maybe_maintain()
        try:
            self._insert(event)
        except Exception as exc:  # noqa: BLE001 - retry once after refreshing
            logger.debug("audit insert failed, refreshing partitions: %s", exc)
            # Crossing a month boundary may miss the partition for the new
            # month; refresh partitions once and retry before dropping the event.
            self._run_maintenance(force=True)
            try:
                self._insert(event)
            except Exception as exc:  # noqa: BLE001 - auditing must not break requests
                logger.warning("audit persistence failed: %s", exc)

    def _insert(self, event: AuditEvent) -> None:  # pragma: no cover - real database only
        from psycopg.types.json import Jsonb

        metadata = {
            "risk": event.risk,
            "decision": event.decision,
            "status": event.status,
            "detail": event.detail,
        }
        with self._connect() as conn:
            conn.execute(
                """
                INSERT INTO audit_logs (
                    id, tenant_id, user_id, action, resource_type,
                    resource_id, trace_id, pii_masked_count, status_code,
                    metadata, created_at
                ) VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s)
                """,
                (
                    event.id,
                    event.tenant_id or "default",
                    event.user_id or "anonymous",
                    event.action,
                    _resource_type(event.action),
                    event.resource,
                    event.trace_id,
                    _pii_masked_count(event.detail),
                    _status_code(event.decision, event.status),
                    Jsonb(metadata),
                    datetime.fromtimestamp(event.ts, tz=UTC),
                ),
            )

    def list(
        self,
        limit: int = 100,
        *,
        tenant_id: str = "",
        action: str = "",
        decision: str = "",
        since: float = 0.0,
        until: float = 0.0,
    ) -> list[dict[str, Any]]:
        clauses: list[str] = []
        params: list[Any] = []
        if tenant_id:
            clauses.append("tenant_id = %s")
            params.append(tenant_id)
        if action:
            clauses.append("action = %s")
            params.append(action)
        if decision:
            clauses.append("metadata->>'decision' = %s")
            params.append(decision)
        if since > 0:
            clauses.append("created_at >= to_timestamp(%s)")
            params.append(since)
        if until > 0:
            clauses.append("created_at <= to_timestamp(%s)")
            params.append(until)
        sql = (
            "SELECT id, tenant_id, user_id, action, resource_id, trace_id, "
            "status_code, metadata, created_at FROM audit_logs"
        )
        if clauses:
            sql += " WHERE " + " AND ".join(clauses)
        sql += " ORDER BY created_at DESC LIMIT %s"
        params.append(max(1, min(limit, 500)))
        try:
            with self._connect() as conn:
                rows = conn.execute(sql, tuple(params)).fetchall()
        except Exception as exc:  # noqa: BLE001 - keep the console usable
            logger.warning("audit query failed: %s", exc)
            return []
        return [_row_to_event(row) for row in rows]

    def clear(self) -> None:
        """Test helper: removes every persisted event."""
        with self._connect() as conn:
            conn.execute("DELETE FROM audit_logs")


PARTITION_PREFIX = "audit_logs_p"


def _table_relkind(conn, name: str) -> str | None:  # pragma: no cover - real database only
    """Return ``'p'`` for partitioned, ``'r'`` for regular, ``None`` if absent."""
    row = conn.execute(
        """
        SELECT c.relkind::text AS relkind
        FROM pg_class c
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = current_schema() AND c.relname = %s
        """,
        (name,),
    ).fetchone()
    return str(row["relkind"]) if row else None


def _month_start(ts: datetime) -> datetime:
    return ts.replace(day=1, hour=0, minute=0, second=0, microsecond=0)


def _next_month(ts: datetime) -> datetime:
    return _month_start(ts.replace(day=28) + timedelta(days=4))


def _partition_name(month: datetime) -> str:
    return f"{PARTITION_PREFIX}{month.year:04d}{month.month:02d}"


def _partition_month(name: str) -> datetime | None:
    if not name.startswith(PARTITION_PREFIX):
        return None
    digits = name[len(PARTITION_PREFIX) :]
    if len(digits) != 6 or not digits.isdigit():
        return None
    try:
        return datetime(int(digits[:4]), int(digits[4:]), 1, tzinfo=UTC)
    except ValueError:
        return None


def _ensure_partition(conn, month: datetime) -> None:  # pragma: no cover - real database only
    start = month.astimezone(UTC).isoformat()
    end = _next_month(month).astimezone(UTC).isoformat()
    conn.execute(
        f'CREATE TABLE IF NOT EXISTS "{_partition_name(month)}" '
        f"PARTITION OF audit_logs FOR VALUES FROM ('{start}') TO ('{end}')"
    )


def _partition_names(conn) -> list[str]:  # pragma: no cover - real database only
    rows = conn.execute(
        """
        SELECT child.relname AS name
        FROM pg_inherits i
        JOIN pg_class parent ON parent.oid = i.inhparent
        JOIN pg_namespace pn ON pn.oid = parent.relnamespace
        JOIN pg_class child ON child.oid = i.inhrelid
        WHERE parent.relname = 'audit_logs' AND pn.nspname = current_schema()
        """
    ).fetchall()
    return [str(row["name"]) for row in rows]


def _resource_type(action: str) -> str:
    prefix = action.split(".", 1)[0]
    if prefix in {"tool", "approval", "agent", "workflow"}:
        return prefix
    return "agent"


def _pii_masked_count(detail: dict[str, Any]) -> int:
    value = detail.get("pii_masked_count", 0)
    return value if isinstance(value, int) and value >= 0 else 0


def _status_code(decision: str, status: str) -> int:
    if status in {"error", "failed"}:
        return 500
    if decision in {"denied", "rejected"}:
        return 403
    if decision == "pending":
        return 202
    return 200


def _row_to_event(row: dict[str, Any]) -> dict[str, Any]:
    metadata = row.get("metadata") or {}
    created = row.get("created_at")
    return {
        "id": str(row["id"]),
        "ts": created.timestamp() if created else 0.0,
        "action": str(row["action"]),
        "tenant_id": str(row.get("tenant_id") or ""),
        "user_id": str(row.get("user_id") or ""),
        "trace_id": str(row.get("trace_id") or ""),
        "resource": str(row.get("resource_id") or ""),
        "risk": str(metadata.get("risk") or "read"),
        "decision": str(metadata.get("decision") or ""),
        "status": str(metadata.get("status") or ""),
        "detail": dict(metadata.get("detail") or {}),
    }


@lru_cache
def get_audit() -> AuditLog:
    settings = get_hub_settings()
    if settings.audit_driver == "postgres" and settings.state_database_url:
        try:
            return PostgresAuditLog(
                settings.state_database_url,
                retention_days=settings.audit_retention_days,
            )
        except Exception as exc:  # noqa: BLE001 - keep the hub available
            logger.warning("audit database unavailable, using memory: %s", exc)
    return AuditLog()
