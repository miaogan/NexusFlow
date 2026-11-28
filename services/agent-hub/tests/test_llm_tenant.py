from __future__ import annotations

import asyncio
from typing import Any

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
