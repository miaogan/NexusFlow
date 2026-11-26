from __future__ import annotations

from collections.abc import AsyncIterator
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
        summary: str = "",
    ) -> str: ...

    def astream(
        self,
        query: str,
        history: list[dict[str, str]],
        chunks: list[dict[str, Any]],
        summary: str = "",
    ) -> AsyncIterator[str]: ...

    def generate_intent(
        self,
        intent: str,
        message: str,
        history: list[dict[str, str]],
        summary: str = "",
    ) -> str: ...

    def astream_intent(
        self,
        intent: str,
        message: str,
        history: list[dict[str, str]],
        summary: str = "",
    ) -> AsyncIterator[str]: ...


class ChatState(TypedDict, total=False):
    tenant_id: str
    user_id: str
    query: str
    history: list[dict[str, str]]
    summary: str
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
        return self.retrieve(
            query=state["query"],
            tenant_id=state.get("tenant_id") or "default",
            top_k=state.get("top_k") or 5,
            kb_id=state.get("kb_id"),
            filters=state.get("filters"),
            auth_header=state.get("auth_header"),
        )

    def retrieve(
        self,
        *,
        query: str,
        tenant_id: str,
        top_k: int,
        kb_id: str | None = None,
        filters: dict[str, str] | None = None,
        auth_header: str | None = None,
    ) -> dict[str, Any]:
        """Retrieval step, exposed so the streaming path can reuse it."""
        chunks, trace_id = self._retriever.search(
            query=query,
            tenant_id=tenant_id,
            top_k=top_k,
            kb_id=kb_id,
            filters=filters,
            auth_header=auth_header,
        )
        return {"chunks": chunks, "trace_id": trace_id}

    def citations(self, chunks: list[dict[str, Any]]) -> list[dict[str, Any]]:
        """Structural citations derived from retrieved chunks (not parsed from
        the model's text), so the UI can link [n] markers to real sources."""
        return [
            {
                "chunk_id": c.get("chunk_id", ""),
                "content": c.get("content", ""),
                "score": float(c.get("score") or 0.0),
                "meta": c.get("meta") or {},
            }
            for c in chunks
        ]

    def _generate(self, state: ChatState) -> dict[str, Any]:
        chunks = state.get("chunks") or []
        answer = self._generator.generate(
            state["query"],
            state.get("history") or [],
            chunks,
            summary=state.get("summary") or "",
        )
        return {"answer": answer, "citations": self.citations(chunks)}

    async def stream_answer(
        self,
        query: str,
        history: list[dict[str, str]],
        chunks: list[dict[str, Any]],
        summary: str = "",
    ) -> AsyncIterator[str]:
        async for piece in self._generator.astream(
            query, history, chunks, summary=summary
        ):
            yield piece

    def generate_intent_answer(
        self,
        intent: str,
        message: str,
        history: list[dict[str, str]],
        summary: str = "",
    ) -> str:
        return self._generator.generate_intent(
            intent, message, history, summary=summary
        )

    async def stream_intent_answer(
        self,
        intent: str,
        message: str,
        history: list[dict[str, str]],
        summary: str = "",
    ) -> AsyncIterator[str]:
        async for piece in self._generator.astream_intent(
            intent, message, history, summary=summary
        ):
            yield piece

    def invoke(self, **state: Any) -> ChatState:
        return self._compiled.invoke(state)  # type: ignore[return-value]
