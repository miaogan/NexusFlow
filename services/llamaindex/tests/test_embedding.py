from __future__ import annotations

import json
from typing import Any

import pytest

from app.services import embedding as embedding_mod
from app.services.embedding import OpenAICompatibleEmbedding


class _Resp:
    def __init__(self, payload: dict[str, Any]):
        self._payload = payload

    def raise_for_status(self) -> None:
        return None

    def json(self) -> dict[str, Any]:
        return self._payload


def _capture(monkeypatch: pytest.MonkeyPatch) -> list[dict[str, Any]]:
    calls: list[dict[str, Any]] = []

    def fake_post(url, *, headers=None, json=None, timeout=None):
        calls.append({"url": url, "headers": headers, "json": json})
        inputs = json["input"]
        data = [
            {"index": idx, "embedding": [float(len(text)), 0.5]}
            for idx, text in enumerate(inputs)
        ]
        return _Resp({"data": data})

    monkeypatch.setattr(embedding_mod.httpx, "post", fake_post)
    return calls


def _client(batch: int = 2, key: str = "sk-test") -> OpenAICompatibleEmbedding:
    return OpenAICompatibleEmbedding(
        model_name="qwen3.7-text-embedding-flash",
        embed_batch_size=batch,
        api_base="https://dashscope.example/compatible-mode/v1",
        api_key=key,
    )


def test_batches_texts_and_preserves_order(monkeypatch: pytest.MonkeyPatch) -> None:
    calls = _capture(monkeypatch)

    vectors = _client(batch=2)._get_text_embeddings(["a", "bb", "ccc"])

    assert len(vectors) == 3
    assert vectors[0] == [1.0, 0.5]
    assert vectors[2] == [3.0, 0.5]
    assert len(calls) == 2
    assert calls[0]["json"] == {
        "model": "qwen3.7-text-embedding-flash",
        "input": ["a", "bb"],
    }
    assert calls[1]["json"]["input"] == ["ccc"]
    assert calls[0]["url"] == "https://dashscope.example/compatible-mode/v1/embeddings"
    assert calls[0]["headers"] == {"Authorization": "Bearer sk-test"}


def test_query_embedding_uses_single_input(monkeypatch: pytest.MonkeyPatch) -> None:
    calls = _capture(monkeypatch)
    vector = _client()._get_query_embedding("hello")
    assert vector == [5.0, 0.5]
    assert calls[0]["json"]["input"] == ["hello"]


def test_missing_key_raises() -> None:
    with pytest.raises(RuntimeError):
        _client(key="")._get_text_embedding("x")
