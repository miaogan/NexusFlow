from __future__ import annotations

import logging
import threading
from typing import Any, Protocol

from nexusflow_agents.core.registry import AgentSpec

logger = logging.getLogger(__name__)


class AgentStore(Protocol):
    """Persistence for tenant-scoped custom SubAgents."""

    def list(self, tenant_id: str | None = None) -> list[AgentSpec]: ...

    def upsert(self, spec: AgentSpec, created_by: str = "") -> None: ...

    def delete(self, name: str, tenant_id: str) -> bool: ...


class InMemoryAgentStore:
    """Process-local store used when no Postgres DSN is configured."""

    def __init__(self) -> None:
        self._lock = threading.RLock()
        self._agents: dict[tuple[str, str], AgentSpec] = {}

    def list(self, tenant_id: str | None = None) -> list[AgentSpec]:
        with self._lock:
            specs = [
                spec
                for (owner, _), spec in self._agents.items()
                if tenant_id is None or owner == tenant_id
            ]
        specs.sort(key=lambda spec: (spec.tenant_id, spec.name))
        return specs

    def upsert(self, spec: AgentSpec, created_by: str = "") -> None:
        with self._lock:
            self._agents[(spec.tenant_id, spec.name)] = spec

    def delete(self, name: str, tenant_id: str) -> bool:
        with self._lock:
            return self._agents.pop((tenant_id, name), None) is not None


class PostgresAgentStore:
    """Postgres-backed custom SubAgent registry with idempotent migration."""

    def __init__(self, dsn: str) -> None:
        self._dsn = dsn
        self._init_schema()

    def _connect(self):  # pragma: no cover - exercised against a real database
        import psycopg
        from psycopg.rows import dict_row

        return psycopg.connect(self._dsn, row_factory=dict_row)

    def _init_schema(self) -> None:  # pragma: no cover - real database only
        with self._connect() as conn:
            # Serialize concurrent replica startups so CREATE TABLE IF NOT
            # EXISTS cannot race on the system catalog.
            conn.execute("SELECT pg_advisory_xact_lock(731001)")
            conn.execute(
                """
                CREATE TABLE IF NOT EXISTS custom_agents (
                    tenant_id      VARCHAR(64) NOT NULL,
                    name           VARCHAR(64) NOT NULL,
                    capabilities   JSONB NOT NULL DEFAULT '[]'::jsonb,
                    kind           VARCHAR(16) NOT NULL DEFAULT 'http',
                    endpoint       VARCHAR(512) NOT NULL DEFAULT '',
                    timeout_ms     INTEGER NOT NULL DEFAULT 5000,
                    priority       INTEGER NOT NULL DEFAULT 100,
                    ttl_seconds    INTEGER NOT NULL DEFAULT 0,
                    version        VARCHAR(32) NOT NULL DEFAULT '1.0',
                    input_schema   JSONB NOT NULL DEFAULT '{}'::jsonb,
                    output_schema  JSONB NOT NULL DEFAULT '{}'::jsonb,
                    permissions    JSONB NOT NULL DEFAULT '[]'::jsonb,
                    tenant_scope   VARCHAR(128) NOT NULL DEFAULT '',
                    tools          JSONB NOT NULL DEFAULT '[]'::jsonb,
                    model          VARCHAR(128) NOT NULL DEFAULT '',
                    max_turns      INTEGER NOT NULL DEFAULT 0,
                    token_budget   INTEGER NOT NULL DEFAULT 0,
                    risk           VARCHAR(16) NOT NULL DEFAULT 'read',
                    created_by     VARCHAR(64) NOT NULL DEFAULT '',
                    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
                    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
                    PRIMARY KEY (tenant_id, name)
                )
                """
            )
            conn.execute(
                """
                CREATE INDEX IF NOT EXISTS custom_agents_tenant_idx
                ON custom_agents (tenant_id, updated_at DESC)
                """
            )

    def list(self, tenant_id: str | None = None) -> list[AgentSpec]:
        with self._connect() as conn:
            if tenant_id is None:
                rows = conn.execute(
                    "SELECT * FROM custom_agents ORDER BY tenant_id, name"
                ).fetchall()
            else:
                rows = conn.execute(
                    "SELECT * FROM custom_agents WHERE tenant_id = %s ORDER BY name",
                    (tenant_id,),
                ).fetchall()
        return [spec_from_row(row) for row in rows]

    def upsert(self, spec: AgentSpec, created_by: str = "") -> None:
        from psycopg.types.json import Jsonb

        with self._connect() as conn:
            conn.execute(
                """
                INSERT INTO custom_agents (
                    tenant_id, name, capabilities, kind, endpoint, timeout_ms,
                    priority, ttl_seconds, version, input_schema, output_schema,
                    permissions, tenant_scope, tools, model, max_turns,
                    token_budget, risk, created_by
                ) VALUES (
                    %s, %s, %s, %s, %s, %s,
                    %s, %s, %s, %s, %s,
                    %s, %s, %s, %s, %s,
                    %s, %s, %s
                )
                ON CONFLICT (tenant_id, name) DO UPDATE SET
                    capabilities = EXCLUDED.capabilities,
                    kind = EXCLUDED.kind,
                    endpoint = EXCLUDED.endpoint,
                    timeout_ms = EXCLUDED.timeout_ms,
                    priority = EXCLUDED.priority,
                    ttl_seconds = EXCLUDED.ttl_seconds,
                    version = EXCLUDED.version,
                    input_schema = EXCLUDED.input_schema,
                    output_schema = EXCLUDED.output_schema,
                    permissions = EXCLUDED.permissions,
                    tenant_scope = EXCLUDED.tenant_scope,
                    tools = EXCLUDED.tools,
                    model = EXCLUDED.model,
                    max_turns = EXCLUDED.max_turns,
                    token_budget = EXCLUDED.token_budget,
                    risk = EXCLUDED.risk,
                    updated_at = NOW()
                """,
                (
                    spec.tenant_id,
                    spec.name,
                    Jsonb(sorted(spec.capabilities)),
                    spec.kind,
                    spec.endpoint,
                    spec.timeout_ms,
                    spec.priority,
                    spec.ttl_seconds,
                    spec.version,
                    Jsonb(spec.input_schema),
                    Jsonb(spec.output_schema),
                    Jsonb(sorted(spec.permissions)),
                    spec.tenant_scope,
                    Jsonb(sorted(spec.tools)),
                    spec.model,
                    spec.max_turns,
                    spec.token_budget,
                    spec.risk,
                    created_by,
                ),
            )

    def delete(self, name: str, tenant_id: str) -> bool:
        with self._connect() as conn:
            result = conn.execute(
                "DELETE FROM custom_agents WHERE tenant_id = %s AND name = %s",
                (tenant_id, name),
            )
        return bool(result.rowcount)


