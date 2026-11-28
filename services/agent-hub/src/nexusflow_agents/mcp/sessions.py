from __future__ import annotations

import logging
import threading
from dataclasses import dataclass, field
from functools import lru_cache
from typing import Any, Protocol

from nexusflow_agents.core.config import get_hub_settings

logger = logging.getLogger(__name__)


class SessionStore(Protocol):
    """Session metadata behind the streamable HTTP transport.

    The live SSE response queues are inherently process-local, but session
    existence, tenant, subscriptions and fingerprints live here so any replica
    can accept follow-up requests for a session created elsewhere.
    """

    def create(
        self, session_id: str, tenant_id: str = "", list_fingerprint: str = ""
    ) -> None: ...

    def exists(self, session_id: str) -> bool: ...

    def tenant(self, session_id: str) -> str: ...

    def count(self) -> int: ...

    def delete(self, session_id: str) -> None: ...

    def subscribe(self, session_id: str, uri: str, fingerprint: str) -> None: ...

    def unsubscribe(self, session_id: str, uri: str) -> None: ...

    def subscriptions(self, session_id: str) -> dict[str, str]: ...

    def set_fingerprint(self, session_id: str, uri: str, fingerprint: str) -> None: ...

    def list_fingerprint(self, session_id: str) -> str: ...

    def set_list_fingerprint(self, session_id: str, fingerprint: str) -> None: ...

    def clear(self) -> None: ...


@dataclass
class _MemorySession:
    tenant_id: str = ""
    subscriptions: dict[str, str] = field(default_factory=dict)
    list_fingerprint: str = ""


class MemorySessionStore:
    """Process-local sessions; single-replica development default."""

    def __init__(self) -> None:
        self._lock = threading.RLock()
        self._sessions: dict[str, _MemorySession] = {}

    def create(
        self, session_id: str, tenant_id: str = "", list_fingerprint: str = ""
    ) -> None:
        with self._lock:
            self._sessions[session_id] = _MemorySession(
                tenant_id=tenant_id, list_fingerprint=list_fingerprint
            )

    def exists(self, session_id: str) -> bool:
        with self._lock:
            return session_id in self._sessions

    def tenant(self, session_id: str) -> str:
        with self._lock:
            session = self._sessions.get(session_id)
            return session.tenant_id if session else ""

    def count(self) -> int:
        with self._lock:
            return len(self._sessions)

    def delete(self, session_id: str) -> None:
        with self._lock:
            self._sessions.pop(session_id, None)

    def subscribe(self, session_id: str, uri: str, fingerprint: str) -> None:
        with self._lock:
            session = self._sessions.get(session_id)
            if session is not None:
                session.subscriptions[uri] = fingerprint

    def unsubscribe(self, session_id: str, uri: str) -> None:
        with self._lock:
            session = self._sessions.get(session_id)
            if session is not None:
                session.subscriptions.pop(uri, None)

    def subscriptions(self, session_id: str) -> dict[str, str]:
        with self._lock:
            session = self._sessions.get(session_id)
            return dict(session.subscriptions) if session else {}

    def set_fingerprint(self, session_id: str, uri: str, fingerprint: str) -> None:
        self.subscribe(session_id, uri, fingerprint)

    def list_fingerprint(self, session_id: str) -> str:
        with self._lock:
            session = self._sessions.get(session_id)
            return session.list_fingerprint if session else ""

    def set_list_fingerprint(self, session_id: str, fingerprint: str) -> None:
        with self._lock:
            session = self._sessions.get(session_id)
            if session is not None:
                session.list_fingerprint = fingerprint

    def clear(self) -> None:
        with self._lock:
            self._sessions.clear()


