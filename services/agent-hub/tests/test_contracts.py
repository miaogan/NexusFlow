from __future__ import annotations

from fastapi.testclient import TestClient

from nexusflow_agents.main import app

client = TestClient(app)


def test_health_lists_local_agents() -> None:
    resp = client.get("/healthz")

    assert resp.status_code == 200
    body = resp.json()
    assert body["role"] == "all"
    assert body["mode"] == "local"
    names = {agent["name"] for agent in body["agents"]}
    assert {"local.intent", "local.summary", "local.memory"} <= names


def test_internal_agent_contracts() -> None:
    intent = client.post("/v1/intent", json={"message": "你好", "history": []})
    assert intent.status_code == 200
    assert intent.json()["intent"] == "chitchat"

    summary = client.post(
        "/v1/summarize",
        json={
            "previous_summary": "",
            "messages": [{"role": "user", "content": "请记住项目叫 NexusFlow"}],
            "budget_chars": 200,
        },
    )
    assert summary.status_code == 200
    assert summary.json()["summary"]

    memory = client.post(
        "/v1/memories/search",
        json={"tenant_id": "t1", "user_id": "u1", "query": "NexusFlow", "top_k": 3},
    )
    assert memory.status_code == 200
    assert memory.json()["source"] in {"semantic", "lexical", "empty"}


def test_chat_contract_without_llm() -> None:
    resp = client.post(
        "/v1/chat",
        json={"message": "你好"},
        headers={"x-tenant-id": "t1", "x-user-id": "u1"},
    )

    assert resp.status_code == 200
    body = resp.json()
    assert body["answer"]
    assert body["conversation_id"]


def test_chat_stream_keeps_sse_contract() -> None:
    with client.stream(
        "POST",
        "/v1/chat/stream",
        json={"message": "你好"},
        headers={"x-tenant-id": "t1", "x-user-id": "u1"},
    ) as resp:
        assert resp.status_code == 200
        body = "".join(resp.iter_text())

    assert '"type": "intent"' in body
    assert '"type": "token"' in body
    assert '"type": "done"' in body


def test_dynamic_registration_is_disabled_by_default() -> None:
    resp = client.post(
        "/v1/agents",
        json={
            "name": "remote.intent",
            "capabilities": ["intent.classify"],
            "endpoint": "http://intent:8002",
        },
    )

    assert resp.status_code == 403
