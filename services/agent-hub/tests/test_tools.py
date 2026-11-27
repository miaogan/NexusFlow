from __future__ import annotations

import time
from typing import Any

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from nexusflow_agents.api import call as call_api
from nexusflow_agents.core.config import HubSettings
from nexusflow_agents.core.envelope import CallEnvelope
from nexusflow_agents.core.registry import AgentRegistry, AgentSpec
from nexusflow_agents.core.tooling import (
    ApprovalRequired,
    ToolError,
    ToolRegistry,
    ToolService,
    ToolSpec,
    validate_payload,
)


def _service() -> ToolService:
    service = ToolService(ToolRegistry())

    def echo(payload: dict[str, Any], ctx: dict[str, Any]) -> dict[str, Any]:
        return {"echo": payload.get("text"), "tenant": ctx.get("tenant_id")}

    service.register(
        ToolSpec(
            name="demo.echo",
            description="echo",
            input_schema={
                "type": "object",
                "required": ["text"],
                "properties": {"text": {"type": "string"}},
                "additionalProperties": False,
            },
        ),
        echo,
    )
    service.register(
        ToolSpec(
            name="demo.sandbox",
            description="danger",
            input_schema={"type": "object", "properties": {}},
            risk="execute",
            requires_approval=True,
        ),
        lambda payload, ctx: {"ran": True},
    )
    return service


def test_validate_payload_rejects_missing_and_unknown_fields() -> None:
    schema = {
        "type": "object",
        "required": ["query"],
        "properties": {"query": {"type": "string"}},
        "additionalProperties": False,
    }
    with pytest.raises(ToolError, match="missing required"):
        validate_payload(schema, {})
    with pytest.raises(ToolError, match="unexpected fields"):
        validate_payload(schema, {"query": "x", "extra": 1})
    with pytest.raises(ToolError, match="must be a string"):
        validate_payload(schema, {"query": 1})


def test_tool_service_calls_with_envelope_context() -> None:
    envelope = CallEnvelope(tenant_id="t1", user_id="u1", trace_id="trace-1")

    result = _service().call("demo.echo", {"text": "hi"}, envelope.context())

    assert result == {"echo": "hi", "tenant": "t1"}


def test_high_risk_tool_requires_approval() -> None:
    with pytest.raises(ApprovalRequired) as err:
        _service().call("demo.sandbox", {})

    assert err.value.tool == "demo.sandbox"
    assert err.value.risk == "execute"


def test_tool_deadline_and_budget_guards() -> None:
    service = _service()
    with pytest.raises(ToolError, match="deadline exceeded"):
        service.call("demo.echo", {"text": "x"}, {"deadline_ms": int(time.time() * 1000) - 1})


def test_tool_output_is_truncated() -> None:
    service = ToolService(ToolRegistry(), max_output_bytes=64)
    service.register(
        ToolSpec(name="demo.big", description="big"),
        lambda payload, ctx: {"data": "x" * 500},
    )

    result = service.call("demo.big", {})

    assert result["truncated"] is True
    assert "preview" in result


def test_llm_tools_uses_openai_shape() -> None:
    tools = _service().llm_tools()

    assert tools[0]["type"] == "function"
    assert tools[0]["function"]["name"] == "demo.echo"


def test_build_tool_service_respects_switches() -> None:
    from nexusflow_agents.core.bootstrap import build_tool_service

    assert build_tool_service(HubSettings(_env_file=None)).specs() == []
    names = {
        spec["name"]
        for spec in build_tool_service(HubSettings(_env_file=None, tools_enabled=True)).specs()
    }
    assert "search.query" in names
    assert "workflow.run" not in names

    workflow = {
        spec["name"]
        for spec in build_tool_service(
            HubSettings(_env_file=None, tools_enabled=True, workflow_enabled=True)
        ).specs()
    }
    assert "workflow.run" in workflow


class _Hub:
    def call(self, capability: str, payload: dict[str, Any], ctx: dict[str, Any], **_: Any):
        from nexusflow_agents.core.hub import AgentResult

        return AgentResult(
            ok=True,
            data={"capability": capability, "payload": payload, "ctx": ctx},
            agent="local.intent",
        )

    def call_agent(
        self, name: str, capability: str, payload: dict[str, Any], ctx: dict[str, Any], **_: Any
    ):
        from nexusflow_agents.core.hub import AgentResult

        return AgentResult(ok=True, data={"agent": name}, agent=name)


def _client(monkeypatch: pytest.MonkeyPatch, tools: ToolService) -> TestClient:
    monkeypatch.setattr(
        call_api, "get_hub_settings", lambda: HubSettings(_env_file=None, tools_enabled=True)
    )
    monkeypatch.setattr(call_api, "get_tools", lambda: tools)
    monkeypatch.setattr(call_api, "get_hub", lambda: _Hub())
    app = FastAPI()
    app.include_router(call_api.router)
    return TestClient(app)


def test_call_api_tool_and_agent(monkeypatch: pytest.MonkeyPatch) -> None:
    client = _client(monkeypatch, _service())

    tool = client.post(
        "/v1/call",
        json={"tool": {"name": "demo.echo", "input": {"text": "hi"}}},
        headers={"x-tenant-id": "t9"},
    )
    assert tool.status_code == 200
    assert tool.json()["data"] == {"echo": "hi", "tenant": "t9"}

    agent = client.post(
        "/v1/call",
        json={"agent": {"capability": "intent.classify", "input": {"message": "hi"}}},
    )
    assert agent.status_code == 200
    assert agent.json()["agent"] == "local.intent"


def test_call_api_returns_202_for_approval(monkeypatch: pytest.MonkeyPatch) -> None:
    client = _client(monkeypatch, _service())

    resp = client.post("/v1/call", json={"tool": {"name": "demo.sandbox", "input": {}}})

    assert resp.status_code == 202
    assert resp.json()["detail"]["status"] == "approval_required"


def test_call_api_disabled_by_default(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(call_api, "get_hub_settings", lambda: HubSettings(_env_file=None))
    monkeypatch.setattr(call_api, "get_tools", lambda: _service())
    app = FastAPI()
    app.include_router(call_api.router)

    resp = TestClient(app).post(
        "/v1/call", json={"tool": {"name": "demo.echo", "input": {"text": "x"}}}
    )

    assert resp.status_code == 404


def test_registry_snapshot_exposes_agent_contract() -> None:
    registry = AgentRegistry()
    registry.register(
        AgentSpec(
            name="remote.agent",
            capabilities=frozenset({"agent.call"}),
            kind="http",
            endpoint="http://127.0.0.1:9000",
            version="2.0",
            risk="external",
            tools=frozenset({"search.query"}),
            permissions=frozenset({"search:use"}),
        )
    )

    spec = registry.snapshot()[0]

    assert spec["version"] == "2.0"
    assert spec["risk"] == "external"
    assert spec["tools"] == ["search.query"]
    assert spec["permissions"] == ["search:use"]
    assert registry.get("remote.agent") is not None
