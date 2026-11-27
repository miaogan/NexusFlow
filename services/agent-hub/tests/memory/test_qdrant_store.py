from __future__ import annotations

import sys
import types
from typing import Any

import pytest

from nexusflow_agents.agents.memory.core.config import Settings
from nexusflow_agents.agents.memory.deps import build_store
from nexusflow_agents.agents.memory.store import QdrantMemoryStore


def _install_fake_qdrant(monkeypatch: pytest.MonkeyPatch) -> None:
    class _VectorParams:
        def __init__(self, size: int, distance: str):
            self.size = size
            self.distance = distance

    class _Distance:
        COSINE = "cosine"

    class _PointStruct:
        def __init__(self, id: str, vector: dict[str, Any], payload: dict[str, Any]):
            self.id = id
            self.vector = vector
            self.payload = payload

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
        VectorParams=_VectorParams,
        Distance=_Distance,
        PointStruct=_PointStruct,
        MatchValue=_MatchValue,
        FieldCondition=_FieldCondition,
        Filter=_Filter,
    )
    monkeypatch.setitem(sys.modules, "qdrant_client", module)


class _Content:
    def __init__(self) -> None:
        self.upsert_args: tuple | None = None
        self.fetch_args: tuple | None = None
        self.search_args: tuple | None = None
        self.rows = [
            {
                "id": "a" * 32,
                "content": "我偏好简洁的回答",
                "kind": "preference",
                "embedding": [0.1, 0.2],
            }
        ]

    def upsert_detailed(
        self, tenant_id: str, user_id: str, candidates: list[dict[str, Any]]
    ) -> tuple[int, int, list[dict[str, Any]]]:
        self.upsert_args = (tenant_id, user_id, candidates)
        return 1, 0, self.rows

    def fetch(self, tenant_id: str, user_id: str, ids: list[str]) -> list[dict[str, Any]]:
        self.fetch_args = (tenant_id, user_id, ids)
        return [
            {"id": self.rows[0]["id"], "content": self.rows[0]["content"], "kind": "preference"}
        ]

    def search(
        self,
        tenant_id: str,
        user_id: str,
        query_text: str,
        query_embedding: list[float] | None,
        top_k: int,
    ) -> list[dict[str, Any]]:
        self.search_args = (tenant_id, user_id, query_text, query_embedding, top_k)
        return [
            {"id": "fallback", "content": "lexical fallback", "kind": "fact", "score": 0.3}
        ]

    def list(self, tenant_id: str, user_id: str, limit: int) -> list[dict[str, Any]]:
        return []

    def iter_embeddings(self, batch_size: int = 500):
        yield {
            "id": "b" * 32,
            "tenant_id": "t2",
            "user_id": "u2",
            "kind": "fact",
            "embedding": "[0.3, 0.4]",
        }


class _Point:
    def __init__(self, point_id: str, score: float, payload: dict[str, Any]):
        self.id = point_id
        self.score = score
        self.payload = payload


class _Response:
    def __init__(self, points: list[_Point] | None = None, error: Exception | None = None):
        self.points = points or []
        self.error = error


class _Client:
    def __init__(self, points: list[_Point] | None = None, fail: bool = False):
        self.created: list[dict[str, Any]] = []
        self.upserted: list[dict[str, Any]] = []
        self.queries: list[dict[str, Any]] = []
        self.points = points or []
        self.fail = fail
        self.exists = False

    def collection_exists(self, collection: str) -> bool:
        return self.exists

    def create_collection(self, **kwargs: Any) -> None:
        self.created.append(kwargs)
        self.exists = True

    def upsert(self, **kwargs: Any) -> None:
        if self.fail:
            raise RuntimeError("qdrant unavailable")
        self.upserted.append(kwargs)

    def query_points(self, **kwargs: Any) -> _Response:
        self.queries.append(kwargs)
        if self.fail:
            raise RuntimeError("qdrant unavailable")
        return _Response(self.points)


def _store(content: _Content, client: _Client) -> QdrantMemoryStore:
    return QdrantMemoryStore(
        "postgresql://example",
        "http://qdrant:6333",
        embedding_dim=2,
        content_store=content,
        client=client,
    )


