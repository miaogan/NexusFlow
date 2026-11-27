from __future__ import annotations

import uuid

from app.services.chunking import stable_node_id
from app.services.indexing import stale_ids


def test_stable_node_id_is_deterministic_uuid() -> None:
    first = stable_node_id("task-1", 0, "hello world")
    again = stable_node_id("task-1", 0, "hello world")

    assert first == again
    uuid.UUID(first)  # Qdrant only accepts UUID or integer point ids
    assert first != stable_node_id("task-1", 1, "hello world")
    assert first != stable_node_id("task-2", 0, "hello world")
    assert first != stable_node_id("task-1", 0, "changed content")


def test_stale_ids_returns_only_removed_points() -> None:
    assert stale_ids({"a", "b", "c"}, {"b", "c", "d"}) == ["a"]
    assert stale_ids(set(), {"a"}) == []
    assert stale_ids({"a"}, {"a"}) == []
