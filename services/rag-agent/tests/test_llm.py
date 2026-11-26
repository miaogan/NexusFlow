from __future__ import annotations

from app.services.llm import AnswerGenerator


def test_extractive_fallback_uses_numbered_chunks() -> None:
    gen = AnswerGenerator()  # no base_url/api_key -> fallback
    assert gen.llm_enabled is False

    answer = gen.generate(
        "what is it?",
        [],
        [
            {"chunk_id": "c1", "content": "First   fact."},
            {"chunk_id": "c2", "content": "Second fact."},
        ],
    )
    assert "[1] First fact." in answer
    assert "[2] Second fact." in answer
    assert "未配置大模型" in answer


def test_empty_chunks_returns_guidance() -> None:
    gen = AnswerGenerator()
    answer = gen.generate("unknown?", [], [])
    assert "未在知识库中检索到" in answer


def test_context_is_capped() -> None:
    gen = AnswerGenerator(max_context_chars=10)
    context = gen._context([{"content": "abcdefghijklmnop"}])
    assert context == "[1] abcdefghij"
