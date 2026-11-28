from __future__ import annotations

import asyncio
import io
import json
from typing import Any

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from nexusflow_agents.core.config import HubSettings
from nexusflow_agents.core.tooling import ToolRegistry, ToolService, ToolSpec
from nexusflow_agents.mcp import server as mcp_server
from nexusflow_agents.mcp import stdio as mcp_stdio
from nexusflow_agents.mcp.client import parse_servers


def _service() -> ToolService:
    service = ToolService(ToolRegistry())
    service.register(
        ToolSpec(
            name="search.query",
            description="search",
            input_schema={
                "type": "object",
                "required": ["query"],
                "properties": {"query": {"type": "string"}},
            },
        ),
        lambda payload, ctx: {"chunks": [payload["query"]]},
    )
    service.register(
        ToolSpec(name="sandbox.execute", description="sandbox", risk="execute"),
        lambda payload, ctx: {"output": "ok"},
    )
    service.register(
        ToolSpec(
            name="demo.write",
            description="write",
            risk="write",
            requires_approval=True,
        ),
        lambda payload, ctx: {"written": True},
    )
    return service


@pytest.fixture
def client(monkeypatch: pytest.MonkeyPatch) -> TestClient:
    service = _service()
    monkeypatch.setattr(
        mcp_server,
        "get_hub_settings",
        lambda: HubSettings(_env_file=None, mcp_enabled=True),
    )
    monkeypatch.setattr(mcp_server, "get_tools", lambda: service)
    app = FastAPI()
    app.include_router(mcp_server.router)
    return TestClient(app)


def _rpc(client: TestClient, method: str, params: dict[str, Any] | None = None):
    return client.post(
        "/mcp",
        json={"jsonrpc": "2.0", "id": 1, "method": method, "params": params or {}},
    )


def test_mcp_initialize_and_list_hides_sandbox(client: TestClient) -> None:
    init = _rpc(client, "initialize")
    assert init.status_code == 200
    assert init.json()["result"]["serverInfo"]["name"] == "nexusflow-agent-hub"

    listing = _rpc(client, "tools/list").json()["result"]["tools"]
    names = {tool["name"] for tool in listing}
    assert "search.query" in names
    assert "sandbox.execute" not in names


def test_mcp_calls_tool(client: TestClient) -> None:
    resp = _rpc(
        client,
        "tools/call",
        {"name": "search.query", "arguments": {"query": "nexus"}},
    ).json()

    text = resp["result"]["content"][0]["text"]
    assert json.loads(text) == {"chunks": ["nexus"]}
    assert resp["result"]["isError"] is False


def test_mcp_high_risk_tool_requires_approval(client: TestClient) -> None:
    resp = _rpc(client, "tools/call", {"name": "demo.write", "arguments": {}}).json()

    assert resp["error"]["code"] == -32002
    assert resp["error"]["data"]["approval_id"].startswith("apr-")


def test_mcp_disabled(client: TestClient, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(
        mcp_server,
        "get_hub_settings",
        lambda: HubSettings(_env_file=None, mcp_enabled=False),
    )
    assert _rpc(client, "initialize").status_code == 404


def test_notifications_return_no_response(client: TestClient) -> None:
    resp = client.post(
        "/mcp",
        json={"jsonrpc": "2.0", "method": "notifications/initialized"},
    )
    assert resp.status_code == 202
    assert resp.content == b""


def test_sse_messages_require_known_session(client: TestClient) -> None:
    resp = client.post(
        "/mcp/messages?session_id=missing",
        json={"jsonrpc": "2.0", "id": 1, "method": "initialize"},
    )
    assert resp.status_code == 404


def test_sse_messages_queue_response(client: TestClient) -> None:
    queue: asyncio.Queue[dict[str, Any]] = asyncio.Queue()
    mcp_server._sse_sessions["test-session"] = queue
    try:
        resp = client.post(
            "/mcp/messages?session_id=test-session",
            json={"jsonrpc": "2.0", "id": 7, "method": "initialize"},
        )
        assert resp.status_code == 202
        message = queue.get_nowait()
        assert message["id"] == 7
        assert message["result"]["serverInfo"]["name"] == "nexusflow-agent-hub"

        client.post(
            "/mcp/messages?session_id=test-session",
            json={"jsonrpc": "2.0", "method": "notifications/initialized"},
        )
        assert queue.empty()
    finally:
        mcp_server._sse_sessions.pop("test-session", None)


def test_stdio_transport_processes_lines(monkeypatch: pytest.MonkeyPatch) -> None:
    service = _service()
    monkeypatch.setattr(
        mcp_server,
        "get_hub_settings",
        lambda: HubSettings(_env_file=None, mcp_enabled=True),
    )
    monkeypatch.setattr(mcp_server, "get_tools", lambda: service)

    lines = [
        json.dumps({"jsonrpc": "2.0", "id": 1, "method": "initialize"}),
        json.dumps(
            {
                "jsonrpc": "2.0",
                "id": 2,
                "method": "tools/call",
                "params": {"name": "search.query", "arguments": {"query": "nexus"}},
            }
        ),
        json.dumps({"jsonrpc": "2.0", "method": "notifications/initialized"}),
        '{"jsonrpc": "2.0", "id": 3,',
    ]
    reader = io.BytesIO(("\n".join(lines) + "\n").encode())
    writer = io.StringIO()

    code = mcp_stdio.process_stream(reader, writer, 1 << 20)

    assert code == 0
    responses = [json.loads(line) for line in writer.getvalue().splitlines()]
    assert len(responses) == 3
    assert responses[0]["id"] == 1
    assert responses[1]["id"] == 2
    assert responses[2]["error"]["code"] == -32700


def test_parse_servers_skips_invalid_urls() -> None:
    servers = parse_servers("a=http://one/mcp, b=https://two/mcp;c=ftp://bad")

    assert servers == [("a", "http://one/mcp"), ("b", "https://two/mcp")]
