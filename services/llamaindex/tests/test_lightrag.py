from __future__ import annotations

import pytest

from app.retrieval import lightrag_engine, recall
from app.retrieval.fusion import Chunk


def test_workspace_is_tenant_scoped_and_sanitized() -> None:
    assert lightrag_engine.workspace_for("t1", "kb-1") == "t1__kb-1"
    assert lightrag_engine.workspace_for("", "default") == "default__default"
    assert lightrag_engine.workspace_for("租户 A", "kb/1") == "A__kb_1"


def test_disabled_without_llm(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(lightrag_engine.settings, "lightrag_enabled", True)
    monkeypatch.setattr(lightrag_engine.settings, "llm_base_url", "")
    monkeypatch.setattr(lightrag_engine.settings, "llm_api_key", "sk")
    assert lightrag_engine.enabled() is False

    monkeypatch.setattr(lightrag_engine.settings, "llm_base_url", "http://llm/v1")
    assert lightrag_engine.enabled() is True


def test_storage_names_follow_driver(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(lightrag_engine.settings, "lightrag_storage_driver", "file")
    assert lightrag_engine._storage_names() == (
        "JsonKVStorage",
        "NanoVectorDBStorage",
        "NetworkXStorage",
        "JsonDocStatusStorage",
    )

    monkeypatch.setattr(
        lightrag_engine.settings, "lightrag_storage_driver", "postgres"
    )
    monkeypatch.setattr(lightrag_engine.settings, "lightrag_graph_storage", "")
    assert lightrag_engine._storage_names() == (
        "PGKVStorage",
        "PGVectorStorage",
        "PGTableGraphStorage",
        "PGDocStatusStorage",
    )

    monkeypatch.setattr(
        lightrag_engine.settings, "lightrag_graph_storage", "PGGraphStorage"
    )
    assert lightrag_engine._storage_names()[2] == "PGGraphStorage"


def test_query_maps_structured_chunks(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(lightrag_engine.settings, "lightrag_enabled", True)
    monkeypatch.setattr(lightrag_engine.settings, "llm_base_url", "http://llm/v1")
    monkeypatch.setattr(lightrag_engine.settings, "llm_api_key", "sk")
    monkeypatch.setattr(
        lightrag_engine.settings, "lightrag_working_dir", "/tmp/nexusflow-test-lightrag"
    )

    async def fake_query(workspace, query, mode, top_k):
        assert workspace == "t1__kb1"
        assert query == "RRF 怎么融合"
        return [
            Chunk("c1", "RRF 融合向量与稀疏结果", 1.0, {"file_path": "a.md"}, "graph")
        ]

    monkeypatch.setattr(lightrag_engine, "_query_async", fake_query)

    chunks = lightrag_engine.query("t1", "kb1", "RRF 怎么融合", 5)
    assert [chunk.chunk_id for chunk in chunks] == ["c1"]
    assert chunks[0].meta["file_path"] == "a.md"


def test_query_returns_empty_when_disabled(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(lightrag_engine.settings, "lightrag_enabled", False)

    def boom(*_args, **_kwargs):
        raise AssertionError("LightRAG must not run while disabled")

    monkeypatch.setattr(lightrag_engine, "_query_async", boom)
    assert lightrag_engine.query("t1", "kb1", "q", 5) == []


def test_recall_prefers_lightrag_when_available(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(recall.lightrag_engine, "enabled", lambda: True)
    monkeypatch.setattr(
        recall.lightrag_engine,
        "query",
        lambda tenant, kb, query, top_k: [Chunk("lr-1", "lightrag chunk")],
    )
    monkeypatch.setattr(
        recall,
        "build_graph",
        lambda *_args, **_kwargs: pytest.fail("in-house graph must not build"),
    )

    chunks = recall._graph_recall("q", 3, {"tenant_id": "t1", "kb_id": "kb1"})
    assert [chunk.chunk_id for chunk in chunks] == ["lr-1"]


def test_recall_falls_back_when_lightrag_is_empty(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr(recall.lightrag_engine, "enabled", lambda: True)
    monkeypatch.setattr(
        recall.lightrag_engine, "query", lambda tenant, kb, query, top_k: []
    )
    fallback = Chunk("in-house", "co-occurrence hit", source="graph")
    monkeypatch.setattr(recall, "_scroll_documents", lambda *_args, **_kwargs: [])
    monkeypatch.setattr(recall, "build_graph", lambda *_args, **_kwargs: object())
    monkeypatch.setattr(recall, "query_graph", lambda *_args, **_kwargs: [fallback])
    monkeypatch.setattr(recall, "cached_graph", lambda _scope, build: build())

    chunks = recall._graph_recall("q", 3, {"tenant_id": "t1", "kb_id": "kb1"})
    assert [chunk.chunk_id for chunk in chunks] == ["in-house"]
