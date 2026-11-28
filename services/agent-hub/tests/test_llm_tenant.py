from __future__ import annotations

import asyncio
from typing import Any

import httpx

from nexusflow_agents.agents.rag.services.llm import (
    AnswerGenerator,
    GenerationOptions,
    reset_generation_options,
    reset_usage,
    set_generation_options,
    start_usage,
)


class FakeLLM:
    def __init__(self) -> None:
        self.kwargs: dict[str, Any] = {}

    def invoke(self, messages: Any, **kwargs: Any) -> Any:
        self.kwargs = kwargs
        return type("Result", (), {"content": "ok"})()


class UsageLLM(FakeLLM):
    def invoke(self, messages: Any, **kwargs: Any) -> Any:
        super().invoke(messages, **kwargs)
        return type(
            "Result",
            (),
            {
                "content": "ok",
                "usage_metadata": {"input_tokens": 11, "output_tokens": 5},
            },
        )()


class UsageStreamLLM(FakeLLM):
    async def astream(self, messages: Any, **kwargs: Any) -> Any:
        for content, usage in (
            ("he", None),
            ("llo", {"input_tokens": 7, "output_tokens": 2}),
        ):
            chunk = type("Chunk", (), {"content": content, "usage_metadata": usage})()
            yield chunk


class BindableLLM(FakeLLM):
    def __init__(self) -> None:
        super().__init__()
        self.bound: dict[str, Any] = {}
        self.messages: list[Any] = []

    def bind(self, **kwargs: Any) -> BindableLLM:
        self.bound = kwargs
        return self

    def invoke(self, messages: Any, **kwargs: Any) -> Any:
        self.messages = list(messages)
        return super().invoke(messages, **kwargs)


def test_generate_forwards_tenant_header_to_llm_router() -> None:
    generator = AnswerGenerator(max_context_chars=100)
    fake = FakeLLM()
    generator._llm = fake

    answer = generator.generate(
        "q", [], [{"content": "ctx"}], tenant_id="tenant-a"
    )

    assert answer == "ok"
    assert fake.kwargs == {"extra_headers": {"x-tenant-id": "tenant-a"}}


def test_generate_without_tenant_sends_no_headers() -> None:
    generator = AnswerGenerator(max_context_chars=100)
    fake = FakeLLM()
    generator._llm = fake

    generator.generate("q", [], [{"content": "ctx"}])

    assert fake.kwargs == {}


def test_generation_options_bind_sampling_params() -> None:
    generator = AnswerGenerator(max_context_chars=100)
    fake = BindableLLM()
    generator._llm = fake

    token = set_generation_options(
        GenerationOptions(temperature=0.1, max_tokens=128)
    )
    try:
        generator.generate("q", [], [{"content": "ctx"}])
    finally:
        reset_generation_options(token)

    assert fake.bound == {"temperature": 0.1, "max_tokens": 128}
    assert fake.kwargs == {}


def test_generation_options_append_system_prompt() -> None:
    generator = AnswerGenerator(max_context_chars=100)
    fake = BindableLLM()
    generator._llm = fake

    token = set_generation_options(GenerationOptions(system_prompt="be terse"))
    try:
        generator.generate("q", [], [{"content": "ctx"}])
    finally:
        reset_generation_options(token)

    systems = [
        message.content
        for message in fake.messages
        if type(message).__name__ == "SystemMessage"
    ]
    assert "be terse" in systems
    assert len(systems) == 2


def test_generation_options_do_not_leak_after_reset() -> None:
    generator = AnswerGenerator(max_context_chars=100)
    fake = BindableLLM()
    generator._llm = fake

    token = set_generation_options(
        GenerationOptions(temperature=0.1, max_tokens=128)
    )
    reset_generation_options(token)

    generator.generate("q", [], [{"content": "ctx"}])

    assert fake.bound == {}


def test_generate_records_token_usage() -> None:
    generator = AnswerGenerator(max_context_chars=100)
    generator._llm = UsageLLM()

    token = start_usage()
    try:
        generator.generate("q", [], [{"content": "ctx"}])
    finally:
        usage = reset_usage(token)

    assert usage.prompt_tokens == 11
    assert usage.completion_tokens == 5
    assert usage.total_tokens == 16


def test_astream_records_token_usage() -> None:
    generator = AnswerGenerator(max_context_chars=100)
    generator._llm = UsageStreamLLM()

    async def collect() -> list[str]:
        return [piece async for piece in generator.astream("q", [], [{"content": "ctx"}])]

    token = start_usage()
    try:
        pieces = asyncio.run(collect())
    finally:
        usage = reset_usage(token)

    assert pieces == ["he", "llo"]
    assert usage.prompt_tokens == 7
    assert usage.completion_tokens == 2
    assert usage.total_tokens == 9


def test_usage_is_empty_without_llm_metadata() -> None:
    generator = AnswerGenerator(max_context_chars=100)
    generator._llm = FakeLLM()

    token = start_usage()
    try:
        generator.generate("q", [], [{"content": "ctx"}])
    finally:
        usage = reset_usage(token)

    assert usage.total_tokens == 0


