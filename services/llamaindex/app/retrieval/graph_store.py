"""Persistent LightRAG-style graph snapshots.

The graph route builds an in-memory ``DocumentGraph`` from Qdrant chunks. With
``LLAMAINDEX_GRAPH_STORE_DRIVER=postgres`` the snapshot is stored in PostgreSQL
so restarts and replicas reuse it instead of re-scrolling Qdrant and re-running
entity extraction. Payloads are keyed by a tenant-scoped collection string, so
graphs never cross tenant boundaries.
"""
from __future__ import annotations

import logging
from functools import lru_cache
from typing import Any, Protocol

from app.core.config import settings
from app.retrieval.fusion import Chunk
from app.retrieval.graph import DocumentGraph, GraphNode

logger = logging.getLogger(__name__)

GRAPH_STORE_ADVISORY_LOCK = 731006


class GraphStore(Protocol):
    def load(self, scope: str) -> dict[str, Any] | None: ...

    def save(self, scope: str, payload: dict[str, Any]) -> None: ...

    def delete(self, scope: str) -> None: ...

    def delete_prefix(self, prefix: str) -> None: ...

    def clear(self) -> None: ...


def graph_to_payload(graph: DocumentGraph) -> dict[str, Any]:
    """Converts a graph to JSON-safe primitives (sets and tuples expanded)."""
    return {
        "mode": graph.mode,
        "chunks": [
            {
                "id": chunk.chunk_id,
                "content": chunk.content,
                "score": chunk.score,
                "meta": dict(chunk.meta),
                "source": chunk.source,
            }
            for chunk in graph.chunks.values()
        ],
        "nodes": [
            {
                "term": node.term,
                "chunk_ids": sorted(node.chunk_ids),
                "weight": node.weight,
            }
            for node in graph.nodes.values()
        ],
        "edges": [
            {"source": source, "target": target, "weight": weight}
            for (source, target), weight in graph.edges.items()
        ],
    }


def graph_from_payload(payload: dict[str, Any]) -> DocumentGraph:
    graph = DocumentGraph(mode=str(payload.get("mode") or "cooccurrence"))
    for item in payload.get("chunks") or []:
        chunk_id = str(item.get("id") or "")
        if not chunk_id:
            continue
        meta = item.get("meta") or {}
        graph.chunks[chunk_id] = Chunk(
            chunk_id=chunk_id,
            content=str(item.get("content") or ""),
            score=float(item.get("score") or 0.0),
            meta={str(key): str(value) for key, value in dict(meta).items()},
            source=str(item.get("source") or "graph"),
        )
    for item in payload.get("nodes") or []:
        term = str(item.get("term") or "")
        if not term:
            continue
        graph.nodes[term] = GraphNode(
            term=term,
            chunk_ids={str(cid) for cid in item.get("chunk_ids") or [] if cid},
            weight=float(item.get("weight") or 0.0),
        )
    for item in payload.get("edges") or []:
        source = str(item.get("source") or "")
        target = str(item.get("target") or "")
        if not source or not target:
            continue
        graph.add_edge(source, target, float(item.get("weight") or 1.0))
    return graph


class PostgresGraphStore:
    """Graph snapshots in the ``rag_graph_snapshots`` table.

    Failures degrade to a cache miss so the graph route always rebuilds rather
    than failing the query.
    """

    def __init__(self, dsn: str) -> None:
        self._dsn = dsn
        self._init_schema()

    def _connect(self):  # pragma: no cover - real database only
        import psycopg
        from psycopg.rows import dict_row

        return psycopg.connect(self._dsn, row_factory=dict_row)

    def _init_schema(self) -> None:  # pragma: no cover - real database only
        with self._connect() as conn:
            conn.execute("SELECT pg_advisory_xact_lock(%s)", (GRAPH_STORE_ADVISORY_LOCK,))
            conn.execute(
                """
                CREATE TABLE IF NOT EXISTS rag_graph_snapshots (
                    scope      VARCHAR(256) PRIMARY KEY,
                    payload    JSONB NOT NULL,
                    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
                )
                """
            )
            conn.execute(
                """
                CREATE INDEX IF NOT EXISTS idx_rag_graph_snapshots_updated
                ON rag_graph_snapshots (updated_at)
                """
            )

    def load(self, scope: str) -> dict[str, Any] | None:  # pragma: no cover - real DB
        try:
            with self._connect() as conn:
                row = conn.execute(
                    "SELECT payload FROM rag_graph_snapshots WHERE scope = %s",
                    (scope,),
                ).fetchone()
        except Exception as exc:  # noqa: BLE001 - degrade to a cache miss
            logger.warning("graph snapshot load failed: %s", exc)
            return None
        payload = row["payload"] if row else None
        return dict(payload) if isinstance(payload, dict) else None

    def save(self, scope: str, payload: dict[str, Any]) -> None:  # pragma: no cover - real DB
        from psycopg.types.json import Jsonb

        try:
            with self._connect() as conn:
                conn.execute(
                    """
                    INSERT INTO rag_graph_snapshots (scope, payload, updated_at)
                    VALUES (%s, %s, NOW())
                    ON CONFLICT (scope) DO UPDATE
                    SET payload = EXCLUDED.payload, updated_at = NOW()
                    """,
                    (scope, Jsonb(payload)),
                )
        except Exception as exc:  # noqa: BLE001 - cache write is best-effort
            logger.warning("graph snapshot save failed: %s", exc)

    def delete(self, scope: str) -> None:  # pragma: no cover - real database only
        try:
            with self._connect() as conn:
                conn.execute("DELETE FROM rag_graph_snapshots WHERE scope = %s", (scope,))
        except Exception as exc:  # noqa: BLE001 - best-effort cleanup
            logger.warning("graph snapshot delete failed: %s", exc)

    def delete_prefix(self, prefix: str) -> None:  # pragma: no cover - real DB
        try:
            with self._connect() as conn:
                conn.execute(
                    "DELETE FROM rag_graph_snapshots WHERE LEFT(scope, %s) = %s",
                    (len(prefix), prefix),
                )
        except Exception as exc:  # noqa: BLE001 - best-effort cleanup
            logger.warning("graph snapshot prefix delete failed: %s", exc)

    def clear(self) -> None:  # pragma: no cover - test/maintenance helper
        try:
            with self._connect() as conn:
                conn.execute("DELETE FROM rag_graph_snapshots")
        except Exception as exc:  # noqa: BLE001 - best-effort cleanup
            logger.warning("graph snapshot clear failed: %s", exc)


@lru_cache
def get_graph_store() -> GraphStore | None:
    """Returns the configured store, or ``None`` to keep the in-memory cache."""
    if settings.graph_store_driver.strip().lower() != "postgres":
        return None
    if not settings.graph_database_url.strip():
        logger.warning(
            "graph store driver is postgres but LLAMAINDEX_GRAPH_DATABASE_URL is empty"
        )
        return None
    try:
        return PostgresGraphStore(settings.graph_database_url)
    except Exception as exc:  # noqa: BLE001 - keep retrieval available
        logger.warning("graph database unavailable, using in-memory cache: %s", exc)
        return None
