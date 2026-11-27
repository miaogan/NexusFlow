from __future__ import annotations

from typing import Any

from nexusflow_agents.agents.rag.graph import ChatGraph


class FakeRetriever:
    def __init__(self) -> None:
        self.calls: list[dict[str, Any]] = []

    def search(self, **kwargs: Any):
        self.calls.append(kwargs)
        return (
            [
                {
                    "chunk_id": "c1",
                    "content": "NexusFlow uses Go for orchestration.",
                    "score": 0.9,
                    "meta": {"file_name": "arch.md"},
                },
                {
                    "chunk_id": "c2",
                    "content": "RAG answers are generated with LangGraph.",
                    "score": 0.8,
                    "meta": {},
                },
            ],
            "trace-123",
        )


class FakeGenerator:
    def __init__(self) -> None:
        self.seen: tuple[str, list[dict[str, str]], list[dict[str, Any]]] | None = None
        self.summary = ""
        self.memories: list[str] | None = None
        self.corrected: list[dict[str, Any]] | None = None

    def generate(self, query, history, chunks, summary="", memories=None, corrected=None):
        self.seen = (query, history, chunks)
        self.summary = summary
        self.memories = memories
        self.corrected = corrected
        return "Answer citing [1] and [2]."


def test_graph_retrieves_then_generates() -> None:
    retriever = FakeRetriever()
    generator = FakeGenerator()
    graph = ChatGraph(retriever, generator)

    out = graph.invoke(
        tenant_id="t1",
        user_id="u1",
        query="how does it work?",
        history=[{"role": "user", "content": "hi"}],
        top_k=5,
        kb_id=None,
        filters=None,
        auth_header="Bearer token",
    )

    assert out["answer"] == "Answer citing [1] and [2]."
    assert out["trace_id"] == "trace-123"
    assert len(out["citations"]) == 2
    assert out["citations"][0]["chunk_id"] == "c1"
    assert out["citations"][0]["meta"] == {"file_name": "arch.md"}

    # Identity and auth are forwarded to the retriever.
    call = retriever.calls[0]
    assert call["tenant_id"] == "t1"
    assert call["top_k"] == 5
    assert call["auth_header"] == "Bearer token"

    # History is passed through to the generator.
    assert generator.seen is not None
    _query, history, chunks = generator.seen
    assert history == [{"role": "user", "content": "hi"}]
    assert len(chunks) == 2


def test_graph_injects_corrected_answers_into_generation() -> None:
    generator = FakeGenerator()
    graph = ChatGraph(FakeRetriever(), generator)
    corrected = [{"query": "how does it work?", "answer": "Use docker compose.", "score": 1.0}]

    out = graph.invoke(
        tenant_id="t1",
        user_id="u1",
        query="how does it work?",
        history=[],
        top_k=3,
        corrected=corrected,
    )

    assert out["answer"].startswith("Answer citing")
    assert generator.corrected == corrected