class FakeResponse:
    def __init__(self, payload: dict[str, Any]) -> None:
        self._payload = payload

    def raise_for_status(self) -> None:
        return None

    def json(self) -> dict[str, Any]:
        return self._payload


def test_http_intent_classifier_sends_tenant_header(monkeypatch: Any) -> None:
    from nexusflow_agents.agents.rag.services.intent import HttpIntentClassifier

    captured: dict[str, Any] = {}

    def fake_post(url: str, **kwargs: Any) -> FakeResponse:
        captured["headers"] = kwargs.get("headers")
        return FakeResponse({"intent": "knowledge_qa", "confidence": 0.8})

    monkeypatch.setattr(httpx, "post", fake_post)

    result = HttpIntentClassifier("http://intent").classify(
        "hi", [], tenant_id="tenant-a"
    )

    assert captured["headers"] == {"x-tenant-id": "tenant-a"}
    assert result["intent"] == "knowledge_qa"


def test_http_intent_classifier_omits_header_without_tenant(monkeypatch: Any) -> None:
    from nexusflow_agents.agents.rag.services.intent import HttpIntentClassifier

    captured: dict[str, Any] = {}

    def fake_post(url: str, **kwargs: Any) -> FakeResponse:
        captured["headers"] = kwargs.get("headers")
        return FakeResponse({"intent": "chitchat"})

    monkeypatch.setattr(httpx, "post", fake_post)

    HttpIntentClassifier("http://intent").classify("hi", [])

    assert captured["headers"] is None


def test_http_summarizer_sends_tenant_header(monkeypatch: Any) -> None:
    from nexusflow_agents.agents.rag.services.summary import HttpSummarizer

    captured: dict[str, Any] = {}

    def fake_post(url: str, **kwargs: Any) -> FakeResponse:
        captured["headers"] = kwargs.get("headers")
        return FakeResponse({"summary": "摘要"})

    monkeypatch.setattr(httpx, "post", fake_post)

    summary = HttpSummarizer("http://summary").summarize(
        "", [{"role": "user", "content": "hi"}], tenant_id="tenant-a"
    )

    assert captured["headers"] == {"x-tenant-id": "tenant-a"}
    assert summary == "摘要"


def test_http_memory_recall_sends_tenant_header(monkeypatch: Any) -> None:
    from nexusflow_agents.agents.rag.services.memory import HttpMemoryClient

    captured: dict[str, Any] = {}

    def fake_post(url: str, **kwargs: Any) -> FakeResponse:
        captured["headers"] = kwargs.get("headers")
        return FakeResponse({"memories": [{"content": "偏好 Go"}]})

    monkeypatch.setattr(httpx, "post", fake_post)

    memories = HttpMemoryClient("http://memory").recall("tenant-a", "u1", "q")

    assert captured["headers"] == {"x-tenant-id": "tenant-a"}
    assert memories == ["偏好 Go"]


def test_http_memory_extract_sends_tenant_header(monkeypatch: Any) -> None:
    from nexusflow_agents.agents.rag.services.memory import HttpMemoryClient

    captured: dict[str, Any] = {}

    def fake_post(url: str, **kwargs: Any) -> FakeResponse:
        captured["headers"] = kwargs.get("headers")
        return FakeResponse({})

    monkeypatch.setattr(httpx, "post", fake_post)

    HttpMemoryClient("http://memory")._extract(
        "tenant-a", "u1", [{"role": "user", "content": "hi"}]
    )

    assert captured["headers"] == {"x-tenant-id": "tenant-a"}


def test_summary_graph_caches_llm_per_tenant(monkeypatch: Any) -> None:
    from nexusflow_agents.agents.summary.core.config import Settings as SummarySettings
    from nexusflow_agents.agents.summary.graph import SummaryGraph

    graph = SummaryGraph(SummarySettings(_env_file=None))
    shared = object()
    graph._llm = shared
    built: list[str] = []

    def fake_build(tenant_id: str) -> Any:
        built.append(tenant_id)
        return object()

    monkeypatch.setattr(graph, "_build_llm", fake_build)

    assert graph._llm_for("") is shared
    first = graph._llm_for("tenant-a")
    assert graph._llm_for("tenant-a") is first
    assert built == ["tenant-a"]


def test_summary_graph_invoke_binds_tenant_llm(monkeypatch: Any) -> None:
    from nexusflow_agents.agents.summary.core.config import Settings as SummarySettings
    from nexusflow_agents.agents.summary.graph import SummaryGraph

    graph = SummaryGraph(SummarySettings(_env_file=None))
    graph._llm = object()
    seen: list[str] = []

    class FakeChatModel:
        def invoke(self, messages: Any) -> Any:
            return type("Result", (), {"content": "租户摘要"})()

    monkeypatch.setattr(
        graph, "_llm_for", lambda tenant_id: seen.append(tenant_id) or FakeChatModel()
    )

    result = graph.invoke(
        "",
        [{"role": "user", "content": "你好"}],
        budget_chars=200,
        tenant_id="tenant-a",
    )

    assert seen == ["tenant-a"]
    assert result["strategy"] == "llm"
    assert result["summary"] == "租户摘要"
