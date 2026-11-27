from __future__ import annotations

from nexusflow_agents.agents.memory.core.config import Settings
from nexusflow_agents.agents.memory.embedding import HttpEmbedder
from nexusflow_agents.agents.memory.graph import ExtractGraph, RecallGraph
from nexusflow_agents.agents.memory.rules import extract_rules
from nexusflow_agents.agents.memory.store import InMemoryMemoryStore


def _settings(**overrides: object) -> Settings:
    return Settings(_env_file=None, **overrides)


def test_extract_rules_keeps_user_facts_and_skips_questions() -> None:
    found = extract_rules(
        [
            {"role": "user", "content": "我偏好简洁的回答"},
            {"role": "assistant", "content": "好的"},
            {"role": "user", "content": "这个怎么部署？"},
            {"role": "user", "content": "我叫小明"},
        ]
    )

    contents = {item["content"]: item["kind"] for item in found}
    assert contents["我偏好简洁的回答"] == "preference"
    assert contents["我叫小明"] == "profile"
    assert all("部署" not in content for content in contents)


def test_store_dedupes_and_merges() -> None:
    store = InMemoryMemoryStore(dedupe_threshold=0.85)

    assert store.upsert(
        "t1", "u1", [{"content": "我偏好简洁的回答", "kind": "preference"}]
    ) == (1, 0)
    assert store.upsert(
        "t1", "u1", [{"content": "我偏好简洁的回答", "kind": "preference"}]
    ) == (0, 1)
    assert store.upsert(
        "t1", "u1", [{"content": "我叫小明", "kind": "profile"}]
    ) == (1, 0)

    items = store.list("t1", "u1", 10)
    assert len(items) == 2
    # Memories never leak across users.
    assert store.list("t1", "u2", 10) == []


def test_store_search_ranks_lexically() -> None:
    store = InMemoryMemoryStore()
    store.upsert(
        "t1",
        "u1",
        [
            {"content": "我偏好简洁的中文回答", "kind": "preference"},
            {"content": "我叫小明，做后端开发", "kind": "profile"},
        ],
    )

    hits = store.search("t1", "u1", "请用简洁的中文回答", None, 3)
    assert hits and "简洁" in hits[0]["content"]
    assert store.search("t1", "u1", "完全无关的向量数据库", None, 3) == []


def test_extract_then_recall_graph() -> None:
    settings = _settings()
    store = InMemoryMemoryStore()
    embedder = HttpEmbedder(settings)

    extract = ExtractGraph(settings, store, embedder)
    result = extract.invoke(
        "t1",
        "u1",
        [
            {"role": "user", "content": "请记住我的项目叫 NexusFlow"},
            {"role": "assistant", "content": "好的"},
        ],
    )
    assert result["created"] == 1
    assert any("NexusFlow" in item["content"] for item in result["memories"])

    recall = RecallGraph(settings, store, embedder)
    hits = recall.invoke("t1", "u1", "NexusFlow 项目")
    assert hits["source"] == "lexical"
    assert hits["memories"]
    assert "NexusFlow" in hits["memories"][0]["content"]


def test_extract_graph_merges_repeated_turns() -> None:
    settings = _settings()
    store = InMemoryMemoryStore()
    extract = ExtractGraph(settings, store, HttpEmbedder(settings))
    messages = [{"role": "user", "content": "我喜欢用中文回答"}]

    assert extract.invoke("t1", "u1", messages)["created"] == 1
    second = extract.invoke("t1", "u1", messages)
    assert second["created"] == 0
    assert second["merged"] == 1
    assert len(store.list("t1", "u1", 10)) == 1


def test_extract_graph_ignores_empty_turns() -> None:
    settings = _settings()
    extract = ExtractGraph(settings, InMemoryMemoryStore(), HttpEmbedder(settings))

    result = extract.invoke("t1", "u1", [{"role": "assistant", "content": "你好"}])
    assert result == {"created": 0, "merged": 0, "memories": []}
