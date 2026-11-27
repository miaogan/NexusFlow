from __future__ import annotations

import time

from nexusflow_agents.core.registry import (
    CAPABILITY_INTENT,
    CAPABILITY_SUMMARY,
    AgentRegistry,
    AgentSpec,
)


def _spec(name: str, capability: str, priority: int = 100, ttl: int = 0) -> AgentSpec:
    return AgentSpec(
        name=name,
        capabilities=frozenset({capability}),
        kind="local",
        priority=priority,
        ttl_seconds=ttl,
    )


def test_resolve_sorts_by_priority_and_skips_unhealthy() -> None:
    registry = AgentRegistry()
    registry.register(_spec("slow", CAPABILITY_INTENT, priority=20))
    registry.register(_spec("fast", CAPABILITY_INTENT, priority=10))
    registry.register(_spec("other", CAPABILITY_SUMMARY))

    assert [spec.name for spec in registry.resolve(CAPABILITY_INTENT)] == [
        "fast",
        "slow",
    ]

    registry.mark_failure("fast", threshold=3)
    registry.mark_failure("fast", threshold=3)
    registry.mark_failure("fast", threshold=3)
    assert [spec.name for spec in registry.resolve(CAPABILITY_INTENT)] == ["slow"]

    registry.mark_success("fast")
    assert registry.resolve(CAPABILITY_INTENT)[0].name == "fast"


def test_expired_registrations_are_pruned() -> None:
    registry = AgentRegistry()
    registry.register(_spec("dynamic", CAPABILITY_INTENT, ttl=1))

    assert registry.prune_expired(now=time.monotonic() + 2) == ["dynamic"]
    assert registry.resolve(CAPABILITY_INTENT) == []


def test_heartbeat_extends_ttl_and_deregister_removes() -> None:
    registry = AgentRegistry()
    registry.register(_spec("dynamic", CAPABILITY_SUMMARY, ttl=1))

    assert registry.heartbeat("dynamic", ttl_seconds=60) is True
    assert registry.prune_expired(now=time.monotonic() + 10) == []
    assert registry.deregister("dynamic") is True
    assert registry.snapshot() == []
