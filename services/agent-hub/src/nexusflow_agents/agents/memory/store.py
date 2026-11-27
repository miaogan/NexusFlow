from __future__ import annotations

import json
import logging
import threading
import time
import uuid
from collections.abc import Iterator
from datetime import UTC, datetime
from typing import Any, Protocol

from nexusflow_agents.agents.memory.rules import clean_content
from nexusflow_agents.agents.memory.similarity import cosine, lexical_score, vector_literal
from nexusflow_agents.agents.memory.vector_index import (
    index_statements,
    normalize_index_type,
    runtime_settings,
)

logger = logging.getLogger(__name__)


def _now() -> str:
    return datetime.now(UTC).isoformat()


def _iso(value: Any) -> str:
    if isinstance(value, datetime):
        return value.isoformat()
    return str(value)


class MemoryStore(Protocol):
    def upsert(
        self, tenant_id: str, user_id: str, candidates: list[dict[str, Any]]
    ) -> tuple[int, int]: ...

    def search(
        self,
        tenant_id: str,
        user_id: str,
        query_text: str,
        query_embedding: list[float] | None,
        top_k: int,
    ) -> list[dict[str, Any]]: ...

    def list(
        self, tenant_id: str, user_id: str, limit: int
    ) -> list[dict[str, Any]]: ...


class InMemoryMemoryStore:
    """Process-local memories, scoped by tenant + user."""

    def __init__(self, dedupe_threshold: float = 0.85) -> None:
        self._lock = threading.Lock()
        self._data: dict[tuple[str, str], list[dict[str, Any]]] = {}
        self._threshold = dedupe_threshold

    def upsert(
        self, tenant_id: str, user_id: str, candidates: list[dict[str, Any]]
    ) -> tuple[int, int]:
        created = merged = 0
        with self._lock:
            rows = self._data.setdefault((tenant_id, user_id), [])
            for candidate in candidates:
                content = clean_content(candidate.get("content") or "")
                if not content:
                    continue
                match = self._find(rows, content)
                if match is not None:
                    self._apply(match, candidate, content)
                    merged += 1
                else:
                    row = self._new_row(candidate, content)
                    rows.append(row)
                    created += 1
        return created, merged

    def _find(
        self, rows: list[dict[str, Any]], content: str
    ) -> dict[str, Any] | None:
        normalized = content.lower()
        for row in rows:
            if row["normalized"] == normalized:
                return row
            if lexical_score(row["content"], content) >= self._threshold:
                return row
        return None

    @staticmethod
    def _new_row(candidate: dict[str, Any], content: str) -> dict[str, Any]:
        ts = _now()
        return {
            "id": uuid.uuid4().hex,
            "content": content,
            "kind": candidate.get("kind") or "fact",
            "normalized": content.lower(),
            "embedding": candidate.get("embedding"),
            "created_at": ts,
            "updated_at": ts,
        }

    @staticmethod
    def _apply(
        row: dict[str, Any], candidate: dict[str, Any], content: str
    ) -> None:
        row["content"] = content
        row["normalized"] = content.lower()
        row["kind"] = candidate.get("kind") or row["kind"]
        if candidate.get("embedding"):
            row["embedding"] = candidate["embedding"]
        row["updated_at"] = _now()

    def search(
        self,
        tenant_id: str,
        user_id: str,
        query_text: str,
        query_embedding: list[float] | None,
        top_k: int,
    ) -> list[dict[str, Any]]:
        with self._lock:
            rows = [dict(row) for row in self._data.get((tenant_id, user_id), [])]
        scored: list[dict[str, Any]] = []
        for row in rows:
            if query_embedding is not None:
                score = cosine(query_embedding, row.get("embedding"))
            else:
                score = lexical_score(query_text, row["content"])
            if score > 0:
                scored.append({**row, "score": round(score, 4)})
        scored.sort(key=lambda item: (item["score"], item["updated_at"]), reverse=True)
        return scored[:top_k]

    def list(
        self, tenant_id: str, user_id: str, limit: int
    ) -> list[dict[str, Any]]:
        with self._lock:
            rows = [dict(row) for row in self._data.get((tenant_id, user_id), [])]
        rows.sort(key=lambda item: item["updated_at"], reverse=True)
        return [{**row, "score": 0.0} for row in rows[:limit]]


