from __future__ import annotations

import asyncio
import io
import json
from collections.abc import Iterator
from typing import Any

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from nexusflow_agents.core.config import HubSettings
from nexusflow_agents.core.registry import AgentRegistry, AgentSpec
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
def client(monkeypatch: pytest.MonkeyPatch) -> Iterator[TestClient]:
    service = _service()
    registry = AgentRegistry()
    registry.register(
        AgentSpec(
            name="rag",
            capabilities=frozenset({"intent.classify"}),
            kind="local",
            skills=frozenset({"kb.search"}),
        )
    )
    monkeypatch.setattr(
        mcp_server,
        "get_hub_settings",
        lambda: HubSettings(_env_file=None, mcp_enabled=True),
    )
    monkeypatch.setattr(mcp_server, "get_tools", lambda: service)
    monkeypatch.setattr(mcp_server, "get_registry", lambda: registry)
    app = FastAPI()
    app.include_router(mcp_server.router)
    _clear_stream_sessions()
    with TestClient(app) as test_client:
        yield test_client
    _clear_stream_sessions()


def _clear_stream_sessions() -> None:
    mcp_server._stream_queues.clear()
    mcp_server.get_session_store().clear()


def _rpc(
    client: TestClient,
    method: str,
    params: dict[str, Any] | None = None,
    headers: dict[str, str] | None = None,
):
    return client.post(
        "/mcp",
        json={"jsonrpc": "2.0", "id": 1, "method": method, "params": params or {}},
        headers=headers,
    )


def _init_session(client: TestClient) -> str:
    resp = _rpc(client, "initialize")
    assert resp.status_code == 200
    session_id = resp.headers.get("mcp-session-id")
    assert session_id
    return session_id


def test_mcp_initialize_and_list_hides_sandbox(client: TestClient) -> None:
    init = _rpc(client, "initialize")
    assert init.status_code == 200
    caps = init.json()["result"]["capabilities"]
    assert "resources" in caps and "prompts" in caps
    assert caps["resources"]["subscribe"] is True
    assert caps["resources"]["listChanged"] is True
    assert init.json()["result"]["serverInfo"]["name"] == "nexusflow-agent-hub"

    listing = _rpc(client, "tools/list").json()["result"]["tools"]
    names = {tool["name"] for tool in listing}
    assert "search.query" in names
    assert "sandbox.execute" not in names


def test_mcp_lists_and_reads_resources(client: TestClient) -> None:
    resources = _rpc(client, "resources/list").json()["result"]["resources"]
    assert {item["uri"] for item in resources} == {
        "nexusflow://agents",
        "nexusflow://skills",
        "nexusflow://tools",
    }

    templates = _rpc(client, "resources/templates/list").json()["result"][
        "resourceTemplates"
    ]
    assert {item["uriTemplate"] for item in templates} == {
        "nexusflow://agents/{name}",
        "nexusflow://tools/{name}",
    }

    agents = _read_resource(client, "nexusflow://agents")
    assert [item["name"] for item in agents["agents"]] == ["rag"]

    tool_names = {
        item["name"] for item in _read_resource(client, "nexusflow://tools")["tools"]
    }
    assert "search.query" in tool_names
    assert "sandbox.execute" not in tool_names

    skills = _read_resource(client, "nexusflow://skills")["skills"]
    assert skills[0]["skill"] == "kb.search"


def test_mcp_reads_single_resource(client: TestClient) -> None:
    agent = _read_resource(client, "nexusflow://agents/rag")
    assert agent["name"] == "rag" and agent["kind"] == "local"

    tool = _read_resource(client, "nexusflow://tools/search.query")
    assert tool["name"] == "search.query"
    assert tool["inputSchema"]["required"] == ["query"]

    missing = _rpc(
        client, "resources/read", {"uri": "nexusflow://tools/unknown"}
    ).json()
    assert missing["error"]["code"] == -32002


def test_mcp_lists_and_gets_prompts(client: TestClient) -> None:
    prompts = _rpc(client, "prompts/list").json()["result"]["prompts"]
    names = {item["name"] for item in prompts}
    assert names == {"rag_answer", "summarize", "intent_classify"}

    result = _rpc(
        client,
        "prompts/get",
        {
            "name": "rag_answer",
            "arguments": {"question": "what is RRF?", "context": "[1] fusion"},
        },
    ).json()["result"]
    text = result["messages"][0]["content"]["text"]
    assert "what is RRF?" in text and "[1] fusion" in text

    summary = _rpc(
        client,
        "prompts/get",
        {"name": "summarize", "arguments": {"text": "long text", "max_chars": "300"}},
    ).json()["result"]
    assert "300 字" in summary["messages"][0]["content"]["text"]

    missing = _rpc(
        client, "prompts/get", {"name": "summarize", "arguments": {}}
    ).json()
    assert missing["error"]["code"] == -32602

    unknown = _rpc(client, "prompts/get", {"name": "nope", "arguments": {}}).json()
    assert unknown["error"]["code"] == -32602


