from __future__ import annotations

from typing import Any

from nexusflow_agents.agents.rag.services.llm import AnswerGenerator


class FakeLLM:
    def __init__(self) -> None:
        self.kwargs: dict[str, Any] = {}

    def invoke(self, messages: Any, **kwargs: Any) -> Any:
        self.kwargs = kwargs
        return type("Result", (), {"content": "ok"})()


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
