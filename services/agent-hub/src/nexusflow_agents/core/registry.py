from __future__ import annotations

import threading
import time
from dataclasses import dataclass
from typing import Any, Literal

CAPABILITY_INTENT = "intent.classify"
CAPABILITY_SUMMARY = "summary.compact"
CAPABILITY_MEMORY_RECALL = "memory.recall"
CAPABILITY_MEMORY_EXTRACT = "memory.extract"

VALID_CAPABILITIES = {
    CAPABILITY_INTENT,
    CAPABILITY_SUMMARY,
    CAPABILITY_MEMORY_RECALL,
    CAPABILITY_MEMORY_EXTRACT,
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


@dataclass
class AgentState:
    spec: AgentSpec
    failures: int = 0
    healthy: bool = True
    expires_at: float | None = None


class AgentRegistry:
    """In-process registry of star-topology spokes (local or remote)."""

    def __init__(self) -> None:
        self._lock = threading.RLock()
        self._agents: dict[str, AgentState] = {}

    def register(self, spec: AgentSpec) -> None:
        expires_at = (
            time.monotonic() + spec.ttl_seconds if spec.ttl_seconds > 0 else None
        )
        with self._lock:
            self._agents[spec.name] = AgentState(spec=spec, expires_at=expires_at)

    def deregister(self, name: str) -> bool:
        with self._lock:
            return self._agents.pop(name, None) is not None

    def heartbeat(self, name: str, ttl_seconds: int | None = None) -> bool:
        with self._lock:
            state = self._agents.get(name)
            if state is None:
                return False
            ttl = state.spec.ttl_seconds if ttl_seconds is None else ttl_seconds
            if ttl > 0:
                state.expires_at = time.monotonic() + ttl
            return True

    def mark_success(self, name: str) -> None:
        with self._lock:
            state = self._agents.get(name)
            if state is not None:
                state.failures = 0
                state.healthy = True

    def mark_failure(self, name: str, threshold: int) -> None:
        with self._lock:
            state = self._agents.get(name)
            if state is None:
                return
            state.failures += 1
            if state.failures >= max(1, threshold):
                state.healthy = False

    def prune_expired(self, now: float | None = None) -> list[str]:
        current = time.monotonic() if now is None else now
        removed: list[str] = []
        with self._lock:
            for name, state in list(self._agents.items()):
                if state.expires_at is not None and state.expires_at <= current:
                    removed.append(name)
                    del self._agents[name]
        return removed

    def resolve(self, capability: str) -> list[AgentSpec]:
        self.prune_expired()
        with self._lock:
            specs = [
                state.spec
                for state in self._agents.values()
                if state.healthy and capability in state.spec.capabilities
            ]
        specs.sort(key=lambda spec: (spec.priority, spec.name))
        return specs

    def snapshot(self) -> list[dict[str, Any]]:
        now = time.monotonic()
        with self._lock:
            return [
                {
                    "name": state.spec.name,
                    "kind": state.spec.kind,
                    "capabilities": sorted(state.spec.capabilities),
                    "endpoint": state.spec.endpoint,
                    "priority": state.spec.priority,
                    "healthy": state.healthy,
                    "failures": state.failures,
                    "ttl_seconds": state.spec.ttl_seconds,
                    "expires_in": (
                        round(state.expires_at - now, 1)
                        if state.expires_at is not None
                        else None
                    ),
                }
                for state in self._agents.values()
            ]

    def by_kind(self, kind: str) -> list[AgentSpec]:
        with self._lock:
            return [
                state.spec
                for state in self._agents.values()
                if state.spec.kind == kind
            ]

    def clear(self) -> None:
        with self._lock:
            self._agents.clear()
