from __future__ import annotations

import threading
import uuid
from datetime import datetime, timezone
from typing import Any, Protocol

from app.rules import clean_content
from app.similarity import cosine, lexical_score, vector_literal


def _now() -> str:
    return datetime.now(timezone.utc).isoformat()


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
    ) -> None:
        self._dsn = dsn
        self._dim = embedding_dim
        self._threshold = dedupe_threshold
        self._max_candidates = max_candidates
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
            if self._dim:
                conn.execute(
                    "CREATE INDEX IF NOT EXISTS rag_memories_embedding_idx "
                    "ON rag_memories USING hnsw (embedding vector_cosine_ops)"
                )

    def upsert(
        self, tenant_id: str, user_id: str, candidates: list[dict[str, Any]]
    ) -> tuple[int, int]:  # pragma: no cover - real database only
        created = merged = 0
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
                if match is not None:
                    self._update(conn, match["id"], candidate, content)
                    match.update(
                        {
                            "content": content,
                            "kind": candidate.get("kind") or match["kind"],
                            "normalized": normalized,
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
                            "kind": candidate.get("kind") or "fact",
                            "normalized": normalized,
                        }
                    )
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

    def search(
        self,
        tenant_id: str,
        user_id: str,
        query_text: str,
        query_embedding: list[float] | None,
        top_k: int,
    ) -> list[dict[str, Any]]:  # pragma: no cover - real database only
        with self._connect() as conn:
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