class PostgresSessionStore:
    """Shared ``mcp_sessions`` table so replicas see each other's sessions.

    Sessions idle longer than ``ttl_seconds`` are pruned on creation. Database
    errors degrade to "session not found" and are logged rather than failing
    every MCP request.
    """

    def __init__(self, dsn: str, ttl_seconds: int = 86400) -> None:
        self._dsn = dsn
        self._ttl_seconds = max(60, ttl_seconds)
        self._init_schema()

    def _connect(self):  # pragma: no cover - real database only
        import psycopg
        from psycopg.rows import dict_row

        return psycopg.connect(self._dsn, row_factory=dict_row)

    def _init_schema(self) -> None:  # pragma: no cover - real database only
        with self._connect() as conn:
            conn.execute("SELECT pg_advisory_xact_lock(731005)")
            conn.execute(
                """
                CREATE TABLE IF NOT EXISTS mcp_sessions (
                    session_id       VARCHAR(64) PRIMARY KEY,
                    tenant_id        VARCHAR(64) NOT NULL DEFAULT '',
                    subscriptions    JSONB NOT NULL DEFAULT '{}'::jsonb,
                    list_fingerprint VARCHAR(64) NOT NULL DEFAULT '',
                    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
                )
                """
            )
            conn.execute(
                """
                CREATE INDEX IF NOT EXISTS idx_mcp_sessions_updated
                ON mcp_sessions (updated_at)
                """
            )

    def create(
        self, session_id: str, tenant_id: str = "", list_fingerprint: str = ""
    ) -> None:  # pragma: no cover - real database only
        try:
            with self._connect() as conn:
                conn.execute(
                    "DELETE FROM mcp_sessions "
                    "WHERE updated_at < NOW() - make_interval(secs => %s)",
                    (self._ttl_seconds,),
                )
                conn.execute(
                    """
                    INSERT INTO mcp_sessions (
                        session_id, tenant_id, list_fingerprint
                    ) VALUES (%s, %s, %s)
                    ON CONFLICT (session_id) DO UPDATE
                    SET tenant_id = EXCLUDED.tenant_id, updated_at = NOW()
                    """,
                    (session_id, tenant_id, list_fingerprint),
                )
        except Exception as exc:  # noqa: BLE001 - degrade to a missing session
            logger.warning("mcp session create failed: %s", exc)

    def exists(self, session_id: str) -> bool:  # pragma: no cover - real database only
        try:
            with self._connect() as conn:
                row = conn.execute(
                    "UPDATE mcp_sessions SET updated_at = NOW() "
                    "WHERE session_id = %s RETURNING 1 AS ok",
                    (session_id,),
                ).fetchone()
            return row is not None
        except Exception as exc:  # noqa: BLE001 - treat as unknown session
            logger.warning("mcp session lookup failed: %s", exc)
            return False

    def tenant(self, session_id: str) -> str:  # pragma: no cover - real database only
        try:
            with self._connect() as conn:
                row = conn.execute(
                    "SELECT tenant_id FROM mcp_sessions WHERE session_id = %s",
                    (session_id,),
                ).fetchone()
            return str(row["tenant_id"]) if row else ""
        except Exception as exc:  # noqa: BLE001 - empty tenant is safe
            logger.warning("mcp session tenant lookup failed: %s", exc)
            return ""

    def count(self) -> int:  # pragma: no cover - real database only
        try:
            with self._connect() as conn:
                row = conn.execute("SELECT COUNT(*) AS total FROM mcp_sessions").fetchone()
            return int(row["total"]) if row else 0
        except Exception as exc:  # noqa: BLE001 - do not block new sessions
            logger.warning("mcp session count failed: %s", exc)
            return 0

    def delete(self, session_id: str) -> None:  # pragma: no cover - real database only
        try:
            with self._connect() as conn:
                conn.execute(
                    "DELETE FROM mcp_sessions WHERE session_id = %s", (session_id,)
                )
        except Exception as exc:  # noqa: BLE001 - best-effort cleanup
            logger.warning("mcp session delete failed: %s", exc)

    def subscribe(
        self, session_id: str, uri: str, fingerprint: str
    ) -> None:  # pragma: no cover - real database only
        try:
            with self._connect() as conn:
                conn.execute(
                    """
                    UPDATE mcp_sessions
                    SET subscriptions = subscriptions || jsonb_build_object(%s, %s),
                        updated_at = NOW()
                    WHERE session_id = %s
                    """,
                    (uri, fingerprint, session_id),
                )
        except Exception as exc:  # noqa: BLE001 - subscription is best-effort
            logger.warning("mcp session subscribe failed: %s", exc)

    def unsubscribe(
        self, session_id: str, uri: str
    ) -> None:  # pragma: no cover - real database only
        try:
            with self._connect() as conn:
                conn.execute(
                    """
                    UPDATE mcp_sessions
                    SET subscriptions = subscriptions - %s, updated_at = NOW()
                    WHERE session_id = %s
                    """,
                    (uri, session_id),
                )
        except Exception as exc:  # noqa: BLE001 - unsubscribe is best-effort
            logger.warning("mcp session unsubscribe failed: %s", exc)

    def subscriptions(
        self, session_id: str
    ) -> dict[str, str]:  # pragma: no cover - real database only
        try:
            with self._connect() as conn:
                row = conn.execute(
                    "SELECT subscriptions FROM mcp_sessions WHERE session_id = %s",
                    (session_id,),
                ).fetchone()
            raw: Any = row["subscriptions"] if row else {}
            return {str(key): str(value) for key, value in dict(raw or {}).items()}
        except Exception as exc:  # noqa: BLE001 - no subscriptions on error
            logger.warning("mcp session subscriptions failed: %s", exc)
            return {}

    def set_fingerprint(
        self, session_id: str, uri: str, fingerprint: str
    ) -> None:  # pragma: no cover - real database only
        self.subscribe(session_id, uri, fingerprint)

    def list_fingerprint(
        self, session_id: str
    ) -> str:  # pragma: no cover - real database only
        try:
            with self._connect() as conn:
                row = conn.execute(
                    "SELECT list_fingerprint FROM mcp_sessions WHERE session_id = %s",
                    (session_id,),
                ).fetchone()
            return str(row["list_fingerprint"]) if row else ""
        except Exception as exc:  # noqa: BLE001 - empty fingerprint is safe
            logger.warning("mcp session fingerprint failed: %s", exc)
            return ""

    def set_list_fingerprint(
        self, session_id: str, fingerprint: str
    ) -> None:  # pragma: no cover - real database only
        try:
            with self._connect() as conn:
                conn.execute(
                    """
                    UPDATE mcp_sessions
                    SET list_fingerprint = %s, updated_at = NOW()
                    WHERE session_id = %s
                    """,
                    (fingerprint, session_id),
                )
        except Exception as exc:  # noqa: BLE001 - dedupe is best-effort
            logger.warning("mcp session fingerprint update failed: %s", exc)

    def clear(self) -> None:  # pragma: no cover - test helper
        try:
            with self._connect() as conn:
                conn.execute("DELETE FROM mcp_sessions")
        except Exception as exc:  # noqa: BLE001 - test helper
            logger.warning("mcp session clear failed: %s", exc)


@lru_cache
def get_session_store() -> SessionStore:
    settings = get_hub_settings()
    if settings.mcp_session_driver == "postgres" and settings.state_database_url:
        try:
            return PostgresSessionStore(
                settings.state_database_url,
                ttl_seconds=settings.mcp_session_ttl_seconds,
            )
        except Exception as exc:  # noqa: BLE001 - keep MCP available
            logger.warning("mcp session database unavailable, using memory: %s", exc)
    return MemorySessionStore()