class PostgresMemoryStore:
    """Persists memories in ``rag_memories``.

    With embeddings enabled the table uses a pgvector column and cosine search;
    otherwise recall falls back to lexical scoring over recent rows.
    """

    def __init__(
        self,
        dsn: str,
        *,
        embedding_dim: int = 0,
        dedupe_threshold: float = 0.85,
        max_candidates: int = 300,
        vector_index: str = "hnsw",
        vector_index_recreate: bool = False,
        hnsw_m: int = 16,
        hnsw_ef_construction: int = 64,
        hnsw_ef_search: int = 40,
        ivfflat_lists: int = 100,
        ivfflat_probes: int = 10,
    ) -> None:
        self._dsn = dsn
        self._dim = embedding_dim
        self._threshold = dedupe_threshold
        self._max_candidates = max_candidates
        self._index_type = normalize_index_type(vector_index)
        self._index_recreate = vector_index_recreate
        self._hnsw_m = hnsw_m
        self._hnsw_ef_construction = hnsw_ef_construction
        self._hnsw_ef_search = hnsw_ef_search
        self._ivfflat_lists = ivfflat_lists
        self._ivfflat_probes = ivfflat_probes
        self._init_schema()

    def _connect(self):  # pragma: no cover - exercised against a real database
        import psycopg
        from psycopg.rows import dict_row

        return psycopg.connect(self._dsn, row_factory=dict_row)

    def _init_schema(self) -> None:  # pragma: no cover - real database only
        embedding_column = (
            f"embedding vector({self._dim})," if self._dim else ""
        )
        with self._connect() as conn:
            if self._dim:
                conn.execute("CREATE EXTENSION IF NOT EXISTS vector")
            conn.execute(
                f"""
                CREATE TABLE IF NOT EXISTS rag_memories (
                    id TEXT PRIMARY KEY,
                    tenant_id TEXT NOT NULL,
                    user_id TEXT NOT NULL,
                    kind TEXT NOT NULL,
                    content TEXT NOT NULL,
                    normalized TEXT NOT NULL,
                    {embedding_column}
                    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
                    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
                )
                """
            )
            if self._dim:
                conn.execute(
                    "ALTER TABLE rag_memories "
                    f"ADD COLUMN IF NOT EXISTS embedding vector({self._dim})"
                )
            conn.execute(
                "CREATE INDEX IF NOT EXISTS rag_memories_owner_idx "
                "ON rag_memories (tenant_id, user_id, updated_at DESC)"
            )
            statements = index_statements(
                index_type=self._index_type,
                dim=self._dim,
                m=self._hnsw_m,
                ef_construction=self._hnsw_ef_construction,
                lists=self._ivfflat_lists,
                recreate=self._index_recreate,
            )
            for statement in statements:
                try:
                    conn.execute(statement)
                except Exception as exc:  # noqa: BLE001 - index tuning is best-effort
                    logger.warning("vector index setup failed: %s", exc)

    def upsert(
        self, tenant_id: str, user_id: str, candidates: list[dict[str, Any]]
    ) -> tuple[int, int]:  # pragma: no cover - real database only
        created, merged, _ = self.upsert_detailed(tenant_id, user_id, candidates)
        return created, merged

    def upsert_detailed(
        self, tenant_id: str, user_id: str, candidates: list[dict[str, Any]]
    ) -> tuple[int, int, list[dict[str, Any]]]:  # pragma: no cover - real database only
        """Persist content and return the rows touched by this batch.

        The Qdrant driver uses the returned embeddings to update its vector
        index; Postgres remains the source of truth for content.
        """
        created = merged = 0
        changed: list[dict[str, Any]] = []
        with self._connect() as conn:
            rows = conn.execute(
                """
                SELECT id, content, kind, normalized
                FROM rag_memories
                WHERE tenant_id = %s AND user_id = %s
                ORDER BY updated_at DESC
                LIMIT %s
                """,
                (tenant_id, user_id, self._max_candidates),
            ).fetchall()
            existing = [dict(row) for row in rows]
            for candidate in candidates:
                content = clean_content(candidate.get("content") or "")
                if not content:
                    continue
                normalized = content.lower()
                match = self._find(existing, content)
                embedding = candidate.get("embedding")
                kind = candidate.get("kind") or "fact"
                if match is not None:
                    kind = candidate.get("kind") or match["kind"]
                    effective = {**candidate, "kind": kind}
                    self._update(conn, match["id"], effective, content)
                    match.update(
                        {
                            "content": content,
                            "kind": kind,
                            "normalized": normalized,
                        }
                    )
                    if embedding:
                        match["embedding"] = embedding
                    changed.append(
                        {
                            "id": match["id"],
                            "content": content,
                            "kind": kind,
                            "embedding": embedding,
                        }
                    )
                    merged += 1
                else:
                    new_id = uuid.uuid4().hex
                    self._insert(conn, new_id, tenant_id, user_id, candidate, content)
                    existing.append(
                        {
                            "id": new_id,
                            "content": content,
                            "kind": kind,
                            "normalized": normalized,
                        }
                    )
                    changed.append(
                        {
                            "id": new_id,
                            "content": content,
                            "kind": kind,
                            "embedding": embedding,
                        }
                    )
                    created += 1
        return created, merged, changed

    def _find(
        self, rows: list[dict[str, Any]], content: str
    ) -> dict[str, Any] | None:
        normalized = content.lower()
        for row in rows:
            if row["normalized"] == normalized:
                return row
            if lexical_score(row["content"], content) >= self._threshold:
                return row
        return None

    def _insert(
        self,
        conn: Any,
        memory_id: str,
        tenant_id: str,
        user_id: str,
        candidate: dict[str, Any],
        content: str,
    ) -> None:
        embedding = candidate.get("embedding")
        if self._dim and embedding:
            conn.execute(
                """
                INSERT INTO rag_memories
                    (id, tenant_id, user_id, kind, content, normalized, embedding)
                VALUES (%s, %s, %s, %s, %s, %s, %s::vector)
                """,
                (
                    memory_id,
                    tenant_id,
                    user_id,
                    candidate.get("kind") or "fact",
                    content,
                    content.lower(),
                    vector_literal(embedding),
                ),
            )
        else:
            conn.execute(
                """
                INSERT INTO rag_memories
                    (id, tenant_id, user_id, kind, content, normalized)
                VALUES (%s, %s, %s, %s, %s, %s)
                """,
                (
                    memory_id,
                    tenant_id,
                    user_id,
                    candidate.get("kind") or "fact",
                    content,
                    content.lower(),
                ),
            )

    def _update(
        self,
        conn: Any,
        memory_id: str,
        candidate: dict[str, Any],
        content: str,
    ) -> None:
        embedding = candidate.get("embedding")
        if self._dim and embedding:
            conn.execute(
                """
                UPDATE rag_memories
                SET kind = %s, content = %s, normalized = %s,
                    embedding = %s::vector, updated_at = now()
                WHERE id = %s
                """,
                (
                    candidate.get("kind") or "fact",
                    content,
                    content.lower(),
                    vector_literal(embedding),
                    memory_id,
                ),
            )
        else:
            conn.execute(
                """
                UPDATE rag_memories
                SET kind = %s, content = %s, normalized = %s, updated_at = now()
                WHERE id = %s
                """,
                (
                    candidate.get("kind") or "fact",
                    content,
                    content.lower(),
                    memory_id,
                ),
            )

    def _apply_runtime_settings(self, conn: Any) -> None:
        """Tunes recall for this connection (pgvector session GUCs)."""
        if not self._dim:
            return
        params = runtime_settings(
            index_type=self._index_type,
            ef_search=self._hnsw_ef_search,
            probes=self._ivfflat_probes,
        )
        for key, value in params.items():
            conn.execute("SELECT set_config(%s, %s, false)", (key, value))

    def search(
        self,
        tenant_id: str,
        user_id: str,
        query_text: str,
        query_embedding: list[float] | None,
        top_k: int,
    ) -> list[dict[str, Any]]:  # pragma: no cover - real database only
        with self._connect() as conn:
            self._apply_runtime_settings(conn)
            if self._dim and query_embedding:
                literal = vector_literal(query_embedding)
                rows = conn.execute(
                    """
                    SELECT id, content, kind,
                           1 - (embedding <=> %s::vector) AS score
                    FROM rag_memories
                    WHERE tenant_id = %s AND user_id = %s
                      AND embedding IS NOT NULL
                    ORDER BY embedding <=> %s::vector
                    LIMIT %s
                    """,
                    (literal, tenant_id, user_id, literal, top_k),
                ).fetchall()
                return [
                    {
                        "id": row["id"],
                        "content": row["content"],
                        "kind": row["kind"],
                        "score": round(float(row["score"]), 4),
                    }
                    for row in rows
                ]

            rows = conn.execute(
                """
                SELECT id, content, kind
                FROM rag_memories
                WHERE tenant_id = %s AND user_id = %s
                ORDER BY updated_at DESC
                LIMIT %s
                """,
                (tenant_id, user_id, self._max_candidates),
            ).fetchall()

        scored = [
            {
                "id": row["id"],
                "content": row["content"],
                "kind": row["kind"],
                "score": round(lexical_score(query_text, row["content"]), 4),
            }
            for row in rows
        ]
        scored = [item for item in scored if item["score"] > 0]
        scored.sort(key=lambda item: item["score"], reverse=True)
        return scored[:top_k]

    def fetch(
        self, tenant_id: str, user_id: str, ids: list[str]
    ) -> list[dict[str, Any]]:  # pragma: no cover - real database only
        """Load memory content for vector hits; Postgres is the source of truth."""
        if not ids:
            return []
        with self._connect() as conn:
            rows = conn.execute(
                """
                SELECT id, content, kind
                FROM rag_memories
                WHERE tenant_id = %s AND user_id = %s AND id = ANY(%s)
                """,
                (tenant_id, user_id, list(ids)),
            ).fetchall()
        return [
            {
                "id": row["id"],
                "content": row["content"],
                "kind": row["kind"],
            }
            for row in rows
        ]

    def iter_embeddings(
        self, batch_size: int = 500
    ) -> Iterator[dict[str, Any]]:  # pragma: no cover - real database only
        """Yield legacy pgvector rows for a one-time Qdrant backfill."""
        with self._connect() as conn:
            cursor = conn.execute(
                """
                SELECT id, tenant_id, user_id, kind, embedding::text AS embedding
                FROM rag_memories
                WHERE embedding IS NOT NULL
                ORDER BY updated_at
                """
            )
            while True:
                batch = cursor.fetchmany(batch_size)
                if not batch:
                    break
                for row in batch:
                    yield dict(row)

    def list(
        self, tenant_id: str, user_id: str, limit: int
    ) -> list[dict[str, Any]]:  # pragma: no cover - real database only
        with self._connect() as conn:
            rows = conn.execute(
                """
                SELECT id, content, kind
                FROM rag_memories
                WHERE tenant_id = %s AND user_id = %s
                ORDER BY updated_at DESC
                LIMIT %s
                """,
                (tenant_id, user_id, limit),
            ).fetchall()
        return [
            {
                "id": row["id"],
                "content": row["content"],
                "kind": row["kind"],
                "score": 0.0,
            }
            for row in rows
        ]


