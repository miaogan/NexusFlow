from __future__ import annotations

import asyncio
from collections.abc import AsyncIterator
from typing import Any

from langchain_core.messages import AIMessage, AIMessageChunk, HumanMessage

from nexusflow_agents.agents.rag.services.approvals import ApprovalService
from nexusflow_agents.agents.rag.services.reasoning import ReasoningLoop
from nexusflow_agents.core.config import HubSettings
from nexusflow_agents.core.tooling import ToolRegistry, ToolService, ToolSpec


class FakeLLM:
    def __init__(self, responses: list[AIMessage]) -> None:
        self.responses = responses
        self.index = 0
        self.bound_tools: list[dict[str, Any]] = []

    def bind_tools(self, tools: list[dict[str, Any]]) -> FakeLLM:
        self.bound_tools = tools
        return self

    def _next(self) -> AIMessage:
        index = min(self.index, len(self.responses) - 1)
        self.index += 1
        return self.responses[index]

    async def ainvoke(self, messages: list[Any]) -> AIMessage:
        return self._next()

    async def astream(self, messages: list[Any]) -> AsyncIterator[AIMessageChunk]:
        message = self._next()
        content = message.content if isinstance(message.content, str) else ""
        if content:
            # Split into two deltas so tests can assert token streaming.
            half = max(1, len(content) // 2)
            yield AIMessageChunk(content=content[:half])
            if content[half:]:
                yield AIMessageChunk(content=content[half:])
        if message.tool_calls:
            yield AIMessageChunk(content="", tool_calls=list(message.tool_calls))
        if not content and not message.tool_calls:
            yield AIMessageChunk(content="")

    def invoke(self, messages: list[Any]) -> AIMessage:
        return self._next()


class FakeGenerator:
    llm_enabled = True

    def __init__(self, llm: FakeLLM) -> None:
        self.llm = llm

    def build_messages(self, query, history, chunks, summary="", memories=None, corrected=None):
        return [HumanMessage(content=query)]


def _tools(requires_approval: bool = False) -> ToolService:
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
            requires_approval=requires_approval,
        ),
        lambda payload, ctx: {"chunks": [payload["query"]]},
    )
    return service


def _loop(
    responses: list[AIMessage],
    *,
    tools: ToolService | None = None,
    approvals: ApprovalService | None = None,
    **overrides: Any,
) -> ReasoningLoop:
    settings = HubSettings(
        _env_file=None,
        reasoning_enabled=True,
        reasoning_max_turns=overrides.pop("max_turns", 4),
        reasoning_tool_budget=overrides.pop("tool_budget", 6),
        reasoning_token_budget=overrides.pop("token_budget", 20000),
        reasoning_timeout_seconds=30,
    )
    checker = approvals.is_approved if approvals else None
    return ReasoningLoop(
        FakeGenerator(FakeLLM(responses)), tools or _tools(), settings,
        approval_checker=checker, approvals=approvals,
    )


def _collect(loop: ReasoningLoop, **kwargs: Any) -> list[dict[str, Any]]:
    async def run() -> list[dict[str, Any]]:
        return [
            event
            async for event in loop.astream(
                query="hi", history=[], chunks=[], **kwargs
            )
        ]

    return asyncio.run(run())


def test_reasoning_executes_tool_then_answers() -> None:
    responses = [
        AIMessage(
            content="",
            tool_calls=[
                {"name": "search.query", "args": {"query": "nexus"}, "id": "call-1"}
            ],
        ),
        AIMessage(content="final answer"),
    ]
    events = _collect(_loop(responses), envelope={"tenant_id": "t1", "user_id": "u1"})

    kinds = [event["type"] for event in events]
    assert kinds[0] == "stage"
    assert "tool_call" in kinds and "tool_result" in kinds
    result = next(e for e in events if e["type"] == "tool_result")
    assert result["ok"] is True
    assert result["result"] == {"chunks": ["nexus"]}
    final = events[-1]
    assert final["type"] == "final"
    assert final["answer"] == "final answer"
    assert final["tool_calls"] == 1
    tokens = [event["content"] for event in events if event["type"] == "token"]
    assert "".join(tokens) == "final answer"
    assert len(tokens) >= 2


def test_reasoning_suppresses_duplicate_tool_calls() -> None:
    call = {"name": "search.query", "args": {"query": "same"}, "id": "c"}
    responses = [
        AIMessage(content="", tool_calls=[call]),
        AIMessage(content="", tool_calls=[{**call, "id": "c2"}]),
        AIMessage(content="done"),
    ]
    events = _collect(_loop(responses))

    results = [e for e in events if e["type"] == "tool_result"]
    assert results[0]["ok"] is True
    assert results[1]["ok"] is False
    assert "duplicate" in results[1]["result"]["error"]


def test_reasoning_respects_max_turns() -> None:
    call = {"name": "search.query", "args": {"query": "x"}, "id": "c"}
    responses = [
        AIMessage(content="", tool_calls=[call]),
        AIMessage(content="", tool_calls=[{**call, "args": {"query": "y"}, "id": "d"}]),
        AIMessage(content="fallback answer"),
    ]
    events = _collect(_loop(responses, max_turns=2))

    final = events[-1]
    assert final["answer"] == "fallback answer"
    assert final["stopped"] in {"max_turns", "answer"}


def test_reasoning_pauses_for_approval_then_resumes() -> None:
    approvals = ApprovalService(ttl_seconds=60)
    call = {"name": "search.query", "args": {"query": "danger"}, "id": "c"}
    loop = _loop(
        [
            AIMessage(content="", tool_calls=[call]),
            AIMessage(content="", tool_calls=[{**call, "id": "c2"}]),
            AIMessage(content="approved"),
        ],
        tools=_tools(requires_approval=True),
        approvals=approvals,
    )

    first = _collect(loop, envelope={"tenant_id": "t1", "user_id": "u1"})
    approval = next(e for e in first if e["type"] == "approval_required")
    assert first[-1]["stopped"] == "approval_required"
    assert approval["approval_id"].startswith("apr-")

    service = approvals.get(approval["approval_id"])
    assert service is not None
    approvals.decide(approval["approval_id"], "approve", decided_by="admin")

    second = _collect(
        loop,
        envelope={
            "tenant_id": "t1",
            "user_id": "u1",
            "approval_id": approval["approval_id"],
        },
    )
    final = second[-1]
    assert final["answer"] == "approved"
    assert any(e["type"] == "tool_result" and e["ok"] for e in second)


def test_reasoning_run_is_sync_equivalent() -> None:
    responses = [
        AIMessage(
            content="",
            tool_calls=[
                {"name": "search.query", "args": {"query": "sync"}, "id": "c"}
            ],
        ),
        AIMessage(content="sync answer"),
    ]
    outcome = _loop(responses).run(query="hi", history=[], chunks=[])

    assert outcome.answer == "sync answer"
    assert outcome.tool_calls == 1
    assert any(event["type"] == "tool_result" for event in outcome.events)
