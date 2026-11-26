from __future__ import annotations

from app.rules import classify_rules


def test_greeting_is_chitchat() -> None:
    result = classify_rules("你好")
    assert result.intent == "chitchat"
    assert result.need_retrieval is False


def test_question_is_knowledge_qa() -> None:
    result = classify_rules("NexusFlow 的检索链路是如何工作的？")
    assert result.intent == "knowledge_qa"
    assert result.need_retrieval is True


def test_greeting_with_question_is_not_chitchat() -> None:
    result = classify_rules("你好，请问检索链路怎么工作？")
    assert result.intent == "knowledge_qa"


def test_unsupported_action_needs_clarification() -> None:
    result = classify_rules("帮我删除所有文档")
    assert result.intent == "clarify"
    assert result.need_retrieval is False


def test_short_ambiguous_message_needs_clarification() -> None:
    result = classify_rules("这个呢")
    assert result.intent == "clarify"
