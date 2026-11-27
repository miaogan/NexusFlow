from __future__ import annotations

from nexusflow_agents.core.agent_store import (
    InMemoryAgentStore,
    build_agent_store,
    spec_from_row,
)
from nexusflow_agents.core.registry import AgentSpec


def _spec(name: str = "custom", tenant_id: str = "t1") -> AgentSpec:
    return AgentSpec(
        name=name,
        capabilities=frozenset({"agent.call"}),
        kind="http",
        endpoint="http://127.0.0.1:9100",
        timeout_ms=8000,
        priority=5,
        version="2.0",
        input_schema={"type": "object"},
        output_schema={"type": "object"},
        permissions=frozenset({"search:use"}),
        tenant_scope="t1",
        tools=frozenset({"search.query"}),
        skills=frozenset({"translate"}),
        model="gpt-4o-mini",
        max_turns=3,
        token_budget=4000,
        risk="read",
        tenant_id=tenant_id,
    )


def test_memory_store_is_tenant_scoped() -> None:
    store = InMemoryAgentStore()
    first = _spec("first", "t1")
    second = _spec("second", "t2")
    store.upsert(first)
    store.upsert(second)

    assert [spec.name for spec in store.list("t1")] == ["first"]
    assert [spec.name for spec in store.list("t2")] == ["second"]
    assert {spec.name for spec in store.list()} == {"first", "second"}

    assert store.delete("first", "t1") is True
    assert store.list("t1") == []
    assert store.delete("first", "t1") is False


def test_memory_store_upsert_replaces() -> None:
    store = InMemoryAgentStore()
    store.upsert(_spec("custom", "t1"))
    updated = AgentSpec(
        name="custom",
        capabilities=frozenset({"tool.call"}),
        kind="http",
        endpoint="http://127.0.0.1:9200",
        tenant_id="t1",
    )
    store.upsert(updated)

    specs = store.list("t1")
    assert len(specs) == 1
    assert specs[0].endpoint == "http://127.0.0.1:9200"
    assert specs[0].capabilities == frozenset({"tool.call"})


def test_build_agent_store_falls_back_to_memory() -> None:
    assert isinstance(build_agent_store("memory", ""), InMemoryAgentStore)
    assert isinstance(build_agent_store("postgres", ""), InMemoryAgentStore)


def test_bootstrap_loads_persisted_agents() -> None:
    from nexusflow_agents.core.bootstrap import _load_custom_agents
    from nexusflow_agents.core.registry import AgentRegistry

    store = InMemoryAgentStore()
    store.upsert(_spec("persisted", "t1"))
    registry = AgentRegistry()

    _load_custom_agents(registry, store)

    assert registry.get("persisted", "t1") is not None
    assert registry.get("persisted", "t2") is None


def test_bootstrap_ignores_store_failure() -> None:
    from nexusflow_agents.core.bootstrap import _load_custom_agents
    from nexusflow_agents.core.registry import AgentRegistry

    class BrokenStore(InMemoryAgentStore):
        def list(self, tenant_id: str | None = None) -> list[AgentSpec]:
            raise RuntimeError("db down")

    registry = AgentRegistry()
    _load_custom_agents(registry, BrokenStore())

    assert registry.snapshot() == []


def test_spec_from_row_maps_contract_fields() -> None:
    spec = spec_from_row(
        {
            "tenant_id": "t1",
            "name": "custom",
            "capabilities": ["agent.call", "tool.call"],
            "kind": "http",
            "endpoint": "http://127.0.0.1:9300",
            "timeout_ms": 9000,
            "priority": 7,
            "ttl_seconds": 0,
            "version": "3.0",
            "input_schema": {"type": "object"},
            "output_schema": {"type": "object"},
            "permissions": ["search:use"],
            "tenant_scope": "t1",
            "tools": ["search.query"],
            "skills": ["translate", "expense.approval"],
            "sandbox": {"code": "print(1)", "runtime": "python"},
            "model": "deepseek-chat",
            "max_turns": 4,
            "token_budget": 6000,
            "risk": "write",
        }
    )

    assert spec.capabilities == frozenset({"agent.call", "tool.call"})
    assert spec.permissions == frozenset({"search:use"})
    assert spec.tools == frozenset({"search.query"})
    assert spec.skills == frozenset({"translate", "expense.approval"})
    assert spec.sandbox["code"] == "print(1)"
    assert spec.tenant_id == "t1"
    assert spec.risk == "write"
    assert spec.kind == "http"
