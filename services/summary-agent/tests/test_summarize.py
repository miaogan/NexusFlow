from __future__ import annotations

from app.core.config import Settings
from app.graph import SummaryGraph
from app.summarize import extractive_summary


def test_extractive_merges_previous_summary_and_messages() -> None:
    summary = extractive_summary(
        "之前讨论了部署方案",
        [
            {"role": "user", "content": "什么是 RRF？"},
            {"role": "assistant", "content": "RRF 是倒数排名融合。"},
        ],
        800,
    )

    assert "此前摘要：之前讨论了部署方案" in summary
    assert "用户：什么是 RRF？" in summary
    assert "助手：RRF 是倒数排名融合。" in summary


def test_extractive_drops_oldest_when_over_budget() -> None:
    long_answer = "答案" * 120  # > assistant snippet cap
    summary = extractive_summary(
        "",
        [
            {"role": "user", "content": "第一个问题"},
            {"role": "assistant", "content": long_answer},
            {"role": "user", "content": "最新问题"},
        ],
        160,
    )

    assert len(summary) <= 160
    assert "第一个问题" not in summary
    assert "最新问题" in summary


def test_extractive_handles_empty_input() -> None:
    assert extractive_summary("", [], 200) == ""
    assert "此前摘要：只有摘要" in extractive_summary("只有摘要", [], 200)


def test_extractive_does_not_nest_previous_header() -> None:
    previous = "早期对话摘要：\n用户：旧问题\n助手：旧回答"
    summary = extractive_summary(
        previous, [{"role": "user", "content": "新问题"}], 500
    )

    assert summary.count("早期对话摘要：") == 1
    assert "旧问题" in summary
    assert "新问题" in summary


def _settings(**overrides: object) -> Settings:
    return Settings(_env_file=None, **overrides)


def test_graph_degrades_to_extractive_without_llm() -> None:
    graph = SummaryGraph(_settings())

    result = graph.invoke(
        previous_summary="",
        messages=[
            {"role": "user", "content": "介绍一下系统"},
            {"role": "assistant", "content": "NexusFlow 是知识库平台。"},
        ],
        budget_chars=400,
    )

    assert result["strategy"] == "extractive"
    assert result["dropped_messages"] == 2
    assert result["budget_chars"] == 400
    assert "介绍一下系统" in result["summary"]


def test_graph_normalizes_and_clamps_budget() -> None:
    graph = SummaryGraph(_settings(max_messages=1, max_message_chars=5))

    result = graph.invoke(
        previous_summary="旧摘要",
        messages=[
            {"role": "user", "content": "会被丢弃"},
            {"role": "assistant", "content": "abcdefghij"},
        ],
        budget_chars=50,
    )

    # Only the last message survives the batch cap, truncated to 5 chars.
    assert result["dropped_messages"] == 1
    assert result["budget_chars"] == 100
    assert "会被丢弃" not in result["summary"]
    assert "abcde" in result["summary"]
