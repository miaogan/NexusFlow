from __future__ import annotations

from nexusflow_agents.core.bootstrap import build_registry
from nexusflow_agents.core.config import HubSettings
from nexusflow_agents.core.registry import (
    CAPABILITY_INTENT,
    CAPABILITY_MEMORY_RECALL,
    CAPABILITY_SUMMARY,
)


def test_all_local_registers_three_agents() -> None:
    registry = build_registry(HubSettings(role="all", mode="local"))

    names = {spec["name"] for spec in registry.snapshot()}
    assert names == {"local.intent", "local.summary", "local.memory"}


def test_split_role_registers_only_its_capability() -> None:
    registry = build_registry(HubSettings(role="memory", mode="local"))

    assert [spec["name"] for spec in registry.snapshot()] == ["local.memory"]
    assert registry.resolve(CAPABILITY_MEMORY_RECALL)
    assert registry.resolve(CAPABILITY_INTENT) == []
    assert registry.resolve(CAPABILITY_SUMMARY) == []


def test_remote_mode_registers_http_agents() -> None:
    registry = build_registry(HubSettings(role="rag", mode="remote"))

    specs = registry.snapshot()
    assert {spec["kind"] for spec in specs} == {"http"}
    assert {spec["name"] for spec in specs} == {
        "remote.intent",
        "remote.summary",
        "remote.memory",
    }


def test_hybrid_prefer_remote_orders_remote_first() -> None:
    registry = build_registry(
        HubSettings(role="rag", mode="hybrid", prefer="remote")
    )

    assert registry.resolve(CAPABILITY_INTENT)[0].name == "remote.intent"
