from __future__ import annotations

import threading
import uuid
from datetime import UTC, datetime
from typing import Any, Protocol


def _now() -> str:
    return datetime.now(UTC).isoformat()


def _iso(value: Any) -> str:
    if isinstance(value, datetime):
        return value.isoformat()
    return str(value)


class ConversationNotFound(Exception):
    """Raised when a conversation does not exist for the given owner."""


class ConversationKBError(Exception):
    """Raised when a conversation is used from a different knowledge base."""


class ConversationStore(Protocol):
    def create(
        self, tenant_id: str, user_id: str, title: str, kb_id: str = "default"
    ) -> dict[str, Any]: ...

    def get(
        self, tenant_id: str, user_id: str, conversation_id: str
    ) -> dict[str, Any] | None: ...

    def append(
        self,
        conversation_id: str,
        role: str,
        content: str,
        citations: list[dict[str, Any]],
    ) -> dict[str, Any]: ...

    def set_summary(
        self, conversation_id: str, summary: str, summary_count: int
    ) -> None: ...

    def list(
        self,
        tenant_id: str,
        user_id: str,
        limit: int,
        offset: int,
        kb_id: str | None = None,
    ) -> tuple[list[dict[str, Any]], int]: ...


def _new_conversation(
    tenant_id: str, user_id: str, title: str, kb_id: str = "default"
) -> dict[str, Any]:
    ts = _now()
    return {
        "id": uuid.uuid4().hex,
        "tenant_id": tenant_id,
        "user_id": user_id,
        "kb_id": kb_id or "default",
        "title": title,
        "created_at": ts,
        "updated_at": ts,
        "message_count": 0,
        "messages": [],
        # Rolling summary of older turns; ``summary_count`` is the number of
        # leading messages already folded into it.
        "summary": "",
        "summary_count": 0,
    }


def _new_message(
    role: str, content: str, citations: list[dict[str, Any]]
) -> dict[str, Any]:
    return {
        "id": uuid.uuid4().hex,
        "role": role,
        "content": content,
        "citations": citations,
        "created_at": _now(),
    }


def _summary(conv: dict[str, Any]) -> dict[str, Any]:
    out = {k: conv[k] for k in ("id", "title", "created_at", "updated_at")}
    out["kb_id"] = conv.get("kb_id") or "default"
    out["message_count"] = conv.get("message_count") or len(conv.get("messages", []))
    return out


