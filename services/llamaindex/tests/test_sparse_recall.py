from __future__ import annotations

import json
import sys
import types
from typing import Any

import pytest

from app.retrieval import recall as recall_mod


def _install_fake_qdrant(monkeypatch: pytest.MonkeyPatch) -> None:
    class _SparseVector:
        def __init__(self, indices: list[int], values: list[float]):
            self.indices = indices
            self.values = values

    class _MatchValue:
        def __init__(self, value: str):
            self.value = value

    class _FieldCondition:
        def __init__(self, key: str, match: _MatchValue):
            self.key = key
            self.match = match

    class _Filter:
        def __init__(self, must: list[_FieldCondition] | None = None):
            self.must = must or []

    module = types.ModuleType("qdrant_client")
    module.models = types.SimpleNamespace(
        SparseVector=_SparseVector,
        MatchValue=_MatchValue,
        FieldCondition=_FieldCondition,
        Filter=_Filter,
    )
    monkeypatch.setitem(sys.modules, "qdrant_client", module)


class _Point:
    def __init__(self, point_id: str, score: float):
        self.id = point_id
        self.score = score


class _Response:
    def __init__(self, points: list[_Point]):
        self.points = points


class _Record:
    def __init__(self, point_id: str, payload: dict[str, Any]):
        self.id = point_id
        self.payload = payload


class _FakeClient:
    def __init__(self, exists: bool = True):
        self.exists = exists
        self.query: dict[str, Any] | None = None

    def collection_exists(self, collection: str) -> bool:
        return self.exists

    def query_points(self, **kwargs: Any) -> _Response:
        self.query = kwargs
        return _Response([_Point("point-1", 0.75)])

    def retrieve(self, **_: Any) -> list[_Record]:
        node = {
            "text": "dense sparse hybrid content",
            "metadata": {"tenant_id": "t1", "task_id": "task-1"},
        }
        return [_Record("point-1", {"_node_content": json.dumps(node)})]


def test_sparse_recall_queries_companion_collection(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    _install_fake_qdrant(monkeypatch)
    monkeypatch.setattr(recall_mod.settings, "sparse_enabled", True)
    monkeypatch.setattr(recall_mod.settings, "collection_prefix", "nexusflow")
    client = _FakeClient()
    monkeypatch.setattr(recall_mod, "_qdrant_client", lambda collection: client)

    chunks = recall_mod._sparse_recall(
        "dense sparse", 5, {"kb_id": "default", "tenant_id": "t1"}
    )

    assert len(chunks) == 1
    assert chunks[0].chunk_id == "point-1"
    assert chunks[0].content == "dense sparse hybrid content"
    assert chunks[0].score == 0.75
    assert client.query is not None
    assert client.query["collection_name"] == "nexusflow_default__sparse"
    assert client.query["using"] == "sparse"
    conditions = client.query["query_filter"].must
    assert [condition.key for condition in conditions] == ["tenant_id"]
    assert conditions[0].match.value == "t1"


def test_sparse_recall_falls_back_when_collection_is_missing(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    _install_fake_qdrant(monkeypatch)
    monkeypatch.setattr(recall_mod.settings, "sparse_enabled", True)
    client = _FakeClient(exists=False)
    monkeypatch.setattr(recall_mod, "_qdrant_client", lambda collection: client)

    assert recall_mod._sparse_recall("dense", 5, {"kb_id": "default"}) == []


def test_sparse_recall_skips_when_disabled(monkeypatch: pytest.MonkeyPatch) -> None:
    _install_fake_qdrant(monkeypatch)
    monkeypatch.setattr(recall_mod.settings, "sparse_enabled", False)
    monkeypatch.setattr(
        recall_mod,
        "_qdrant_client",
        lambda collection: pytest.fail("client should not be created"),
    )

    assert recall_mod._sparse_recall("dense", 5, {"kb_id": "default"}) == []


def test_bm25_route_merges_legacy_results_until_backfilled(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    route = types.SimpleNamespace(type="bm25", top_k=3)
    sparse = recall_mod.Chunk("point-1", "sparse hit", 0.9, source="bm25")
    legacy = [
        recall_mod.Chunk("point-1", "sparse hit", 0.8, source="bm25"),
        recall_mod.Chunk("point-2", "legacy hit", 0.7, source="bm25"),
    ]
    monkeypatch.setattr(recall_mod, "_sparse_recall", lambda *_: [sparse])
    monkeypatch.setattr(recall_mod, "_bm25_recall", lambda *_: legacy)

    chunks = recall_mod.retrieve(route, "dense sparse", {})

    assert [chunk.chunk_id for chunk in chunks] == ["point-1", "point-2"]


class _ScrollClient:
    def __init__(self) -> None:
        self.kwargs: dict[str, Any] | None = None

    def scroll(self, **kwargs: Any) -> tuple[list[_Record], None]:
        self.kwargs = kwargs
        node = {
            "text": "tenant scoped content",
            "metadata": {"tenant_id": "t1", "kb_id": "default"},
        }
        return [_Record("point-1", {"_node_content": json.dumps(node)})], None


def test_scroll_documents_applies_tenant_filter(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    _install_fake_qdrant(monkeypatch)
    monkeypatch.setattr(recall_mod.settings, "collection_prefix", "nexusflow")
    client = _ScrollClient()
    monkeypatch.setattr(recall_mod, "_qdrant_client", lambda collection: client)

    docs = recall_mod._scroll_documents({"kb_id": "default", "tenant_id": "t1"})

    assert [doc.chunk_id for doc in docs] == ["point-1"]
    assert client.kwargs is not None
    conditions = client.kwargs["scroll_filter"].must
    assert [condition.key for condition in conditions] == ["tenant_id"]
    assert conditions[0].match.value == "t1"


def test_graph_scope_includes_tenant(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(recall_mod.settings, "collection_prefix", "nexusflow")

    assert recall_mod._graph_scope({"kb_id": "kb1", "tenant_id": "t1"}) == (
        "nexusflow_kb1|tenant=t1"
    )
    assert recall_mod._graph_scope({"kb_id": "kb1"}) == "nexusflow_kb1"
