from __future__ import annotations

from typing import Any

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from nexusflow_agents.api import admin as admin_api
from nexusflow_agents.core.agent_store import InMemoryAgentStore
from nexusflow_agents.core.config import HubSettings
from nexusflow_agents.core.registry import (
    CAPABILITY_INTENT,
    AgentRegistry,
    AgentSpec,
)


def _body(name: str = "custom.agent", capability: str = "agent.call") -> dict[str, Any]:
    return {
        "name": name,
        "capabilities": [capability],
        "endpoint": "http://127.0.0.1:9100/v1/call",
        "timeout_ms": 8000,
        "priority": 5,
        "risk": "read",
        "tenant_scope": "t1",
    }


@pytest.fixture
def client(monkeypatch: pytest.MonkeyPatch) -> tuple[TestClient, AgentRegistry, InMemoryAgentStore]:
    registry = AgentRegistry()
    registry.register(
        AgentSpec(
            name="local.intent",
            capabilities=frozenset({CAPABILITY_INTENT}),
            kind="local",
        )
    )
    store = InMemoryAgentStore()
    monkeypatch.setattr(
        admin_api,
        "get_hub_settings",
        lambda: HubSettings(_env_file=None, dynamic_registration=True),
    )
    monkeypatch.setattr(admin_api, "get_registry", lambda: registry)
    monkeypatch.setattr(admin_api, "get_agent_store", lambda: store)
    app = FastAPI()
    app.include_router(admin_api.router)
    return TestClient(app), registry, store


def test_custom_agent_crud_is_tenant_scoped(
    client: tuple[TestClient, AgentRegistry, InMemoryAgentStore],
) -> None:
    http, registry, store = client
    response = http.post(
        "/v1/agents",
        json=_body(),
        headers={"x-tenant-id": "t1", "x-user-id": "u1"},
    )
    assert response.status_code == 201
    assert response.json()["tenant_id"] == "t1"
    assert store.list("t1")[0].name == "custom.agent"
    assert registry.get("custom.agent", "t1") is not None

    names_t1 = {
        item["name"]
        for item in http.get("/v1/agents", headers={"x-tenant-id": "t1"}).json()["agents"]
    }
    assert names_t1 == {"local.intent", "custom.agent"}

    names_t2 = {
        item["name"]
        for item in http.get("/v1/agents", headers={"x-tenant-id": "t2"}).json()["agents"]
    }
    assert names_t2 == {"local.intent"}

    assert (
        http.get("/v1/agents/custom.agent", headers={"x-tenant-id": "t1"}).status_code
        == 200
    )
    assert (
        http.get("/v1/agents/custom.agent", headers={"x-tenant-id": "t2"}).status_code
        == 404
    )

    deleted = http.delete(
        "/v1/agents/custom.agent", headers={"x-tenant-id": "t1"}
    )
    assert deleted.status_code == 200
    assert deleted.json()["found"] is True
    assert store.list("t1") == []
    assert registry.get("custom.agent", "t1") is None
    assert registry.get("local.intent", "t1") is not None


def test_custom_agent_rejects_reserved_name_and_bad_capability(
    client: tuple[TestClient, AgentRegistry, InMemoryAgentStore],
) -> None:
    http, _, _ = client
    headers = {"x-tenant-id": "t1"}

    reserved = http.post(
        "/v1/agents", json=_body(name="local.intent"), headers=headers
    )
    assert reserved.status_code == 422

    invalid = http.post(
        "/v1/agents", json=_body(capability="unknown.capability"), headers=headers
    )
    assert invalid.status_code == 422


def test_custom_agent_writes_require_dynamic_registration(
    client: tuple[TestClient, AgentRegistry, InMemoryAgentStore],
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    http, _, _ = client
    monkeypatch.setattr(
        admin_api,
        "get_hub_settings",
        lambda: HubSettings(_env_file=None, dynamic_registration=False),
    )

    assert (
        http.post(
            "/v1/agents", json=_body(), headers={"x-tenant-id": "t1"}
        ).status_code
        == 403
    )


def test_custom_agent_persist_failure_does_not_register(
    client: tuple[TestClient, AgentRegistry, InMemoryAgentStore],
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    http, registry, _ = client

    class BrokenStore(InMemoryAgentStore):
        def upsert(self, spec: AgentSpec, created_by: str = "") -> None:
            raise RuntimeError("db down")

    monkeypatch.setattr(admin_api, "get_agent_store", lambda: BrokenStore())

    response = http.post(
        "/v1/agents", json=_body(), headers={"x-tenant-id": "t1"}
    )

    assert response.status_code == 500
    assert registry.get("custom.agent", "t1") is None
