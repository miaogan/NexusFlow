from __future__ import annotations

from typing import Any

import pytest

from nexusflow_agents.agents.rag.services import retrieval
from nexusflow_agents.agents.rag.services.retrieval import GatewayRetriever, RetrievalUnavailable


class _Resp:
    def __init__(self, status_code: int, text: str, payload: dict[str, Any] | None = None):
        self.status_code = status_code
        self.text = text
        self._payload = payload or {}

    @property
    def is_error(self) -> bool:
        return self.status_code >= 400

    def json(self) -> dict[str, Any]:
        return self._payload


def test_returns_chunks_and_trace(monkeypatch: pytest.MonkeyPatch) -> None:
    payload = {
        "chunks": [{"chunk_id": "c1", "content": "x", "score": 0.5, "meta": {}}],
        "trace_id": "tr-1",
    }
    monkeypatch.setattr(retrieval.httpx, "post", lambda *a, **k: _Resp(200, "ok", payload))

    chunks, trace = GatewayRetriever("http://gw").search(
        query="q", tenant_id="t1", top_k=5
    )
    assert chunks[0]["chunk_id"] == "c1"
    assert trace == "tr-1"


def test_empty_recall_is_treated_as_no_context(monkeypatch: pytest.MonkeyPatch) -> None:
    resp = _Resp(502, "rpc error: no recall route produced results")
    monkeypatch.setattr(retrieval.httpx, "post", lambda *a, **k: resp)

    chunks, trace = GatewayRetriever("http://gw").search(
        query="q", tenant_id="t1", top_k=5
    )
    assert chunks == []
    assert trace is None


def test_other_errors_raise(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(
        retrieval.httpx, "post", lambda *a, **k: _Resp(500, "boom")
    )
    with pytest.raises(RetrievalUnavailable):
        GatewayRetriever("http://gw").search(query="q", tenant_id="t1", top_k=5)
