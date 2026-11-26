from __future__ import annotations

from collections.abc import AsyncIterator
from typing import Any

from starlette.concurrency import run_in_threadpool

from app.core.config import Settings
from app.graph import ChatGraph
from app.schemas import (
    ChatRequest,
    ChatResponse,
    ConversationDetail,
    ConversationListResponse,
    MessageOut,
)
from app.services.corrected import (
    CorrectedAnswerProvider,
    NullCorrectedAnswerProvider,
)
from app.services.intent import HttpIntentClassifier, IntentClassifier
from app.services.memory import ConversationMemory, HttpMemoryClient
from app.services.retrieval import RetrievalUnavailable
from app.services.store import ConversationNotFound, ConversationStore
from app.services.summary import ConversationSummarizer, HttpSummarizer


class ChatService:
    """Coordinates conversation persistence with the LangGraph pipeline."""

    def __init__(
        self,
        store: ConversationStore,
        graph: ChatGraph,
        settings: Settings,
        intent: IntentClassifier | None = None,
        summarizer: ConversationSummarizer | None = None,
        memory: ConversationMemory | None = None,
        corrections: CorrectedAnswerProvider | None = None,
    ) -> None:
        self._store = store
        self._graph = graph
        self._settings = settings
        # Default classifier degrades to knowledge QA without a configured URL.
        self._intent: IntentClassifier = intent or HttpIntentClassifier("")
        # Default summarizer is a no-op without a configured URL.
        self._summarizer: ConversationSummarizer = summarizer or HttpSummarizer("")
        # Default memory client is a no-op without a configured URL.
        self._memory: ConversationMemory = memory or HttpMemoryClient("")
        # Default correction lookup is a no-op without a configured gateway.
        self._corrections: CorrectedAnswerProvider = (
            corrections or NullCorrectedAnswerProvider()
        )

    def _prepare_history(
        self, conv: dict[str, Any]
    ) -> tuple[list[dict[str, str]], str]:
        """Returns the recent window plus a rolling summary of older turns.

        Once the conversation outgrows ``max_history``, the messages between
        the already-summarized prefix and the recent window are folded into the
        summary. If the summary agent is unavailable the raw window is kept, so
        a summary outage only costs context, never correctness.
        """
        messages = conv.get("messages", [])
        summary = conv.get("summary") or ""
        count = int(conv.get("summary_count") or 0)
        limit = self._settings.max_history
        total = len(messages)

        if total > limit:
            recent = min(limit, max(2, self._settings.summary_recent_messages))
            end = total - recent
            if end > count:
                folded = [
                    {"role": m["role"], "content": m["content"]}
                    for m in messages[count:end]
                ]
                new_summary = self._summarizer.summarize(summary, folded)
                if new_summary:
                    summary = new_summary
                    count = end
                    self._store.set_summary(conv["id"], summary, count)

        start = count if total - count <= limit else total - limit
        history = [
            {"role": m["role"], "content": m["content"]}
            for m in messages[start:]
        ]
        return history, summary

    def _remember(
        self, tenant_id: str, user_id: str, message: str, answer: str
    ) -> None:
        """Fires cross-session memory extraction without blocking the reply."""
        self._memory.extract_async(
            tenant_id,
            user_id,
            [
                {"role": "user", "content": message},
                {"role": "assistant", "content": answer},
            ],
        )

    def _corrected_answers(
        self, message: str, tenant_id: str, auth_header: str | None
    ) -> list[dict[str, Any]]:
        """Matches historical corrected answers before retrieval.

        Corrections are best-effort: any lookup failure returns no corrections
        so a review-store outage only costs answer quality, never availability.
        """
        try:
            return (
                self._corrections.lookup(
                    query=message, tenant_id=tenant_id, auth_header=auth_header
                )
                or []
            )
        except Exception:  # noqa: BLE001 - corrections must never fail a turn
            return []

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

        history, summary = self._prepare_history(conv)
        memories = self._memory.recall(
            tenant_id, user_id, message, self._settings.memory_top_k
        )

        # Persist the user turn before generation so a failed generation still
        # leaves the question visible for a retry.
        self._store.append(conv["id"], "user", message, [])

        top_k = req.top_k or self._settings.default_top_k
        top_k = max(1, min(top_k, self._settings.max_top_k))

        intent = self._intent.classify(message, history)
        if intent.get("need_retrieval", True):
            corrected = self._corrected_answers(message, tenant_id, auth_header)
            state = self._graph.invoke(
                tenant_id=tenant_id,
                user_id=user_id,
                query=message,
                history=history,
                summary=summary,
                memories=memories,
                top_k=top_k,
                kb_id=req.kb_id,
                filters=req.filters,
                auth_header=auth_header,
                corrected=corrected,
            )
            answer = state.get("answer") or ""
            citations: list[dict[str, Any]] = state.get("citations") or []
            trace_id = state.get("trace_id")
        else:
            answer = self._graph.generate_intent_answer(
                str(intent.get("intent") or "clarify"),
                message,
                history,
                summary=summary,
                memories=memories,
            )
            citations = []
            trace_id = None

        msg = self._store.append(conv["id"], "assistant", answer, citations)
        self._remember(tenant_id, user_id, message, answer)

        return ChatResponse(
            conversation_id=conv["id"],
            message_id=msg["id"],
            answer=answer,
            citations=citations,
            trace_id=trace_id,
            created_at=msg["created_at"],
        )

    async def chat_stream(
        self,
        req: ChatRequest,
        tenant_id: str,
        user_id: str,
        auth_header: str | None,
    ) -> AsyncIterator[dict[str, Any]]:
        """Streams pipeline events: retrieve → sources → generate tokens → done.

        Event shapes are consumed by the Go gateway (SSE relay) and the web
        console (stage timeline, source panel, inline [n] jumps).
        """
        message = req.message.strip()
        if req.conversation_id:
            conv = await run_in_threadpool(
                self._store.get, tenant_id, user_id, req.conversation_id
            )
            if conv is None:
                yield {"type": "error", "error": "conversation not found"}
                return
        else:
            conv = await run_in_threadpool(
                self._store.create, tenant_id, user_id, message[:40]
            )

        history, summary = await run_in_threadpool(self._prepare_history, conv)
        memories = await run_in_threadpool(
            self._memory.recall,
            tenant_id,
            user_id,
            message,
            self._settings.memory_top_k,
        )
        await run_in_threadpool(self._store.append, conv["id"], "user", message, [])

        top_k = req.top_k or self._settings.default_top_k
        top_k = max(1, min(top_k, self._settings.max_top_k))

        yield {"type": "stage", "stage": "intent", "status": "start"}
        intent = await run_in_threadpool(self._intent.classify, message, history)
        yield {"type": "intent", **intent}
        yield {"type": "stage", "stage": "intent", "status": "done"}

        # chitchat/clarify turns answer directly; only knowledge QA retrieves.
        if not intent.get("need_retrieval", True):
            yield {"type": "stage", "stage": "generate", "status": "start"}
            intent_parts: list[str] = []
            try:
                async for piece in self._graph.stream_intent_answer(
                    str(intent.get("intent") or "clarify"),
                    message,
                    history,
                    summary=summary,
                    memories=memories,
                ):
                    intent_parts.append(piece)
                    yield {"type": "token", "content": piece}
            except Exception as exc:  # noqa: BLE001 - surface upstream LLM failures
                yield {"type": "error", "error": f"generation failed: {exc}"}
                return

            answer = "".join(intent_parts)
            msg = await run_in_threadpool(
                self._store.append, conv["id"], "assistant", answer, []
            )
            self._remember(tenant_id, user_id, message, answer)
            yield {
                "type": "done",
                "conversation_id": conv["id"],
                "message_id": msg["id"],
                "answer": answer,
                "citations": [],
                "trace_id": None,
                "created_at": msg["created_at"],
            }
            return

        yield {"type": "stage", "stage": "retrieve", "status": "start"}
        corrected = await run_in_threadpool(
            self._corrected_answers, message, tenant_id, auth_header
        )
        try:
            retrieved = await run_in_threadpool(
                self._graph.retrieve,
                query=message,
                tenant_id=tenant_id,
                top_k=top_k,
                kb_id=req.kb_id,
                filters=req.filters,
                auth_header=auth_header,
            )
        except RetrievalUnavailable as exc:
            yield {"type": "error", "error": str(exc)}
            return

        chunks: list[dict[str, Any]] = retrieved.get("chunks") or []
        citations = self._graph.citations(chunks)
        yield {"type": "sources", "citations": citations}
        yield {
            "type": "stage",
            "stage": "retrieve",
            "status": "done",
            "count": len(citations),
        }
        yield {"type": "stage", "stage": "generate", "status": "start"}

        parts: list[str] = []
        try:
            async for piece in self._graph.stream_answer(
                message,
                history,
                chunks,
                summary=summary,
                memories=memories,
                corrected=corrected,
            ):
                parts.append(piece)
                yield {"type": "token", "content": piece}
        except Exception as exc:  # noqa: BLE001 - surface upstream LLM failures
            yield {"type": "error", "error": f"generation failed: {exc}"}
            return

        answer = "".join(parts)
        msg = await run_in_threadpool(
            self._store.append, conv["id"], "assistant", answer, citations
        )
        self._remember(tenant_id, user_id, message, answer)
        yield {
            "type": "done",
            "conversation_id": conv["id"],
            "message_id": msg["id"],
            "answer": answer,
            "citations": citations,
            "trace_id": retrieved.get("trace_id"),
            "created_at": msg["created_at"],
        }

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
