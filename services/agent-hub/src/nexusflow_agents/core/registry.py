from __future__ import annotations

import threading
import time
from dataclasses import dataclass, field
from typing import Any, Literal

CAPABILITY_INTENT = "intent.classify"
CAPABILITY_SUMMARY = "summary.compact"
CAPABILITY_MEMORY_RECALL = "memory.recall"
CAPABILITY_MEMORY_EXTRACT = "memory.extract"
CAPABILITY_AGENT_CALL = "agent.call"
CAPABILITY_TOOL_CALL = "tool.call"

VALID_CAPABILITIES = {
    CAPABILITY_INTENT,
    CAPABILITY_SUMMARY,
    CAPABILITY_MEMORY_RECALL,
    CAPABILITY_MEMORY_EXTRACT,
    CAPABILITY_AGENT_CALL,
    CAPABILITY_TOOL_CALL,
}


@dataclass(frozen=True)
class AgentSpec:
    name: str
    capabilities: frozenset[str]
    kind: Literal["local", "http"]
    endpoint: str = ""
    timeout_ms: int = 5000
    priority: int = 100
    ttl_seconds: int = 0
    # Standard agent contract: schema, version and scope used by /v1/call,
    # dynamic registration and MCP exposure.
    version: str = "1.0"
    input_schema: dict[str, Any] = field(default_factory=dict)
    output_schema: dict[str, Any] = field(default_factory=dict)
    permissions: frozenset[str] = frozenset()
    tenant_scope: str = ""
    tools: frozenset[str] = frozenset()
    model: str = ""
    max_turns: int = 0
    token_budget: int = 0
    risk: Literal["read", "write", "execute", "external"] = "read"
    # Empty means shared/global (built-in agents); custom SubAgents set the
    # owning tenant so a spoke is only visible to that tenant.
    tenant_id: str = ""


@dataclass
class AgentState:
    spec: AgentSpec
    failures: int = 0
    healthy: bool = True
    expires_at: float | None = None


class AgentRegistry:
    """In-process registry of star-topology spokes (local or remote).

    Entries are keyed by ``(tenant_id, name)``. Built-in agents use an empty
    tenant (shared by everyone); custom SubAgents are only resolved for their
    owning tenant.
    """

    def __init__(self) -> None:
        self._lock = threading.RLock()
        self._agents: dict[tuple[str, str], AgentState] = {}

    def register(self, spec: AgentSpec) -> None:
        expires_at = (
            time.monotonic() + spec.ttl_seconds if spec.ttl_seconds > 0 else None
        )
        with self._lock:
            self._agents[(spec.tenant_id, spec.name)] = AgentState(
                spec=spec, expires_at=expires_at
            )

    def _lookup(self, name: str, tenant_id: str) -> AgentState | None:
        state = self._agents.get((tenant_id, name))
        if state is None and tenant_id:
            state = self._agents.get(("", name))
        return state

    def get(self, name: str, tenant_id: str = "") -> AgentSpec | None:
        with self._lock:
            state = self._lookup(name, tenant_id)
            return state.spec if state is not None else None

    def deregister(self, name: str, tenant_id: str = "") -> bool:
        with self._lock:
            return self._agents.pop((tenant_id, name), None) is not None

    def heartbeat(
        self, name: str, ttl_seconds: int | None = None, tenant_id: str = ""
    ) -> bool:
        with self._lock:
            state = self._lookup(name, tenant_id)
            if state is None:
                return False
            ttl = state.spec.ttl_seconds if ttl_seconds is None else ttl_seconds
            if ttl > 0:
                state.expires_at = time.monotonic() + ttl
            return True

    def mark_success(self, name: str, tenant_id: str = "") -> None:
        with self._lock:
            state = self._lookup(name, tenant_id)
            if state is not None:
                state.failures = 0
                state.healthy = True

    def mark_failure(self, name: str, threshold: int, tenant_id: str = "") -> None:
        with self._lock:
            state = self._lookup(name, tenant_id)
            if state is None:
                return
            state.failures += 1
            if state.failures >= max(1, threshold):
                state.healthy = False

    def prune_expired(self, now: float | None = None) -> list[str]:
        current = time.monotonic() if now is None else now
        removed: list[str] = []
        with self._lock:
            for key, state in list(self._agents.items()):
                if state.expires_at is not None and state.expires_at <= current:
                    removed.append(state.spec.name)
                    del self._agents[key]
        return removed

    def resolve(self, capability: str, tenant_id: str = "") -> list[AgentSpec]:
        self.prune_expired()
        with self._lock:
            specs = [
                state.spec
                for state in self._agents.values()
                if state.healthy
                and capability in state.spec.capabilities
                and state.spec.tenant_id in {"", tenant_id}
            ]
        # A tenant-specific agent wins ties against the shared built-in.
        specs.sort(
            key=lambda spec: (
                spec.priority,
                0 if spec.tenant_id == tenant_id else 1,
                spec.name,
            )
        )
        return specs

    def snapshot(self, tenant_id: str | None = None) -> list[dict[str, Any]]:
        now = time.monotonic()
        with self._lock:
            states = [
                state
                for state in self._agents.values()
                if tenant_id is None or state.spec.tenant_id in {"", tenant_id}
            ]
            return [
                {
                    "name": state.spec.name,
                    "tenant_id": state.spec.tenant_id,
                    "kind": state.spec.kind,
                    "capabilities": sorted(state.spec.capabilities),
                    "endpoint": state.spec.endpoint,
                    "priority": state.spec.priority,
                    "healthy": state.healthy,
                    "failures": state.failures,
                    "ttl_seconds": state.spec.ttl_seconds,
                    "version": state.spec.version,
                    "risk": state.spec.risk,
                    "model": state.spec.model,
                    "tools": sorted(state.spec.tools),
                    "tenant_scope": state.spec.tenant_scope,
                    "input_schema": state.spec.input_schema,
                    "output_schema": state.spec.output_schema,
                    "permissions": sorted(state.spec.permissions),
                    "expires_in": (
                        round(state.expires_at - now, 1)
                        if state.expires_at is not None
                        else None
                    ),
                }
                for state in states
            ]

    def by_kind(
        self, kind: str, tenant_id: str | None = None
    ) -> list[AgentSpec]:
        with self._lock:
            return [
                state.spec
                for state in self._agents.values()
                if state.spec.kind == kind
                and (tenant_id is None or state.spec.tenant_id in {"", tenant_id})
            ]

    def clear(self) -> None:
        with self._lock:
            self._agents.clear()