def test_streamable_http_initialize_returns_session(client: TestClient) -> None:
    resp = _rpc(client, "initialize")
    assert resp.status_code == 200
    assert resp.headers.get("mcp-session-id")


def test_streamable_http_can_reply_with_sse(client: TestClient) -> None:
    resp = client.post(
        "/mcp",
        json={"jsonrpc": "2.0", "id": 1, "method": "initialize"},
        headers={"Accept": "text/event-stream"},
    )
    assert resp.status_code == 200
    assert resp.headers["content-type"].startswith("text/event-stream")
    assert "event: message" in resp.text
    assert "nexusflow-agent-hub" in resp.text
    assert resp.headers.get("mcp-session-id")


def test_streamable_http_rejects_unknown_session(client: TestClient) -> None:
    resp = _rpc(
        client,
        "initialize",
        headers={"Mcp-Session-Id": "missing"},
    )
    assert resp.status_code == 404


def test_streamable_http_delete_session(client: TestClient) -> None:
    session_id = _init_session(client)
    headers = {"Mcp-Session-Id": session_id}
    assert client.delete("/mcp", headers=headers).status_code == 204
    assert client.delete("/mcp", headers=headers).status_code == 404


def test_streamable_http_get_requires_sse(client: TestClient) -> None:
    assert client.get("/mcp").status_code == 406


def test_resource_subscription_requires_session(client: TestClient) -> None:
    resp = _rpc(client, "resources/subscribe", {"uri": "nexusflow://agents"}).json()
    assert resp["error"]["code"] == -32602


def test_resource_subscription_emits_updates(client: TestClient) -> None:
    session_id = _init_session(client)
    headers = {"Mcp-Session-Id": session_id}
    subscribed = _rpc(
        client, "resources/subscribe", {"uri": "nexusflow://agents"}, headers
    ).json()
    assert subscribed["result"] == {}
    assert mcp_server._collect_updates(session_id) == []

    mcp_server.get_registry().register(
        AgentSpec(name="extra", capabilities=frozenset(), kind="local")
    )
    updates = mcp_server._collect_updates(session_id)
    assert len(updates) == 1
    assert updates[0]["method"] == "notifications/resources/updated"
    assert updates[0]["params"]["uri"] == "nexusflow://agents"
    assert mcp_server._collect_updates(session_id) == []

    missing = _rpc(
        client,
        "resources/subscribe",
        {"uri": "nexusflow://agents/nope"},
        headers,
    ).json()
    assert missing["error"]["code"] == -32002

    removed = _rpc(
        client, "resources/unsubscribe", {"uri": "nexusflow://agents"}, headers
    ).json()
    assert removed["result"] == {}


def _read_resource(client: TestClient, uri: str) -> dict[str, Any]:
    resp = _rpc(client, "resources/read", {"uri": uri}).json()
    return json.loads(resp["result"]["contents"][0]["text"])


def test_resource_list_changed_notifies_on_registry_update(client: TestClient) -> None:
    session_id = _init_session(client)
    assert mcp_server._collect_list_changed(session_id) == []

    mcp_server.get_registry().register(
        AgentSpec(name="newcomer", capabilities=frozenset(), kind="local")
    )

    updates = mcp_server._collect_list_changed(session_id)
    assert len(updates) == 1
    assert updates[0]["method"] == "notifications/resources/list_changed"
    assert mcp_server._collect_list_changed(session_id) == []


def test_memory_session_store_tracks_subscriptions() -> None:
    from nexusflow_agents.mcp.sessions import MemorySessionStore

    store = MemorySessionStore()
    store.create("s1", "t1", "fp0")

    assert store.exists("s1") is True
    assert store.tenant("s1") == "t1"
    assert store.count() == 1
    assert store.list_fingerprint("s1") == "fp0"

    store.subscribe("s1", "nexusflow://agents", "hash1")
    store.set_fingerprint("s1", "nexusflow://agents", "hash2")
    assert store.subscriptions("s1") == {"nexusflow://agents": "hash2"}

    store.unsubscribe("s1", "nexusflow://agents")
    assert store.subscriptions("s1") == {}

    store.set_list_fingerprint("s1", "fp1")
    assert store.list_fingerprint("s1") == "fp1"

    store.delete("s1")
    assert store.exists("s1") is False
    assert store.count() == 0


def test_stream_close_keeps_initialized_session() -> None:
    store = mcp_server.get_session_store()
    store.create("sid-persistent")
    mcp_server._stream_queues["sid-persistent"] = asyncio.Queue()

    # A GET stream closing must only release the in-process handle.
    mcp_server._drop_stream_session("sid-persistent")
    assert "sid-persistent" not in mcp_server._stream_queues
    assert store.exists("sid-persistent") is True

    # Ephemeral sessions created by the stream itself are deleted.
    mcp_server._drop_stream_session("sid-persistent", delete=True)
    assert store.exists("sid-persistent") is False


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
