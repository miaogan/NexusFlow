from __future__ import annotations

from typing import Any

import httpx
import pytest

from app.services import corrected
from app.services.corrected import HttpCorrectedAnswerProvider


class _Resp:
    def __init__(
        self,
        status_code: int,
        payload: dict[str, Any] | None = None,
        invalid_json: bool = False,
    ):
        self.status_code = status_code
        self._payload = payload or {}
        self._invalid = invalid_json

    @property
    def is_error(self) -> bool:
        return self.status_code >= 400

    def json(self) -> dict[str, Any]:
        if self._invalid:
            raise ValueError("not json")
        return self._payload


def _capture_get(monkeypatch: pytest.MonkeyPatch, resp: _Resp) -> list[dict[str, Any]]:
    calls: list[dict[str, Any]] = []

    def fake_get(url, *, params=None, headers=None, timeout=None):
        calls.append({"url": url, "params": params, "headers": headers, "timeout": timeout})
        return resp

    monkeypatch.setattr(corrected.httpx, "get", fake_get)
    return calls


def test_lookup_returns_matched_corrections(monkeypatch: pytest.MonkeyPatch) -> None:
    payload = {
        "items": [
            {"query": "怎么部署", "corrected_answer": "使用 make up。", "match_score": 0.9},
            {"query": "空答案", "corrected_answer": "   ", "match_score": 1.0},
        ],
        "count": 2,
    }
    calls = _capture_get(monkeypatch, _Resp(200, payload))
    provider = HttpCorrectedAnswerProvider("http://gw", 2.0, 3)

    rows = provider.lookup(
        query="怎么部署 NexusFlow", tenant_id="t1", auth_header="Bearer token"
    )

    assert rows == [{"query": "怎么部署", "answer": "使用 make up。", "score": 0.9}]
    assert calls[0]["url"] == "http://gw/v1/corrected-answers"
    assert calls[0]["params"] == {"query": "怎么部署 NexusFlow", "limit": 3}
    assert calls[0]["headers"] == {
        "x-tenant-id": "t1",
        "Authorization": "Bearer token",
    }


def test_lookup_is_fail_open(monkeypatch: pytest.MonkeyPatch) -> None:
    provider = HttpCorrectedAnswerProvider("http://gw")

    assert provider.lookup(query="q", tenant_id="t1") == []

    # HTTP error status -> no corrections.
    monkeypatch.setattr(corrected.httpx, "get", lambda *a, **k: _Resp(503))
    assert provider.lookup(query="q", tenant_id="t1") == []

    # Transport error -> no corrections.
    def boom(*a, **k):
        raise httpx.ConnectError("down")

    monkeypatch.setattr(corrected.httpx, "get", boom)
    assert provider.lookup(query="q", tenant_id="t1") == []

    # Malformed body -> no corrections.
    monkeypatch.setattr(
        corrected.httpx, "get", lambda *a, **k: _Resp(200, invalid_json=True)
    )
    assert provider.lookup(query="q", tenant_id="t1") == []


def test_lookup_without_gateway_or_query_is_skipped() -> None:
    assert HttpCorrectedAnswerProvider("").lookup(query="q", tenant_id="t1") == []
    assert (
        HttpCorrectedAnswerProvider("http://gw").lookup(query="  ", tenant_id="t1")
        == []
    )
