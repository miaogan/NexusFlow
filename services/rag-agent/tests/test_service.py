from __future__ import annotations

import asyncio
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


class FakeStreamGraph:
    def __init__(self) -> None:
        self.retrieve_calls: list[dict[str, Any]] = []

    def retrieve(self, **kwargs: Any) -> dict[str, Any]:
        self.retrieve_calls.append(kwargs)
        return {
            "chunks": [
                {"chunk_id": "c1", "content": "ctx", "score": 0.5, "meta": {"file_name": "a.md"}}
            ],
            "trace_id": "tr-stream",
        }

    def citations(self, chunks: list[dict[str, Any]]) -> list[dict[str, Any]]:
        return [
            {
                "chunk_id": c["chunk_id"],
                "content": c["content"],
                "score": c["score"],
                "meta": c["meta"],
            }
            for c in chunks
        ]

    async def stream_answer(self, query, history, chunks):
        for piece in ("hello ", "world"):
            yield piece

    def generate_intent(self, intent, message, history):
        return f"{intent} answer"

    async def stream_intent_answer(self, intent, message, history):
        for piece in ("chat ", "reply"):
            yield piece


class StaticIntent:
    def __init__(self, intent: str = "knowledge_qa") -> None:
        self.intent = intent

    def classify(self, message: str, history: list[dict[str, str]]) -> dict[str, Any]:
        return {
            "intent": self.intent,
            "confidence": 0.9,
            "need_retrieval": self.intent == "knowledge_qa",
            "reason": "test",
            "source": "test",
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


def _collect_stream(service: ChatService, req: ChatRequest, tenant: str, user: str) -> list[dict]:
    async def run() -> list[dict]:
        return [event async for event in service.chat_stream(req, tenant, user, None)]

    return asyncio.run(run())


def test_chat_stream_emits_pipeline_events_and_persists() -> None:
    store = InMemoryConversationStore()
    graph = FakeStreamGraph()
    service = ChatService(store, graph, Settings(_env_file=None))

    events = _collect_stream(service, ChatRequest(message="hi"), "t1", "u1")
    types = [e["type"] for e in events]

    assert events[0] == {"type": "stage", "stage": "intent", "status": "start"}
    assert events[1]["type"] == "intent"
    assert events[1]["need_retrieval"] is True
    assert any(
        e == {"type": "stage", "stage": "retrieve", "status": "start"} for e in events
    )
    assert "sources" in types and "token" in types
    assert types[-1] == "done"

    sources = next(e for e in events if e["type"] == "sources")
    assert sources["citations"][0]["chunk_id"] == "c1"

    tokens = "".join(e["content"] for e in events if e["type"] == "token")
    assert tokens == "hello world"

    done = events[-1]
    assert done["answer"] == "hello world"
    assert done["trace_id"] == "tr-stream"
    assert graph.retrieve_calls[0]["tenant_id"] == "t1"

    detail = store.get("t1", "u1", done["conversation_id"])
    assert detail is not None
    assert [m["role"] for m in detail["messages"]] == ["user", "assistant"]
    assert detail["messages"][1]["citations"][0]["chunk_id"] == "c1"


def test_chat_stream_reports_unknown_conversation() -> None:
    service, _, _ = _service()
    events = _collect_stream(
        service, ChatRequest(message="hi", conversation_id="missing"), "t1", "u1"
    )
    assert events == [{"type": "error", "error": "conversation not found"}]


def test_chat_stream_skips_retrieval_for_chitchat() -> None:
    store = InMemoryConversationStore()
    graph = FakeStreamGraph()
    service = ChatService(
        store, graph, Settings(_env_file=None), StaticIntent("chitchat")
    )

    events = _collect_stream(service, ChatRequest(message="你好"), "t1", "u1")

    assert graph.retrieve_calls == []
    assert not any(e["type"] == "sources" for e in events)
    intent_event = next(e for e in events if e["type"] == "intent")
    assert intent_event["intent"] == "chitchat"
    assert intent_event["need_retrieval"] is False

    done = events[-1]
    assert done["answer"] == "chat reply"
    assert done["citations"] == []
    assert done["trace_id"] is None


def test_intent_classifier_falls_back_without_url() -> None:
    from app.services.intent import HttpIntentClassifier

    result = HttpIntentClassifier("").classify("hi", [])
    assert result["intent"] == "knowledge_qa"
    assert result["need_retrieval"] is True
    assert result["source"] == "fallback"
