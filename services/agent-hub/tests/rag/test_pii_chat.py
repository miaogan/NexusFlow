from __future__ import annotations

import asyncio
from typing import Any

from nexusflow_agents.agents.rag.core.config import Settings
from nexusflow_agents.agents.rag.schemas import ChatRequest
from nexusflow_agents.agents.rag.services.chat import ChatService
from nexusflow_agents.agents.rag.services.store import InMemoryConversationStore
from nexusflow_agents.core.audit import get_audit

PHONE = "13800138000"
EMAIL = "alice@example.com"


class PIIGraph:
    def __init__(self) -> None:
        self.retrieve_calls: list[dict[str, Any]] = []
        self.prompt_message = ""
        self.prompt_chunks: list[dict[str, Any]] = []

    def retrieve(self, **kwargs: Any) -> dict[str, Any]:
        self.retrieve_calls.append(kwargs)
        return {
            "chunks": [
                {"chunk_id": "c1", "content": f"call {PHONE}", "score": 1.0, "meta": {}}
            ],
            "trace_id": "tr-pii",
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

    def generate(
        self, query, history, chunks, summary="", memories=None, corrected=None, tenant_id=""
    ) -> str:
        self.prompt_message = query
        self.prompt_chunks = chunks
        return f"answer about {chunks[0]['content']}"

    def invoke(self, **state: Any) -> dict[str, Any]:  # pragma: no cover
        raise AssertionError("PII mode must retrieve before generating")

    async def stream_answer(
        self, query, history, chunks, summary="", memories=None, corrected=None, tenant_id=""
    ):
        self.prompt_message = query
        self.prompt_chunks = chunks
        for piece in ("answer ", "about ", chunks[0]["content"]):
            yield piece


def test_chat_masks_prompt_and_context_but_retrieves_original() -> None:
    audit = get_audit()
    audit.clear()
    graph = PIIGraph()
    service = ChatService(
        InMemoryConversationStore(),
        graph,
        Settings(_env_file=None, pii_enabled=True),
    )

    resp = service.chat(ChatRequest(message=f"email me at {EMAIL}"), "t1", "u1", None)

    # Retrieval keeps the original query so PII searches still hit.
    assert graph.retrieve_calls[0]["query"] == f"email me at {EMAIL}"
    assert EMAIL not in graph.prompt_message
    assert "{{EMAIL_1}}" in graph.prompt_message
    assert PHONE not in graph.prompt_chunks[0]["content"]
    assert "{{PHONE_1}}" in graph.prompt_chunks[0]["content"]
    assert EMAIL not in resp.answer and PHONE not in resp.answer

    events = audit.list(action="pii.mask", tenant_id="t1")
    assert events and events[0]["detail"]["types"]["email"] == 1


def test_chat_stream_masks_output_and_records_audit() -> None:
    audit = get_audit()
    audit.clear()
    graph = PIIGraph()
    service = ChatService(
        InMemoryConversationStore(),
        graph,
        Settings(_env_file=None, pii_enabled=True),
    )

    async def collect() -> list[dict[str, Any]]:
        return [
            event
            async for event in service.chat_stream(
                ChatRequest(message=f"call {PHONE}"), "t2", "u2", None
            )
        ]

    events = asyncio.run(collect())
    done = next(event for event in events if event["type"] == "done")
    assert PHONE not in done["answer"]
    assert "{{PHONE_1}}" in done["answer"]
    assert PHONE not in graph.prompt_message

    audit_events = audit.list(action="pii.mask", tenant_id="t2")
    assert audit_events and audit_events[0]["detail"]["types"]["phone"] == 1
