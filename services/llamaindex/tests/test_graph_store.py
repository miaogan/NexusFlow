from __future__ import annotations

from typing import Any

import pytest

from app.retrieval import graph as graph_mod
from app.retrieval.fusion import Chunk
from app.retrieval.graph import (
    build_cooccurrence_graph,
    cached_graph,
    clear_graph_cache,
    query_graph,
)
from app.retrieval.graph_store import graph_from_payload, graph_to_payload


def _docs() -> list[Chunk]:
    return [
        Chunk("c1", "NexusFlow 使用 Go 负责编排调度，Python 负责索引构建。"),
        Chunk("c2", "检索链路使用向量召回与 RRF 融合，再由 Cross-Encoder 精排。"),
    ]


def test_payload_roundtrip_preserves_graph() -> None:
    original = build_cooccurrence_graph(_docs())
    restored = graph_from_payload(graph_to_payload(original))

    assert restored.mode == original.mode
    assert set(restored.nodes) == set(original.nodes)
    assert set(restored.edges) == set(original.edges)
    assert restored.chunks["c1"].content == original.chunks["c1"].content
    assert [chunk.chunk_id for chunk in query_graph(restored, "RRF 融合", 3)] == [
        chunk.chunk_id for chunk in query_graph(original, "RRF 融合", 3)
    ]


class FakeGraphStore:
    def __init__(self) -> None:
        self.data: dict[str, dict[str, Any]] = {}
        self.loaded: list[str] = []

    def load(self, scope: str) -> dict[str, Any] | None:
        self.loaded.append(scope)
        return self.data.get(scope)

    def save(self, scope: str, payload: dict[str, Any]) -> None:
        self.data[scope] = payload

    def delete(self, scope: str) -> None:
        self.data.pop(scope, None)

    def delete_prefix(self, prefix: str) -> None:
        for key in [key for key in self.data if key.startswith(prefix)]:
            self.data.pop(key, None)

    def clear(self) -> None:
        self.data.clear()


def _fake_store(monkeypatch: pytest.MonkeyPatch) -> FakeGraphStore:
    store = FakeGraphStore()
    monkeypatch.setattr(graph_mod, "_graph_store", lambda: store)
    monkeypatch.setattr(graph_mod, "_graph_cache", {})
    # TTL 0 disables the process cache so the persistent store is exercised.
    monkeypatch.setattr(graph_mod.settings, "graph_cache_seconds", 0.0)
    return store


def test_cached_graph_reuses_persisted_snapshot(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    store = _fake_store(monkeypatch)
    builds: list[int] = []

    def build():
        builds.append(1)
        return build_cooccurrence_graph(_docs())

    first = cached_graph("nexusflow_kb|tenant=t1", build)
    assert len(builds) == 1
    assert "nexusflow_kb|tenant=t1" in store.data

    second = cached_graph("nexusflow_kb|tenant=t1", build)
    assert len(builds) == 1  # loaded from PostgreSQL instead of rebuilding
    assert set(second.nodes) == set(first.nodes)
    assert store.loaded == ["nexusflow_kb|tenant=t1", "nexusflow_kb|tenant=t1"]


def test_clear_graph_cache_drops_every_tenant_for_collection(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    store = _fake_store(monkeypatch)
    builds: list[int] = []

    def build():
        builds.append(1)
        return build_cooccurrence_graph(_docs())

    cached_graph("nexusflow_kb|tenant=t1", build)
    cached_graph("nexusflow_kb|tenant=t2", build)
    assert len(store.data) == 2

    clear_graph_cache("nexusflow_kb")
    assert store.data == {}

    cached_graph("nexusflow_kb|tenant=t1", build)
    assert len(builds) == 3


def test_cached_graph_without_store_only_uses_process_cache(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr(graph_mod, "_graph_store", lambda: None)
    monkeypatch.setattr(graph_mod, "_graph_cache", {})
    builds: list[int] = []

    def build():
        builds.append(1)
        return build_cooccurrence_graph(_docs())

    cached_graph("nexusflow_kb|tenant=t1", build)
    cached_graph("nexusflow_kb|tenant=t1", build)
    assert len(builds) == 1