def test_upsert_writes_content_then_vectors(monkeypatch: pytest.MonkeyPatch) -> None:
    _install_fake_qdrant(monkeypatch)
    content = _Content()
    client = _Client()

    store = _store(content, client)
    created, merged = store.upsert(
        "t1", "u1", [{"content": "我偏好简洁的回答", "kind": "preference", "embedding": [0.1, 0.2]}]
    )

    assert (created, merged) == (1, 0)
    assert client.created[0]["vectors_config"]["dense"].size == 2
    points = client.upserted[0]["points"]
    assert points[0].id == "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
    assert points[0].payload == {
        "memory_id": "a" * 32,
        "tenant_id": "t1",
        "user_id": "u1",
        "kind": "preference",
    }
    assert points[0].vector["dense"] == [0.1, 0.2]


def test_qdrant_failure_does_not_break_content_write(monkeypatch: pytest.MonkeyPatch) -> None:
    _install_fake_qdrant(monkeypatch)
    content = _Content()
    client = _Client(fail=True)
    store = _store(content, client)

    assert store.upsert("t1", "u1", [{"content": "x", "kind": "fact"}]) == (1, 0)
    assert content.upsert_args is not None


def test_search_uses_qdrant_ids_then_postgres_content(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    _install_fake_qdrant(monkeypatch)
    content = _Content()
    client = _Client(
        points=[_Point("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", 0.82, {"memory_id": "a" * 32})]
    )
    store = _store(content, client)

    hits = store.search("t1", "u1", "简洁回答", [0.1, 0.2], 3)

    assert hits == [
        {"id": "a" * 32, "content": "我偏好简洁的回答", "kind": "preference", "score": 0.82}
    ]
    assert content.fetch_args == ("t1", "u1", ["a" * 32])
    query = client.queries[0]
    assert query["using"] == "dense"
    assert [condition.key for condition in query["query_filter"].must] == [
        "tenant_id",
        "user_id",
    ]


def test_search_falls_back_to_lexical_when_qdrant_is_empty(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    _install_fake_qdrant(monkeypatch)
    content = _Content()
    store = _store(content, _Client())

    hits = store.search("t1", "u1", "简洁回答", [0.1, 0.2], 3)

    assert hits[0]["id"] == "fallback"
    assert content.search_args is not None
    assert content.search_args[3] is None


def test_search_falls_back_when_qdrant_fails(monkeypatch: pytest.MonkeyPatch) -> None:
    _install_fake_qdrant(monkeypatch)
    content = _Content()
    store = _store(content, _Client(fail=True))

    assert store.search("t1", "u1", "简洁回答", [0.1, 0.2], 3)[0]["id"] == "fallback"


def test_backfill_copies_legacy_embeddings(monkeypatch: pytest.MonkeyPatch) -> None:
    _install_fake_qdrant(monkeypatch)
    content = _Content()
    client = _Client()
    store = _store(content, client)

    total = store.backfill(batch_size=10)

    assert total == 1
    point = client.upserted[0]["points"][0]
    assert point.payload["memory_id"] == "b" * 32
    assert point.payload["tenant_id"] == "t2"
    assert point.payload["user_id"] == "u2"
    assert point.vector["dense"] == [0.3, 0.4]


def test_qdrant_driver_requires_qdrant_url() -> None:
    settings = Settings(
        _env_file=None,
        driver="qdrant",
        database_url="postgresql://example",
    )

    with pytest.raises(ValueError, match="MEMORY_QDRANT_URL"):
        build_store(settings)


def test_store_kind_auto_detection() -> None:
    assert Settings(_env_file=None, driver="").store_kind == "memory"
    assert (
        Settings(_env_file=None, driver="", database_url="postgresql://x").store_kind
        == "postgres"
    )
    assert (
        Settings(
            _env_file=None,
            driver="",
            database_url="postgresql://x",
            qdrant_url="http://qdrant:6333",
        ).store_kind
        == "qdrant"
    )
    assert (
        Settings(
            _env_file=None,
            driver="postgres",
            database_url="postgresql://x",
            qdrant_url="http://qdrant:6333",
        ).store_kind
        == "postgres"
    )
