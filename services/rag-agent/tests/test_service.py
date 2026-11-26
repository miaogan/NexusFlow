from __future__ import annotations

from typing import Any

from app.core.config import Settings
from app.schemas import ChatRequest
from app.services.chat import ChatService
from app.services.store import ConversationNotFound, InMemoryConversationStore


class FakeGraph:
    def __init__(self) -> None:
        self.calls: list[dict[str, Any]] = []

    def invoke(self, **state: Any) -> dict[str, Any]:
        self.calls.append(state)
        return {
            "answer": "generated answer",
            "citations": [
                {"chunk_id": "c1", "content": "ctx", "score": 0.5, "meta": {}}
            ],
            "trace_id": "tr-1",
        }


def _service() -> tuple[ChatService, InMemoryConversationStore, FakeGraph]:
    store = InMemoryConversationStore()
    graph = FakeGraph()
    settings = Settings(_env_file=None)
    return ChatService(store, graph, settings), store, graph


def test_chat_creates_conversation_and_persists_turns() -> None:
    service, store, graph = _service()
    resp = service.chat(ChatRequest(message="  hello  "), "t1", "u1", None)

    assert resp.answer == "generated answer"
    assert resp.trace_id == "tr-1"
    assert len(resp.citations) == 1

    detail = store.get("t1", "u1", resp.conversation_id)
    assert detail is not None
    assert [m["role"] for m in detail["messages"]] == ["user", "assistant"]
    assert detail["messages"][0]["content"] == "hello"
    assert detail["messages"][1]["content"] == "generated answer"
    assert graph.calls[0]["query"] == "hello"
    assert graph.calls[0]["top_k"] == Settings(_env_file=None).default_top_k


def test_chat_reuses_conversation_history() -> None:
    service, store, graph = _service()
    first = service.chat(ChatRequest(message="first"), "t1", "u1", None)
    service.chat(
        ChatRequest(message="second", conversation_id=first.conversation_id),
        "t1",
        "u1",
        None,
    )

    # Second call sees the first user turn plus the assistant answer.
    history = graph.calls[1]["history"]
    assert [h["role"] for h in history] == ["user", "assistant"]
    assert history[0]["content"] == "first"

    detail = store.get("t1", "u1", first.conversation_id)
    assert detail is not None
    assert len(detail["messages"]) == 4


def test_chat_rejects_unknown_conversation() -> None:
    service, _, _ = _service()
    try:
        service.chat(ChatRequest(message="hi", conversation_id="missing"), "t1", "u1", None)
    except ConversationNotFound:
        return
    raise AssertionError("expected ConversationNotFound")
