from __future__ import annotations

import json
from typing import Any

import pytest

from app.retrieval import rerank as rerank_mod
from app.retrieval.fusion import Chunk
from app.retrieval.rerank import rerank


class _Resp:
    def __init__(self, payload: dict[str, Any]):
        self.text = json.dumps(payload)

    def raise_for_status(self) -> None:
        return None


def _capture(monkeypatch: pytest.MonkeyPatch, payload: dict[str, Any]) -> list[dict[str, Any]]:
    calls: list[dict[str, Any]] = []

    def fake_post(url, *, json=None, headers=None, timeout=None):
        calls.append({"url": url, "json": json, "headers": headers, "timeout": timeout})
        return _Resp(payload)

    monkeypatch.setattr(rerank_mod.httpx, "post", fake_post)
    monkeypatch.setattr(rerank_mod.settings, "rerank_url", "http://rerank.local")
    return calls


def test_generic_protocol_payload_and_order(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(rerank_mod.settings, "rerank_protocol", "generic")
    monkeypatch.setattr(rerank_mod.settings, "rerank_api_key", "")
    monkeypatch.setattr(rerank_mod.settings, "rerank_pool", 0)
    calls = _capture(
        monkeypatch,
        {"results": [{"index": 1, "relevance_score": 0.9}, {"index": 0, "relevance_score": 0.2}]},
    )

    out = rerank("q", [Chunk("a", "A"), Chunk("b", "B")], "bge", 2)

    assert [c.chunk_id for c in out] == ["b", "a"]
    assert calls[0]["json"] == {
        "model": "bge",
        "query": "q",
        "documents": ["A", "B"],
        "top_n": 2,
    }
    assert calls[0]["headers"] is None


def test_dashscope_protocol_payload_and_auth(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(rerank_mod.settings, "rerank_protocol", "dashscope")
    monkeypatch.setattr(rerank_mod.settings, "rerank_api_key", "sk-ws-test")
    monkeypatch.setattr(rerank_mod.settings, "rerank_pool", 0)
    calls = _capture(
        monkeypatch,
        {"output": {"results": [{"index": 0, "relevance_score": 0.8}]}},
    )

    out = rerank("怎么部署", [Chunk("a", "使用 make up")], "qwen3.7-text-rerank", 1)

    assert len(out) == 1 and out[0].chunk_id == "a" and out[0].score == 0.8
    assert calls[0]["headers"] == {"Authorization": "Bearer sk-ws-test"}
    body = calls[0]["json"]
    assert body["model"] == "qwen3.7-text-rerank"
    assert body["input"] == {"query": "怎么部署", "documents": ["使用 make up"]}
    assert body["parameters"] == {"return_documents": False, "top_n": 1}


def test_pool_truncates_candidates(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(rerank_mod.settings, "rerank_protocol", "generic")
    monkeypatch.setattr(rerank_mod.settings, "rerank_api_key", "")
    monkeypatch.setattr(rerank_mod.settings, "rerank_pool", 2)
    calls = _capture(monkeypatch, {"results": [{"index": 1, "relevance_score": 0.5}]})

    out = rerank("q", [Chunk("a", "1"), Chunk("b", "2"), Chunk("c", "3")], "bge", 2)

    assert calls[0]["json"]["documents"] == ["1", "2"]
    assert [c.chunk_id for c in out] == ["b"]


def test_missing_url_raises(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(rerank_mod.settings, "rerank_url", "")
    with pytest.raises(RuntimeError):
        rerank("q", [Chunk("a", "A")], "bge", 1)