def _parse_vector(value: Any) -> list[float]:
    """Accepts a list or the ``[0.1,0.2]`` text form returned by pgvector."""
    if isinstance(value, (list, tuple)):
        return [float(item) for item in value]
    if not value:
        return []
    try:
        parsed = json.loads(str(value))
    except (TypeError, ValueError, json.JSONDecodeError):
        return []
    if not isinstance(parsed, list):
        return []
    return [float(item) for item in parsed]


class QdrantMemoryStore:
    """Memory content in Postgres, memory vectors in Qdrant.

    Postgres stays the source of truth; Qdrant holds only the embedding and the
    owner/filter fields. Recall queries Qdrant for ids and then fetches content
    from Postgres, falling back to lexical recall whenever Qdrant is unavailable
    or returns no usable hits.
    """

    def __init__(
        self,
        dsn: str,
        qdrant_url: str,
        *,
        collection: str = "nexusflow_memories",
        embedding_dim: int = 0,
        dedupe_threshold: float = 0.85,
        max_candidates: int = 300,
        content_store: Any | None = None,
        client: Any | None = None,
        ensure_collection: bool = True,
    ) -> None:
        self._dim = embedding_dim
        self._collection = collection
        self._qdrant_url = qdrant_url
        self._ensure_on_init = ensure_collection
        self._lock = threading.Lock()
        self._content = content_store or PostgresMemoryStore(
            dsn,
            embedding_dim=0,
            dedupe_threshold=dedupe_threshold,
            max_candidates=max_candidates,
        )
        self._client = client
        self._ready = False
        self._retry_at = 0.0
        self._try_init()

    def _try_init(self) -> bool:
        """Lazily (re)connect so a late Qdrant start does not disable vectors."""
        if self._ready:
            return True
        if not self._dim:
            return False
        with self._lock:
            if self._ready:
                return True
            now = time.monotonic()
            if now < self._retry_at:
                return False
            try:
                if self._client is None:
                    if not self._qdrant_url:
                        return False
                    from qdrant_client import QdrantClient

                    self._client = QdrantClient(url=self._qdrant_url)
                if self._ensure_on_init:
                    self._ensure_collection()
                self._ready = True
            except Exception as exc:  # noqa: BLE001 - lexical fallback stays available
                self._retry_at = now + 30.0
                logger.warning("qdrant memory store init failed: %s", exc)
                return False
        return True

    @property
    def ready(self) -> bool:
        return self._try_init()

    def _ensure_collection(self) -> None:
        from qdrant_client import models as qmodels

        if self._client.collection_exists(self._collection):
            return
        try:
            self._client.create_collection(
                collection_name=self._collection,
                vectors_config={
                    "dense": qmodels.VectorParams(
                        size=self._dim, distance=qmodels.Distance.COSINE
                    )
                },
            )
            logger.info("created qdrant memory collection %s", self._collection)
        except Exception:
            # Another replica may have created it between the check and create.
            if not self._client.collection_exists(self._collection):
                raise

    @staticmethod
    def point_id(memory_id: str) -> str:
        try:
            return str(uuid.UUID(memory_id))
        except (ValueError, AttributeError, TypeError):
            return str(uuid.uuid5(uuid.NAMESPACE_URL, str(memory_id)))

    def _build_points(
        self,
        rows: list[dict[str, Any]],
        tenant_id: str = "",
        user_id: str = "",
    ) -> list:
        from qdrant_client import models as qmodels

        points = []
        for row in rows:
            embedding = _parse_vector(row.get("embedding"))
            if not embedding:
                continue
            points.append(
                qmodels.PointStruct(
                    id=self.point_id(str(row["id"])),
                    vector={"dense": embedding},
                    payload={
                        "memory_id": str(row["id"]),
                        "tenant_id": str(row.get("tenant_id") or tenant_id),
                        "user_id": str(row.get("user_id") or user_id),
                        "kind": str(row.get("kind") or "fact"),
                    },
                )
            )
        return points

    def _upsert_points(
        self,
        rows: list[dict[str, Any]],
        tenant_id: str = "",
        user_id: str = "",
    ) -> int:
        points = self._build_points(rows, tenant_id, user_id)
        if points:
            self._client.upsert(
                collection_name=self._collection, points=points, wait=True
            )
        return len(points)

    def upsert(
        self, tenant_id: str, user_id: str, candidates: list[dict[str, Any]]
    ) -> tuple[int, int]:
        created, merged, rows = self._content.upsert_detailed(
            tenant_id, user_id, candidates
        )
        if rows and self._try_init():
            try:
                self._upsert_points(rows, tenant_id, user_id)
            except Exception as exc:  # noqa: BLE001 - content is already durable
                logger.warning(
                    "qdrant memory sync failed for %s/%s: %s", tenant_id, user_id, exc
                )
        return created, merged

    def _query_points(
        self,
        tenant_id: str,
        user_id: str,
        query_embedding: list[float],
        top_k: int,
    ) -> list[tuple[str, float]]:
        from qdrant_client import models as qmodels

        flt = qmodels.Filter(
            must=[
                qmodels.FieldCondition(
                    key="tenant_id", match=qmodels.MatchValue(value=tenant_id)
                ),
                qmodels.FieldCondition(
                    key="user_id", match=qmodels.MatchValue(value=user_id)
                ),
            ]
        )
        result = self._client.query_points(
            collection_name=self._collection,
            query=query_embedding,
            using="dense",
            query_filter=flt,
            limit=top_k,
            with_payload=True,
        )
        points = getattr(result, "points", result) or []
        hits: list[tuple[str, float]] = []
        for point in points:
            payload = point.payload or {}
            memory_id = payload.get("memory_id")
            if not memory_id:
                memory_id = str(point.id).replace("-", "")
            hits.append((str(memory_id), float(point.score or 0.0)))
        return hits

    def search(
        self,
        tenant_id: str,
        user_id: str,
        query_text: str,
        query_embedding: list[float] | None,
        top_k: int,
    ) -> list[dict[str, Any]]:
        if query_embedding and self._try_init():
            try:
                hits = self._query_points(tenant_id, user_id, query_embedding, top_k)
                if hits:
                    rows = self._content.fetch(
                        tenant_id, user_id, [memory_id for memory_id, _ in hits]
                    )
                    by_id = {str(row["id"]): row for row in rows}
                    out = []
                    for memory_id, score in hits:
                        row = by_id.get(memory_id)
                        if row is None:
                            continue
                        out.append(
                            {
                                "id": row["id"],
                                "content": row["content"],
                                "kind": row.get("kind") or "fact",
                                "score": round(score, 4),
                            }
                        )
                    if out:
                        return out
            except Exception as exc:  # noqa: BLE001 - lexical fallback stays available
                logger.warning(
                    "qdrant memory recall failed for %s/%s: %s",
                    tenant_id,
                    user_id,
                    exc,
                )
        return self._content.search(tenant_id, user_id, query_text, None, top_k)

    def list(
        self, tenant_id: str, user_id: str, limit: int
    ) -> list[dict[str, Any]]:
        return self._content.list(tenant_id, user_id, limit)

    def backfill(self, batch_size: int = 500) -> int:
        """Copy legacy pgvector embeddings into Qdrant (one-time migration)."""
        if not self._try_init():
            raise RuntimeError("qdrant memory store is not ready")
        total = 0
        batch: list[dict[str, Any]] = []
        try:
            for row in self._content.iter_embeddings(batch_size):
                batch.append(row)
                if len(batch) >= batch_size:
                    total += self._upsert_points(batch)
                    batch = []
            if batch:
                total += self._upsert_points(batch)
        except Exception as exc:  # noqa: BLE001 - report progress, keep old data
            logger.warning("memory vector backfill stopped after %d rows: %s", total, exc)
        return total
