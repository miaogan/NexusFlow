from __future__ import annotations

from typing import Any, Protocol, TypedDict

from langgraph.graph import END, START, StateGraph


class RetrieverLike(Protocol):
    def search(
        self,
        *,
        query: str,
        tenant_id: str,
        top_k: int,
        kb_id: str | None = None,
        filters: dict[str, str] | None = None,
        auth_header: str | None = None,
    ) -> tuple[list[dict[str, Any]], str | None]: ...


class GeneratorLike(Protocol):
    def generate(
        self,
        query: str,
        history: list[dict[str, str]],
        chunks: list[dict[str, Any]],
    ) -> str: ...


class ChatState(TypedDict, total=False):
    tenant_id: str
    user_id: str
    query: str
    history: list[dict[str, str]]
    top_k: int
    kb_id: str | None
    filters: dict[str, str] | None
    auth_header: str | None
    # outputs
    chunks: list[dict[str, Any]]
    answer: str
    citations: list[dict[str, Any]]
    trace_id: str | None


class ChatGraph:
    """LangGraph pipeline: retrieve relevant chunks, then generate an answer."""

    def __init__(self, retriever: RetrieverLike, generator: GeneratorLike) -> None:
        self._retriever = retriever
        self._generator = generator

        builder = StateGraph(ChatState)
        builder.add_node("retrieve", self._retrieve)
        builder.add_node("generate", self._generate)
        builder.add_edge(START, "retrieve")
        builder.add_edge("retrieve", "generate")
        builder.add_edge("generate", END)
        self._compiled = builder.compile()

    def _retrieve(self, state: ChatState) -> dict[str, Any]:
        chunks, trace_id = self._retriever.search(
            query=state["query"],
            tenant_id=state.get("tenant_id") or "default",
            top_k=state.get("top_k") or 5,
            kb_id=state.get("kb_id"),
            filters=state.get("filters"),
            auth_header=state.get("auth_header"),
        )
        return {"chunks": chunks, "trace_id": trace_id}

    def _generate(self, state: ChatState) -> dict[str, Any]:
        chunks = state.get("chunks") or []
        answer = self._generator.generate(
            state["query"], state.get("history") or [], chunks
        )
        citations = [
            {
                "chunk_id": c.get("chunk_id", ""),
                "content": c.get("content", ""),
                "score": float(c.get("score") or 0.0),
                "meta": c.get("meta") or {},
            }
            for c in chunks
        ]
        return {"answer": answer, "citations": citations}

    def invoke(self, **state: Any) -> ChatState:
        return self._compiled.invoke(state)  # type: ignore[return-value]
