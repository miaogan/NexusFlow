from __future__ import annotations

from typing import Any

from nexusflow_agents.agents.rag.services.llm import (
    AnswerGenerator,
    GenerationOptions,
    reset_generation_options,
    set_generation_options,
)


class FakeLLM:
    def __init__(self) -> None:
        self.kwargs: dict[str, Any] = {}

    def invoke(self, messages: Any, **kwargs: Any) -> Any:
        self.kwargs = kwargs
        return type("Result", (), {"content": "ok"})()


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
