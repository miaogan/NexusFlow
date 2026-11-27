from __future__ import annotations

import asyncio

from nexusflow_agents.agents.rag.services.llm import AnswerGenerator


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


def _collect(agen) -> list[str]:
    async def run() -> list[str]:
        return [piece async for piece in agen]

    return asyncio.run(run())


def test_astream_fallback_yields_numbered_chunks() -> None:
    gen = AnswerGenerator()
    parts = _collect(
        gen.astream(
            "what?",
            [],
            [{"chunk_id": "c1", "content": "Alpha"}, {"chunk_id": "c2", "content": "Beta"}],
        )
    )
    assert len(parts) == 1
    assert "[1] Alpha" in parts[0]
    assert "[2] Beta" in parts[0]


def test_astream_without_chunks_returns_guidance() -> None:
    gen = AnswerGenerator()
    parts = _collect(gen.astream("unknown?", [], []))
    assert len(parts) == 1
    assert "未在知识库中检索到" in parts[0]


def test_corrected_answers_are_injected_into_prompt_and_fallback() -> None:
    gen = AnswerGenerator()
    corrected = [{"query": "怎么部署？", "answer": "使用 make up 启动。"}]

    block = gen._corrected_block(corrected)
    assert "[C1]" in block
    assert "怎么部署？" in block
    assert "使用 make up 启动。" in block

    # Extractive fallback surfaces the correction ahead of chunks.
    answer = gen.generate(
        "怎么部署？",
        [],
        [{"chunk_id": "c1", "content": "other context"}],
        corrected=corrected,
    )
    assert "历史纠正答案" in answer
    assert answer.index("使用 make up 启动。") < answer.index("other context")

    # Corrections alone answer the question even when retrieval found nothing.
    only_corrected = gen.generate("怎么部署？", [], [], corrected=corrected)
    assert "未在知识库中检索到" not in only_corrected
    assert "使用 make up 启动。" in only_corrected


def test_no_corrections_keeps_original_prompt_shape() -> None:
    gen = AnswerGenerator()
    assert gen._corrected_block([]) == ""
    assert gen._corrected_block(None) == ""
    assert "未在知识库中检索到" in gen.generate("q", [], [])
