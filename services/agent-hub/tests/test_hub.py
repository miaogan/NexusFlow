from __future__ import annotations

from typing import Any

from nexusflow_agents.core.hub import Hub
from nexusflow_agents.core.registry import (
    CAPABILITY_INTENT,
    AgentRegistry,
    AgentSpec,
)


class _Executor:
    def __init__(self, fail: bool = False) -> None:
        self.fail = fail
        self.calls = 0

    def call(
        self,
        spec: AgentSpec,
        capability: str,
        payload: dict[str, Any],
        ctx: dict[str, Any],
    ) -> dict[str, Any]:
        self.calls += 1
        if self.fail:
            raise RuntimeError(f"{spec.name} unavailable")
        return {"agent": spec.name, "payload": payload}

    def probe(self, spec: AgentSpec) -> bool:
        return not self.fail


def _registry() -> AgentRegistry:
    registry = AgentRegistry()
    registry.register(
        AgentSpec(
            name="primary",
            capabilities=frozenset({CAPABILITY_INTENT}),
            kind="local",
            priority=10,
        )
    )
    registry.register(
        AgentSpec(
            name="backup",
            capabilities=frozenset({CAPABILITY_INTENT}),
            kind="local",
            priority=20,
        )
    )
    return registry


def test_hub_fails_over_to_next_agent() -> None:
    registry = _registry()
    primary = _Executor(fail=True)
    backup = _Executor()
    hub = Hub(registry, {"local": _FailFirst(primary, backup)}, max_attempts=2)

    result = hub.call(CAPABILITY_INTENT, {"message": "hi"})

    assert result.ok is True
    assert result.agent == "backup"
    assert result.attempts == 2
    assert result.data == {"agent": "backup", "payload": {"message": "hi"}}


class _FailFirst:
    """Routes the first call to a failing executor, then to a healthy one."""

    def __init__(self, first: _Executor, second: _Executor) -> None:
        self._first = first
        self._second = second

    def call(
        self,
        spec: AgentSpec,
        capability: str,
        payload: dict[str, Any],
        ctx: dict[str, Any],
    ) -> dict[str, Any]:
        if spec.name == "primary":
            return self._first.call(spec, capability, payload, ctx)
        return self._second.call(spec, capability, payload, ctx)

    def probe(self, spec: AgentSpec) -> bool:
        return True


def test_hub_returns_degraded_when_all_agents_fail() -> None:
    hub = Hub(_registry(), {"local": _Executor(fail=True)}, max_attempts=2)

    result = hub.call(CAPABILITY_INTENT, {"message": "hi"})

    assert result.ok is False
    assert result.degraded is True
    assert result.attempts == 2
    assert "unavailable" in result.error


def test_hub_returns_degraded_without_agents() -> None:
    result = Hub(AgentRegistry(), {}).call(CAPABILITY_INTENT, {})

    assert result.ok is False
    assert result.degraded is True
    assert "no healthy agent" in result.error


def test_call_skill_routes_to_tenant_agent() -> None:
    registry = AgentRegistry()
    registry.register(
        AgentSpec(
            name="shared.translate",
            capabilities=frozenset({"agent.call"}),
            kind="local",
            skills=frozenset({"translate"}),
            priority=100,
        )
    )
    registry.register(
        AgentSpec(
            name="tenant.translate",
            capabilities=frozenset({"agent.call"}),
            kind="local",
            skills=frozenset({"translate"}),
            priority=10,
            tenant_id="t1",
        )
    )
    hub = Hub(registry, {"local": _Executor()})

    own = hub.call_skill("translate", {"text": "hi"}, {"tenant_id": "t1"})
    shared = hub.call_skill("translate", {"text": "hi"}, {"tenant_id": "t2"})
    missing = hub.call_skill("unknown", {}, {"tenant_id": "t1"})

    assert own.ok is True and own.agent == "tenant.translate"
    assert shared.ok is True and shared.agent == "shared.translate"
    assert missing.ok is False and missing.degraded is True
