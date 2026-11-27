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


def test_sandbox_agent_is_stored_with_redacted_listing(
    client: tuple[TestClient, AgentRegistry, InMemoryAgentStore],
) -> None:
    http, registry, store = client
    body = {
        "name": "sandbox.agent",
        "capabilities": ["agent.call"],
        "kind": "sandbox",
        "sandbox": {
            "code": "print(input())",
            "runtime": "python",
            "timeout_seconds": 5,
            "packages": ["numpy"],
        },
    }

    response = http.post(
        "/v1/agents", json=body, headers={"x-tenant-id": "t1"}
    )

    assert response.status_code == 201
    spec = registry.get("sandbox.agent", "t1")
    assert spec is not None
    assert spec.kind == "sandbox"
    assert spec.risk == "execute"
    assert spec.sandbox["code"] == "print(input())"
    assert store.list("t1")[0].sandbox["runtime"] == "python"

    item = next(
        agent
        for agent in http.get(
            "/v1/agents", headers={"x-tenant-id": "t1"}
        ).json()["agents"]
        if agent["name"] == "sandbox.agent"
    )
    assert item["sandbox"]["configured"] is True
    assert item["sandbox"]["code_bytes"] > 0
    assert "code" not in item["sandbox"]


def test_sandbox_metadata_update_keeps_existing_code(
    client: tuple[TestClient, AgentRegistry, InMemoryAgentStore],
) -> None:
    http, registry, _ = client
    created = {
        "name": "sandbox.agent",
        "capabilities": ["agent.call"],
        "kind": "sandbox",
        "sandbox": {"code": "print('keep me')", "runtime": "python"},
    }
    assert (
        http.post(
            "/v1/agents", json=created, headers={"x-tenant-id": "t1"}
        ).status_code
        == 201
    )

    updated = {
        "name": "sandbox.agent",
        "capabilities": ["agent.call"],
        "kind": "sandbox",
        "skills": ["translate"],
        "sandbox": {"runtime": "python", "timeout_seconds": 7},
    }
    response = http.post(
        "/v1/agents", json=updated, headers={"x-tenant-id": "t1"}
    )

    assert response.status_code == 201
    spec = registry.get("sandbox.agent", "t1")
    assert spec is not None
    assert spec.sandbox["code"] == "print('keep me')"
    assert spec.sandbox["timeout_seconds"] == 7
    assert spec.skills == frozenset({"translate"})


def test_sandbox_agent_rejects_missing_code(
    client: tuple[TestClient, AgentRegistry, InMemoryAgentStore],
) -> None:
    http, _, _ = client

    response = http.post(
        "/v1/agents",
        json={
            "name": "sandbox.agent",
            "capabilities": ["agent.call"],
            "kind": "sandbox",
            "sandbox": {"runtime": "python"},
        },
        headers={"x-tenant-id": "t1"},
    )

    assert response.status_code == 422


def test_http_agent_requires_endpoint(
    client: tuple[TestClient, AgentRegistry, InMemoryAgentStore],
) -> None:
    http, _, _ = client

    response = http.post(
        "/v1/agents",
        json={"name": "http.agent", "capabilities": ["agent.call"]},
        headers={"x-tenant-id": "t1"},
    )

    assert response.status_code == 422


def test_agent_skills_feed_dynamic_router(
    client: tuple[TestClient, AgentRegistry, InMemoryAgentStore],
) -> None:
    http, _, _ = client
    body = _body()
    body["skills"] = ["translate", "expense.approval"]

    response = http.post(
        "/v1/agents", json=body, headers={"x-tenant-id": "t1"}
    )

    assert response.status_code == 201
    skills = http.get(
        "/v1/skills", headers={"x-tenant-id": "t1"}
    ).json()["skills"]
    assert {item["skill"] for item in skills} == {"translate", "expense.approval"}
    assert {item["agent"] for item in skills} == {"custom.agent"}
    assert http.get("/v1/skills", headers={"x-tenant-id": "t2"}).json()["skills"] == []

    invalid = _body(name="bad.skills")
    invalid["skills"] = ["BAD SKILL"]
    assert (
        http.post("/v1/agents", json=invalid, headers={"x-tenant-id": "t1"}).status_code
        == 422
    )


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
