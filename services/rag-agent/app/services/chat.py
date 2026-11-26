from __future__ import annotations

from typing import Any

from app.core.config import Settings
from app.graph import ChatGraph
from app.schemas import (
    ChatRequest,
    ChatResponse,
    ConversationDetail,
    ConversationListResponse,
    MessageOut,
)
from app.services.store import ConversationNotFound, ConversationStore


class ChatService:
    """Coordinates conversation persistence with the LangGraph pipeline."""

    def __init__(
        self, store: ConversationStore, graph: ChatGraph, settings: Settings
    ) -> None:
        self._store = store
        self._graph = graph
        self._settings = settings

    def chat(
        self,
        req: ChatRequest,
        tenant_id: str,
        user_id: str,
        auth_header: str | None,
    ) -> ChatResponse:
        message = req.message.strip()
        if req.conversation_id:
            conv = self._store.get(tenant_id, user_id, req.conversation_id)
            if conv is None:
                raise ConversationNotFound("conversation not found")
        else:
            conv = self._store.create(tenant_id, user_id, message[:40])

        history = [
            {"role": m["role"], "content": m["content"]}
            for m in conv.get("messages", [])
        ][-self._settings.max_history :]

        # Persist the user turn before generation so a failed generation still
        # leaves the question visible for a retry.
        self._store.append(conv["id"], "user", message, [])

        top_k = req.top_k or self._settings.default_top_k
        top_k = max(1, min(top_k, self._settings.max_top_k))
        state = self._graph.invoke(
            tenant_id=tenant_id,
            user_id=user_id,
            query=message,
            history=history,
            top_k=top_k,
            kb_id=req.kb_id,
            filters=req.filters,
            auth_header=auth_header,
        )

        answer = state.get("answer") or ""
        citations: list[dict[str, Any]] = state.get("citations") or []
        msg = self._store.append(conv["id"], "assistant", answer, citations)

        return ChatResponse(
            conversation_id=conv["id"],
            message_id=msg["id"],
            answer=answer,
            citations=citations,
            trace_id=state.get("trace_id"),
            created_at=msg["created_at"],
        )

    def list_conversations(
        self, tenant_id: str, user_id: str, limit: int, offset: int
    ) -> ConversationListResponse:
        items, total = self._store.list(tenant_id, user_id, limit, offset)
        return ConversationListResponse(
            conversations=items, total=total, limit=limit, offset=offset
        )

    def get_conversation(
        self, tenant_id: str, user_id: str, conversation_id: str
    ) -> ConversationDetail:
        conv = self._store.get(tenant_id, user_id, conversation_id)
        if conv is None:
            raise ConversationNotFound("conversation not found")
        messages = [
            MessageOut(
                id=m["id"],
                role=m["role"],
                content=m["content"],
                citations=m.get("citations") or [],
                created_at=m["created_at"],
            )
            for m in conv.get("messages", [])
        ]
        summary = {
            "id": conv["id"],
            "title": conv["title"],
            "message_count": conv.get("message_count") or len(messages),
            "created_at": conv["created_at"],
            "updated_at": conv["updated_at"],
        }
        return ConversationDetail(conversation=summary, messages=messages)