def spec_from_row(row: dict[str, Any]) -> AgentSpec:
    capabilities = row.get("capabilities") or []
    permissions = row.get("permissions") or []
    tools = row.get("tools") or []
    return AgentSpec(
        name=str(row["name"]),
        capabilities=frozenset(str(item) for item in capabilities),
        kind="http",
        endpoint=str(row.get("endpoint") or ""),
        timeout_ms=int(row.get("timeout_ms") or 5000),
        priority=int(row.get("priority") or 100),
        ttl_seconds=int(row.get("ttl_seconds") or 0),
        version=str(row.get("version") or "1.0"),
        input_schema=dict(row.get("input_schema") or {}),
        output_schema=dict(row.get("output_schema") or {}),
        permissions=frozenset(str(item) for item in permissions),
        tenant_scope=str(row.get("tenant_scope") or ""),
        tools=frozenset(str(item) for item in tools),
        model=str(row.get("model") or ""),
        max_turns=int(row.get("max_turns") or 0),
        token_budget=int(row.get("token_budget") or 0),
        risk=str(row.get("risk") or "read"),  # type: ignore[arg-type]
        tenant_id=str(row["tenant_id"]),
    )


def build_agent_store(driver: str, dsn: str) -> AgentStore:
    """Returns a Postgres store when configured, otherwise memory.

    A broken DSN must not take chat down, so failures fall back to memory with
    a warning; custom SubAgents then only live for the process lifetime.
    """
    if driver != "postgres":
        return InMemoryAgentStore()
    if not dsn:
        logger.warning(
            "AGENT_HUB_REGISTRY_DRIVER=postgres without "
            "AGENT_HUB_REGISTRY_DATABASE_URL; using memory"
        )
        return InMemoryAgentStore()
    try:
        return PostgresAgentStore(dsn)
    except Exception as exc:  # noqa: BLE001 - keep the hub available
        logger.warning("agent registry database unavailable, using memory: %s", exc)
        return InMemoryAgentStore()
