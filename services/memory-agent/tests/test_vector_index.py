from __future__ import annotations

from app.vector_index import (
    index_statements,
    normalize_index_type,
    runtime_settings,
)
from app.store import PostgresMemoryStore


class FakeConnection:
    def __init__(self) -> None:
        self.calls: list[tuple[str, tuple]] = []

    def execute(self, sql: str, params: tuple = ()) -> None:
        self.calls.append((sql, params))


def test_normalize_index_type_defaults_to_hnsw() -> None:
    assert normalize_index_type("HNSW") == "hnsw"
    assert normalize_index_type("bogus") == "hnsw"
    assert normalize_index_type("none") == "none"


def test_hnsw_ddl_uses_tuned_parameters() -> None:
    statements = index_statements(
        index_type="hnsw",
        dim=1536,
        m=32,
        ef_construction=200,
        lists=100,
    )

    assert len(statements) == 1
    sql = statements[0]
    assert "USING hnsw" in sql
    assert "m = 32" in sql
    assert "ef_construction = 200" in sql


def test_ivfflat_ddl_recreates_index() -> None:
    statements = index_statements(
        index_type="ivfflat",
        dim=1536,
        m=16,
        ef_construction=64,
        lists=200,
        recreate=True,
    )

    assert statements[0].startswith("DROP INDEX")
    assert "USING ivfflat" in statements[1]
    assert "lists = 200" in statements[1]


def test_none_or_missing_dim_skips_index() -> None:
    assert (
        index_statements(
            index_type="none", dim=1536, m=16, ef_construction=64, lists=100
        )
        == []
    )
    assert (
        index_statements(
            index_type="hnsw", dim=0, m=16, ef_construction=64, lists=100
        )
        == []
    )


def test_runtime_settings_map_to_pgvector_gucs() -> None:
    assert runtime_settings(index_type="hnsw", ef_search=80, probes=10) == {
        "hnsw.ef_search": "80"
    }
    assert runtime_settings(index_type="ivfflat", ef_search=40, probes=24) == {
        "ivfflat.probes": "24"
    }
    assert runtime_settings(index_type="none", ef_search=40, probes=10) == {}


def test_parameters_are_clamped() -> None:
    sql = index_statements(
        index_type="hnsw",
        dim=1536,
        m=999,
        ef_construction=9999,
        lists=1,
    )[0]
    assert "m = 64" in sql
    assert "ef_construction = 512" in sql
    assert runtime_settings(index_type="hnsw", ef_search=0, probes=0) == {
        "hnsw.ef_search": "1"
    }


def test_runtime_settings_are_applied_to_connection() -> None:
    store = object.__new__(PostgresMemoryStore)
    store._dim = 1536
    store._index_type = "hnsw"
    store._hnsw_ef_search = 80
    store._ivfflat_probes = 10
    conn = FakeConnection()

    store._apply_runtime_settings(conn)  # type: ignore[arg-type]

    assert conn.calls == [
        ("SELECT set_config(%s, %s, false)", ("hnsw.ef_search", "80"))
    ]


def test_runtime_settings_skipped_without_embeddings() -> None:
    store = object.__new__(PostgresMemoryStore)
    store._dim = 0
    conn = FakeConnection()

    store._apply_runtime_settings(conn)  # type: ignore[arg-type]

    assert conn.calls == []
