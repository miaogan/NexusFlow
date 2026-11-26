from __future__ import annotations

import json

import pytest

from app.retrieval import graph
from app.retrieval.fusion import Chunk
from app.retrieval.graph import (
    build_cooccurrence_graph,
    build_graph,
    extract_terms,
    query_graph,
)


def _docs() -> list[Chunk]:
    return [
        Chunk("c1", "NexusFlow 使用 Go 负责编排调度，Python 负责索引构建。"),
        Chunk("c2", "检索链路使用向量召回与 RRF 融合，再由 Cross-Encoder 精排。"),
        Chunk("c3", "沙箱执行具备强隔离与 TTL 限制。"),
    ]


def test_extract_terms_handles_english_and_chinese() -> None:
    terms = extract_terms("NexusFlow 的检索链路 Retrieval Pipeline")
    assert "nexusflow" in terms
    assert "retrieval" in terms
    assert "pipeline" in terms
    assert "检索" in terms
    assert "链路" in terms


def test_cooccurrence_graph_ranks_matching_chunk_first() -> None:
    document_graph = build_cooccurrence_graph(_docs())
    assert document_graph.mode == "cooccurrence"

    results = query_graph(document_graph, "RRF 融合", 3)
    assert results
    assert results[0].chunk_id == "c2"
    assert results[0].source == "graph"


def test_query_expands_one_hop_neighbours() -> None:
    document_graph = build_cooccurrence_graph(
        [Chunk("a", "alpha beta"), Chunk("b", "beta gamma")]
    )
    ids = [chunk.chunk_id for chunk in query_graph(document_graph, "alpha", 2)]
    assert ids[0] == "a"  # direct entity match ranks first
    assert "b" in ids  # reached through the shared "beta" neighbour


def test_graph_falls_back_without_llm(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(graph.settings, "llm_base_url", "")
    monkeypatch.setattr(graph.settings, "llm_api_key", "")
    assert build_graph(_docs()).mode == "cooccurrence"


def test_llm_extraction_builds_entity_graph(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(graph.settings, "llm_base_url", "http://llm/v1")
    monkeypatch.setattr(graph.settings, "llm_api_key", "sk-test")
    payload = {
        "entities": ["NexusFlow", "RRF", "Missing"],
        "relations": [["NexusFlow", "RRF"], ["Missing", "RRF"]],
    }

    class _Resp:
        def raise_for_status(self) -> None:
            return None

        def json(self) -> dict:
            return {
                "choices": [
                    {"message": {"content": json.dumps(payload)}}
                ]
            }

    monkeypatch.setattr(graph.httpx, "post", lambda *a, **k: _Resp())

    document_graph = build_graph(_docs())
    assert document_graph.mode == "llm"
    assert "NexusFlow" in document_graph.nodes
    # Entities without chunk evidence are dropped.
    assert "Missing" not in document_graph.nodes
    assert ("NexusFlow", "RRF") in document_graph.edges


def test_llm_failure_falls_back_to_cooccurrence(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(graph.settings, "llm_base_url", "http://llm/v1")
    monkeypatch.setattr(graph.settings, "llm_api_key", "sk-test")

    def boom(*_args, **_kwargs):
        raise RuntimeError("llm down")

    monkeypatch.setattr(graph.httpx, "post", boom)
    assert build_graph(_docs()).mode == "cooccurrence"


def test_graph_extract_endpoint_reports_entities() -> None:
    from fastapi.testclient import TestClient

    from app.main import app

    client = TestClient(app)
    resp = client.post(
        "/v1/graph/extract",
        json={"markdown": "NexusFlow 使用 Go 编排。\n\n检索使用 RRF 融合。"},
    )
    assert resp.status_code == 200
    body = resp.json()
    assert body["success"] is True
    assert body["mode"] == "cooccurrence"
    terms = {entity["term"] for entity in body["entities"]}
    assert "nexusflow" in terms
    assert body["relations"]
