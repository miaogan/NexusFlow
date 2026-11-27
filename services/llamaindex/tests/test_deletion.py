from __future__ import annotations

import sys
import types
from typing import Any

import pytest

from app.schemas import DeleteRequest
from app.services import deletion as deletion_mod
from app.services.deletion import delete_document


def _install_fake_qdrant(monkeypatch: pytest.MonkeyPatch) -> None:
    """qdrant-client is optional in this environment, so stub its filter models."""

    class _MatchValue:
        def __init__(self, value: str):
            self.value = value

    class _FieldCondition:
        def __init__(self, key: str, match: _MatchValue):
            self.key = key
            self.match = match

    class _Filter:
        def __init__(self, must: list[_FieldCondition]):
            self.must = must

    module = types.ModuleType("qdrant_client")
    module.models = types.SimpleNamespace(
        Filter=_Filter, FieldCondition=_FieldCondition, MatchValue=_MatchValue
    )
    monkeypatch.setitem(sys.modules, "qdrant_client", module)


class _Count:
    def __init__(self, value: int):
        self.count = value


class _FakeClient:
    def __init__(self, exists: bool = True, count: int = 3):
        self.exists = exists
        self.count_value = count
        self.deleted: list[dict[str, Any]] = []
        self.counted: list[dict[str, Any]] = []

    def collection_exists(self, collection: str) -> bool:
        return self.exists

    def count(self, **kwargs: Any) -> _Count:
        self.counted.append(kwargs)
        return _Count(self.count_value)

    def delete(self, **kwargs: Any) -> None:
        self.deleted.append(kwargs)


def test_delete_targets_task_filter_in_kb_collection(monkeypatch: pytest.MonkeyPatch) -> None:
    _install_fake_qdrant(monkeypatch)
    monkeypatch.setattr(deletion_mod.settings, "collection_prefix", "nexusflow")
    cleared: list[str | None] = []
    monkeypatch.setattr(deletion_mod, "clear_graph_cache", lambda name=None: cleared.append(name))
    client = _FakeClient(count=4)

    out = delete_document(DeleteRequest(task_id="task-1", kb_id="kb-a"), client=client)

    assert out.success and out.deleted == 4 and out.collection == "nexusflow_kb-a"
    assert client.counted[0]["collection_name"] == "nexusflow_kb-a"
    selector = client.deleted[0]["points_selector"]
    condition = selector.must[0]
    assert condition.key == "task_id" and condition.match.value == "task-1"
    assert client.deleted[0]["wait"] is True
    assert cleared == ["nexusflow_kb-a"]


def test_delete_is_idempotent_without_collection(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(deletion_mod.settings, "collection_prefix", "nexusflow")
    cleared: list[str | None] = []
    monkeypatch.setattr(deletion_mod, "clear_graph_cache", lambda name=None: cleared.append(name))
    client = _FakeClient(exists=False)

    out = delete_document(DeleteRequest(task_id="missing"), client=client)

    assert out.success and out.deleted == 0 and out.collection == "nexusflow_default"
    assert client.deleted == []
    assert cleared == ["nexusflow_default"]
