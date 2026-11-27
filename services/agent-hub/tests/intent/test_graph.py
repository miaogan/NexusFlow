from __future__ import annotations

from nexusflow_agents.agents.intent.core.config import Settings
from nexusflow_agents.agents.intent.graph import IntentGraph


def _graph() -> IntentGraph:
    return IntentGraph(Settings(_env_file=None))


def test_graph_falls_back_to_rules_without_llm() -> None:
    result = _graph().invoke("你好", [])
    assert result["intent"] == "chitchat"
    assert result["source"] == "rules"


def test_graph_routes_available_skill() -> None:
    result = _graph().invoke(
        "please translate this paragraph",
        [],
        ["translate", "expense.approval"],
    )
    assert result["intent"] == "skill"
    assert result["skill"] == "translate"
    assert result["need_retrieval"] is False


def test_graph_ignores_skill_without_offer() -> None:
    result = _graph().invoke("please translate this paragraph", [], [])
    assert result["intent"] != "skill"


def test_graph_classifies_knowledge_question() -> None:
    result = _graph().invoke("Rerank 是怎么工作的？", [{"role": "user", "content": "之前聊过检索"}])
    assert result["intent"] == "knowledge_qa"
    assert result["need_retrieval"] is True
    assert 0.0 <= result["confidence"] <= 1.0
