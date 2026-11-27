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


def _tenant_spec(
    name: str, tenant_id: str, capability: str, priority: int = 100
) -> AgentSpec:
    return AgentSpec(
        name=name,
        capabilities=frozenset({capability}),
        kind="http",
        endpoint="http://127.0.0.1:9000",
        priority=priority,
        tenant_id=tenant_id,
    )


def test_resolve_only_returns_global_and_own_tenant() -> None:
    registry = AgentRegistry()
    registry.register(_tenant_spec("shared", "", CAPABILITY_INTENT, priority=10))
    registry.register(_tenant_spec("a-only", "tenant-a", CAPABILITY_INTENT))
    registry.register(_tenant_spec("b-only", "tenant-b", CAPABILITY_INTENT))

    assert [s.name for s in registry.resolve(CAPABILITY_INTENT, "tenant-a")] == [
        "shared",
        "a-only",
    ]
    assert [s.name for s in registry.resolve(CAPABILITY_INTENT, "tenant-b")] == [
        "shared",
        "b-only",
    ]
    assert [s.name for s in registry.resolve(CAPABILITY_INTENT)] == ["shared"]
    assert [s.name for s in registry.resolve(CAPABILITY_INTENT, "tenant-c")] == [
        "shared"
    ]


def test_tenant_agent_wins_priority_tie() -> None:
    registry = AgentRegistry()
    registry.register(_tenant_spec("shared", "", CAPABILITY_INTENT, priority=10))
    registry.register(_tenant_spec("custom", "tenant-a", CAPABILITY_INTENT, priority=10))

    specs = registry.resolve(CAPABILITY_INTENT, "tenant-a")

    assert [s.name for s in specs] == ["custom", "shared"]


def test_skill_resolution_is_tenant_scoped() -> None:
    registry = AgentRegistry()
    registry.register(
        AgentSpec(
            name="shared.translate",
            capabilities=frozenset({"agent.call"}),
            kind="http",
            endpoint="http://shared:9000",
            skills=frozenset({"translate"}),
            priority=100,
        )
    )
    registry.register(
        AgentSpec(
            name="tenant.assistant",
            capabilities=frozenset({"agent.call"}),
            kind="http",
            endpoint="http://tenant:9000",
            skills=frozenset({"translate", "expense.approval"}),
            priority=10,
            tenant_id="t1",
        )
    )

    assert [s.name for s in registry.resolve_skill("translate", "t1")] == [
        "tenant.assistant",
        "shared.translate",
    ]
    assert [s.name for s in registry.resolve_skill("expense.approval", "t1")] == [
        "tenant.assistant"
    ]
    assert registry.resolve_skill("expense.approval", "t2") == []

    skills = {item["skill"]: item for item in registry.available_skills("t1")}
    assert set(skills) == {"translate", "expense.approval"}
    assert skills["translate"]["agent"] == "tenant.assistant"
    assert registry.available_skills("t2") == [
        {
            "skill": "translate",
            "agent": "shared.translate",
            "tenant_id": "",
            "capabilities": ["agent.call"],
            "priority": 100,
        }
    ]


def test_tenant_lookup_and_delete_are_scoped() -> None:
    registry = AgentRegistry()
    registry.register(_tenant_spec("shared", "", CAPABILITY_SUMMARY))
    registry.register(_tenant_spec("custom", "tenant-a", CAPABILITY_SUMMARY))

    assert registry.get("custom", "tenant-a").tenant_id == "tenant-a"
    assert registry.get("custom", "tenant-b") is None
    assert registry.deregister("custom", "tenant-b") is False
    assert registry.deregister("custom", "tenant-a") is True
    assert registry.get("shared", "tenant-a").name == "shared"

    snapshot = registry.snapshot("tenant-a")
    assert {item["name"] for item in snapshot} == {"shared"}
    assert snapshot[0]["tenant_id"] == ""