class InMemoryConversationStore:
    """Process-local store used when ``RAG_DATABASE_URL`` is unset."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._conversations: dict[str, dict[str, Any]] = {}

    def create(
        self, tenant_id: str, user_id: str, title: str, kb_id: str = "default"
    ) -> dict[str, Any]:
        conv = _new_conversation(tenant_id, user_id, title, kb_id)
        with self._lock:
            self._conversations[conv["id"]] = conv
        return _summary(conv)

    def get(
        self, tenant_id: str, user_id: str, conversation_id: str
    ) -> dict[str, Any] | None:
        with self._lock:
            conv = self._conversations.get(conversation_id)
            if conv is None or conv["tenant_id"] != tenant_id or conv["user_id"] != user_id:
                return None
            return {
                **_summary(conv),
                "messages": [dict(m) for m in conv["messages"]],
                "summary": conv.get("summary") or "",
                "summary_count": int(conv.get("summary_count") or 0),
            }

    def append(
        self,
        conversation_id: str,
        role: str,
        content: str,
        citations: list[dict[str, Any]],
    ) -> dict[str, Any]:
        msg = _new_message(role, content, citations)
        with self._lock:
            conv = self._conversations[conversation_id]
            conv["messages"].append(msg)
            conv["updated_at"] = msg["created_at"]
        return dict(msg)

    def set_summary(
        self, conversation_id: str, summary: str, summary_count: int
    ) -> None:
        with self._lock:
            conv = self._conversations.get(conversation_id)
            if conv is None:
                return
            conv["summary"] = summary
            conv["summary_count"] = summary_count

    def list(
        self,
        tenant_id: str,
        user_id: str,
        limit: int,
        offset: int,
        kb_id: str | None = None,
    ) -> tuple[list[dict[str, Any]], int]:
        with self._lock:
            owned = [
                c
                for c in self._conversations.values()
                if c["tenant_id"] == tenant_id
                and c["user_id"] == user_id
                and (kb_id is None or (c.get("kb_id") or "default") == kb_id)
            ]
            owned.sort(key=lambda c: (c["updated_at"], c["id"]), reverse=True)
            total = len(owned)
            page = owned[offset : offset + limit]
            items = [
                {**_summary(c), "message_count": len(c["messages"])} for c in page
            ]
        return items, total


class PostgresConversationStore:
    """Postgres-backed store: conversations + messages in two tables."""

    def __init__(self, dsn: str) -> None:
        self._dsn = dsn
        self._init_schema()

    def _connect(self):  # pragma: no cover - exercised against a real database
        import psycopg
        from psycopg.rows import dict_row

        return psycopg.connect(self._dsn, row_factory=dict_row)

    def _init_schema(self) -> None:  # pragma: no cover - real database only
        with self._connect() as conn:
            conn.execute(
                """
                CREATE TABLE IF NOT EXISTS rag_conversations (
                    id TEXT PRIMARY KEY,
                    tenant_id TEXT NOT NULL,
                    user_id TEXT NOT NULL,
                    kb_id TEXT NOT NULL DEFAULT 'default',
                    title TEXT NOT NULL,
                    summary TEXT NOT NULL DEFAULT '',
                    summary_count INTEGER NOT NULL DEFAULT 0,
                    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
                    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
                )
                """
            )
            # Isolate history per knowledge base; older rows default to "default".
            conn.execute(
                "ALTER TABLE rag_conversations "
                "ADD COLUMN IF NOT EXISTS kb_id TEXT NOT NULL DEFAULT 'default'"
            )
            # Add the compaction columns to databases created before summaries.
            conn.execute(
                "ALTER TABLE rag_conversations "
                "ADD COLUMN IF NOT EXISTS summary TEXT NOT NULL DEFAULT ''"
            )
            conn.execute(
                "ALTER TABLE rag_conversations "
                "ADD COLUMN IF NOT EXISTS summary_count INTEGER NOT NULL DEFAULT 0"
            )
            conn.execute(
                """
                CREATE INDEX IF NOT EXISTS rag_conversations_owner_idx
                ON rag_conversations (tenant_id, user_id, updated_at DESC)
                """
            )
            conn.execute(
                """
                CREATE INDEX IF NOT EXISTS rag_conversations_kb_idx
                ON rag_conversations (tenant_id, user_id, kb_id, updated_at DESC)
                """
            )
            conn.execute(
                """
                CREATE TABLE IF NOT EXISTS rag_messages (
                    id TEXT PRIMARY KEY,
                    conversation_id TEXT NOT NULL
                        REFERENCES rag_conversations(id) ON DELETE CASCADE,
                    role TEXT NOT NULL,
                    content TEXT NOT NULL,
                    citations JSONB NOT NULL DEFAULT '[]'::jsonb,
                    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
                )
                """
            )
            conn.execute(
                """
                CREATE INDEX IF NOT EXISTS rag_messages_conversation_idx
                ON rag_messages (conversation_id, created_at)
                """
            )

    def create(
        self, tenant_id: str, user_id: str, title: str, kb_id: str = "default"
    ) -> dict[str, Any]:
        conv_id = uuid.uuid4().hex
        with self._connect() as conn:
            row = conn.execute(
                """
                INSERT INTO rag_conversations (id, tenant_id, user_id, kb_id, title)
                VALUES (%s, %s, %s, %s, %s)
                RETURNING id, kb_id, title, created_at, updated_at
                """,
                (conv_id, tenant_id, user_id, kb_id or "default", title),
            ).fetchone()
        return {**_row(row), "message_count": 0}

    def get(
        self, tenant_id: str, user_id: str, conversation_id: str
    ) -> dict[str, Any] | None:
        with self._connect() as conn:
            conv = conn.execute(
                """
                SELECT id, kb_id, title, summary, summary_count, created_at, updated_at
                FROM rag_conversations
                WHERE id = %s AND tenant_id = %s AND user_id = %s
                """,
                (conversation_id, tenant_id, user_id),
            ).fetchone()
            if conv is None:
                return None
            rows = conn.execute(
                """
                SELECT id, role, content, citations, created_at
                FROM rag_messages
                WHERE conversation_id = %s
                ORDER BY created_at ASC, id ASC
                """,
                (conversation_id,),
            ).fetchall()
        messages = [_message_row(r) for r in rows]
        return {
            **_row(conv),
            "message_count": len(messages),
            "messages": messages,
            "summary": conv.get("summary") or "",
            "summary_count": int(conv.get("summary_count") or 0),
        }

    def append(
        self,
        conversation_id: str,
        role: str,
        content: str,
        citations: list[dict[str, Any]],
    ) -> dict[str, Any]:
        from psycopg.types.json import Jsonb

        msg_id = uuid.uuid4().hex
        with self._connect() as conn:
            row = conn.execute(
                """
                INSERT INTO rag_messages (id, conversation_id, role, content, citations)
                VALUES (%s, %s, %s, %s, %s)
                RETURNING id, role, content, citations, created_at
                """,
                (msg_id, conversation_id, role, content, Jsonb(citations)),
            ).fetchone()
            conn.execute(
                "UPDATE rag_conversations SET updated_at = now() WHERE id = %s",
                (conversation_id,),
            )
        return _message_row(row)

    def set_summary(
        self, conversation_id: str, summary: str, summary_count: int
    ) -> None:
        with self._connect() as conn:
            conn.execute(
                """
                UPDATE rag_conversations
                SET summary = %s, summary_count = %s, updated_at = now()
                WHERE id = %s
                """,
                (summary, summary_count, conversation_id),
            )

    def list(
        self,
        tenant_id: str,
        user_id: str,
        limit: int,
        offset: int,
        kb_id: str | None = None,
    ) -> tuple[list[dict[str, Any]], int]:
        where = "tenant_id = %s AND user_id = %s"
        params: list[Any] = [tenant_id, user_id]
        if kb_id:
            where += " AND kb_id = %s"
            params.append(kb_id)
        with self._connect() as conn:
            total = conn.execute(
                f"SELECT count(*) AS n FROM rag_conversations WHERE {where}",
                params,
            ).fetchone()["n"]
            rows = conn.execute(
                f"""
                SELECT c.id, c.kb_id, c.title, c.created_at, c.updated_at,
                       (SELECT count(*) FROM rag_messages m
                        WHERE m.conversation_id = c.id) AS message_count
                FROM rag_conversations c
                WHERE {where}
                ORDER BY c.updated_at DESC, c.id DESC
                LIMIT %s OFFSET %s
                """,
                [*params, limit, offset],
            ).fetchall()
        return [{**_row(r), "message_count": int(r["message_count"])} for r in rows], int(
            total
        )


def _row(row: dict[str, Any]) -> dict[str, Any]:
    return {
        "id": row["id"],
        "kb_id": row.get("kb_id") or "default",
        "title": row["title"],
        "created_at": _iso(row["created_at"]),
        "updated_at": _iso(row["updated_at"]),
    }


def _message_row(row: dict[str, Any]) -> dict[str, Any]:
    return {
        "id": row["id"],
        "role": row["role"],
        "content": row["content"],
        "citations": row.get("citations") or [],
        "created_at": _iso(row["created_at"]),
    }
