from __future__ import annotations

from nexusflow_agents.agents.rag.services.store import InMemoryConversationStore


def test_create_get_and_list_are_scoped_to_owner() -> None:
    store = InMemoryConversationStore()
    conv = store.create("t1", "u1", "first question")
    assert conv["id"]
    assert conv["message_count"] == 0

    # Another user/tenant cannot read or list the conversation.
    assert store.get("t1", "u2", conv["id"]) is None
    assert store.get("t2", "u1", conv["id"]) is None
    items, total = store.list("t1", "u2", 20, 0)
    assert items == [] and total == 0

    store.append(conv["id"], "user", "first question", [])
    store.append(
        conv["id"],
        "assistant",
        "answer",
        [{"chunk_id": "c1", "content": "x", "score": 1.0, "meta": {}}],
    )

    detail = store.get("t1", "u1", conv["id"])
    assert detail is not None
    assert detail["message_count"] == 2
    assert [m["role"] for m in detail["messages"]] == ["user", "assistant"]
    assert detail["messages"][1]["citations"][0]["chunk_id"] == "c1"

    items, total = store.list("t1", "u1", 20, 0)
    assert total == 1
    assert items[0]["id"] == conv["id"]
    assert items[0]["message_count"] == 2


def test_list_paginates_newest_first() -> None:
    store = InMemoryConversationStore()
    ids = [store.create("t1", "u1", f"q{i}")["id"] for i in range(3)]
    # Touch the first conversation so it becomes the most recent.
    store.append(ids[0], "user", "bump", [])

    page, total = store.list("t1", "u1", 2, 0)
    assert total == 3
    assert len(page) == 2
    assert page[0]["id"] == ids[0]

    rest, _ = store.list("t1", "u1", 2, 2)
    assert len(rest) == 1


def test_list_filters_by_knowledge_base() -> None:
    store = InMemoryConversationStore()
    kb_a = store.create("t1", "u1", "question a", "kb-a")
    kb_b = store.create("t1", "u1", "question b", "kb-b")

    scoped, total = store.list("t1", "u1", 20, 0, "kb-a")
    assert total == 1
    assert scoped[0]["id"] == kb_a["id"]
    assert scoped[0]["kb_id"] == "kb-a"

    # Without a filter the owner still sees the full history.
    everything, overall = store.list("t1", "u1", 20, 0)
    assert overall == 2
    assert {item["id"] for item in everything} == {kb_a["id"], kb_b["id"]}
