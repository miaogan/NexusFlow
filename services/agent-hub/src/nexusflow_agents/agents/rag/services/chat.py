from __future__ import annotations

import logging
from collections.abc import AsyncIterator
from datetime import UTC, datetime
from typing import Any
from uuid import uuid4

from starlette.concurrency import run_in_threadpool

from nexusflow_agents.agents.rag.core.config import Settings
from nexusflow_agents.agents.rag.graph import ChatGraph
from nexusflow_agents.agents.rag.schemas import (
    ChatRequest,
    ChatResponse,
    ConversationDetail,
    ConversationListResponse,
    MessageOut,
)
from nexusflow_agents.agents.rag.services.corrected import (
    CorrectedAnswerProvider,
    NullCorrectedAnswerProvider,
)
from nexusflow_agents.agents.rag.services.intent import HttpIntentClassifier, IntentClassifier
from nexusflow_agents.agents.rag.services.llm import current_usage
from nexusflow_agents.agents.rag.services.memory import ConversationMemory, HttpMemoryClient
from nexusflow_agents.agents.rag.services.reasoning import ApprovalPending, ReasoningLoop
from nexusflow_agents.agents.rag.services.retrieval import RetrievalUnavailable
from nexusflow_agents.agents.rag.services.skills import SkillRouter
from nexusflow_agents.agents.rag.services.store import (
    ConversationKBError,
    ConversationNotFound,
    ConversationStore,
)
from nexusflow_agents.agents.rag.services.summary import ConversationSummarizer, HttpSummarizer
from nexusflow_agents.core.audit import get_audit
from nexusflow_agents.core.pii import MaskSession, get_pii_shield

APPROVAL_PENDING_ANSWER = "该操作需要人工审批，已暂停执行。"

logger = logging.getLogger(__name__)


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
        reasoning: ReasoningLoop | None = None,
        skills: SkillRouter | None = None,
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
        # Optional multi-turn tool loop; disabled unless reasoning is enabled.
        self._reasoning = reasoning
        # Optional tenant-scoped skill router for custom SubAgents.
        self._skills = skills

    def _prepare_history(
        self, conv: dict[str, Any], tenant_id: str = ""
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
                new_summary = self._summarizer.summarize(summary, folded, tenant_id)
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

    @staticmethod
    def _request_history(req: ChatRequest) -> list[dict[str, str]]:
        """History supplied by stateless callers (OpenAI-compatible facade)."""
        items: list[dict[str, str]] = []
        for m in req.history or []:
            if m.role in ("user", "assistant") and m.content.strip():
                items.append({"role": m.role, "content": m.content})
        return items

    @staticmethod
    def _ephemeral_message(role: str, content: str) -> dict[str, str]:
        """Envelope for stateless turns that are never written to the store."""
        return {
            "id": str(uuid4()),
            "role": role,
            "content": content,
            "created_at": datetime.now(UTC).isoformat(),
        }

    async def _persist_reply(
        self,
        conv: dict[str, Any] | None,
        answer: str,
        citations: list[dict[str, Any]],
    ) -> dict[str, Any]:
        """Stores the assistant turn, or returns an ephemeral envelope."""
        if conv is None:
            return self._ephemeral_message("assistant", answer)
        return await run_in_threadpool(
            self._store.append, conv["id"], "assistant", answer, citations
        )

    @staticmethod
    def _conversation_id(conv: dict[str, Any] | None) -> str:
        return conv["id"] if conv is not None else ""

    @staticmethod
    def _usage_payload() -> dict[str, int] | None:
        """Real token usage reported by the LLM, or None when unavailable."""
        usage = current_usage()
        if usage is None or usage.total_tokens == 0:
            return None
        return {
            "prompt_tokens": usage.prompt_tokens,
            "completion_tokens": usage.completion_tokens,
            "total_tokens": usage.total_tokens,
        }

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

    def _pii_session(self) -> MaskSession:
        return get_pii_shield(self._settings).session()

    @staticmethod
    def _mask_history(
        session: MaskSession, history: list[dict[str, str]]
    ) -> list[dict[str, str]]:
        return [
            {"role": item.get("role", ""), "content": session.mask(item.get("content", ""))}
            for item in history
        ]

    @staticmethod
    def _mask_corrected(
        session: MaskSession, corrected: list[dict[str, Any]]
    ) -> list[dict[str, Any]]:
        return [
            {
                **item,
                "answer": session.mask(str(item.get("answer") or "")),
                "query": session.mask(str(item.get("query") or "")),
            }
            for item in corrected
        ]

    @staticmethod
    def _audit_pii(
        session: MaskSession,
        tenant_id: str,
        user_id: str,
        trace_id: str | None,
    ) -> None:
        if session.total() == 0:
            return
        try:
            get_audit().record(
                "pii.mask",
                tenant_id=tenant_id,
                user_id=user_id,
                trace_id=trace_id or "",
                resource="chat",
                risk="read",
                decision="masked",
                status="ok",
                detail={
                    "types": session.counts(),
                    "count": session.total(),
                    "restore_output": session.restore_output,
                },
            )
        except Exception as exc:  # noqa: BLE001 - auditing must not fail a turn
            logger.warning("pii audit failed: %s", exc)

    def _corrected_answers(        self, message: str, tenant_id: str, auth_header: str | None
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

    def _reasoning_state(
        self,
        *,
        message: str,
        history: list[dict[str, str]],
        summary: str,
        memories: list[str],
        corrected: list[dict[str, Any]],
        top_k: int,
        req: ChatRequest,
        tenant_id: str,
        user_id: str,
        auth_header: str | None,
        prompt_message: str | None = None,
        prompt_history: list[dict[str, str]] | None = None,
        prompt_summary: str | None = None,
        prompt_memories: list[str] | None = None,
        prompt_corrected: list[dict[str, Any]] | None = None,
        session: MaskSession | None = None,
    ) -> dict[str, Any] | None:
        """Runs retrieval + the reasoning loop; None means fall back.

        Retrieval always sees the original query so PII searches still work;
        the prompt receives the masked variants.
        """
        if self._reasoning is None or not self._reasoning.enabled:
            return None
        try:
            retrieved = self._graph.retrieve(
                query=message,
                tenant_id=tenant_id,
                top_k=top_k,
                kb_id=req.kb_id,
                filters=req.filters,
                auth_header=auth_header,
            )
        except Exception:  # noqa: BLE001 - retrieval failure falls back to graph
            return None
        chunks = retrieved.get("chunks") or []
        prompt_chunks = session.mask_chunks(chunks) if session is not None else chunks
        try:
            outcome = self._reasoning.run(
                query=prompt_message if prompt_message is not None else message,
                history=prompt_history if prompt_history is not None else history,
                chunks=prompt_chunks,
                summary=prompt_summary if prompt_summary is not None else summary,
                memories=prompt_memories if prompt_memories is not None else memories,
                corrected=prompt_corrected if prompt_corrected is not None else corrected,
                envelope={
                    "tenant_id": tenant_id,
                    "user_id": user_id,
                    "trace_id": retrieved.get("trace_id") or "",
                    "approval_id": req.approval_id or "",
                },
                auth_header=auth_header,
                extra_ctx={
                    "kb_id": req.kb_id,
                    "filters": req.filters,
                    "top_k": top_k,
                },
            )
        except ApprovalPending as exc:
            return {
                "answer": APPROVAL_PENDING_ANSWER
                + f"（approval_id={exc.approval_id}）",
                "citations": self._graph.citations(chunks),
                "trace_id": retrieved.get("trace_id"),
            }
        except Exception:  # noqa: BLE001 - tool loop must never break chat
            return None
        if not outcome.answer:
            return None
        return {
            "answer": outcome.answer,
            "citations": self._graph.citations(chunks),
            "trace_id": retrieved.get("trace_id"),
        }

    def _available_skills(self, tenant_id: str) -> list[str]:
        if self._skills is None:
            return []
        try:
            return self._skills.available(tenant_id)
        except Exception:  # noqa: BLE001 - routing must never break chat
            return []

    def _invoke_skill(
        self,
        skill: str,
        message: str,
        history: list[dict[str, str]],
        tenant_id: str,
        user_id: str,
        auth_header: str | None,
    ) -> tuple[str, str | None] | None:
        if not skill or self._skills is None:
            return None
        try:
            data = self._skills.invoke(
                skill,
                message,
                history,
                tenant_id=tenant_id,
                user_id=user_id,
                auth_header=auth_header,
            )
        except Exception as exc:  # noqa: BLE001 - degrade to normal RAG
            logger.warning("skill %s failed, falling back to RAG: %s", skill, exc)
            return None
        answer = self._skills.answer(data).strip()
        if not answer:
            return None
        trace_id = data.get("trace_id")
        return answer, trace_id if isinstance(trace_id, str) else None

    def chat(
        self,
        req: ChatRequest,
        tenant_id: str,
        user_id: str,
        auth_header: str | None,
    ) -> ChatResponse:
        message = req.message.strip()
        if req.stateless:
            conv = None
            history = self._request_history(req)
            summary = ""
        else:
            if req.conversation_id:
                conv = self._store.get(tenant_id, user_id, req.conversation_id)
                if conv is None:
                    raise ConversationNotFound("conversation not found")
                if (conv.get("kb_id") or "default") != (req.kb_id or "default"):
                    raise ConversationKBError(
                        "conversation belongs to a different knowledge base"
                    )
            else:
                conv = self._store.create(
                    tenant_id, user_id, message[:40], req.kb_id or "default"
                )
            history, summary = self._prepare_history(conv, tenant_id)

        memories = self._memory.recall(
            tenant_id, user_id, message, self._settings.memory_top_k
        )

        # Persist the user turn before generation so a failed generation still
        # leaves the question visible for a retry. Stateless turns own no record.
        if conv is not None:
            self._store.append(conv["id"], "user", message, [])

        top_k = req.top_k or self._settings.default_top_k
        top_k = max(1, min(top_k, self._settings.max_top_k))

        session = self._pii_session()
        masked_message = session.mask(message)
        masked_history = self._mask_history(session, history)
        masked_summary = session.mask(summary)
        masked_memories = [session.mask(item) for item in memories]

        skills = self._available_skills(tenant_id)
        intent = self._intent.classify(
            masked_message, masked_history, tenant_id=tenant_id, skills=skills
        )
        citations: list[dict[str, Any]] = []
        answer = ""
        trace_id = None
        if intent.get("intent") == "skill":
            skill_result = self._invoke_skill(
                str(intent.get("skill") or ""),
                masked_message,
                masked_history,
                tenant_id,
                user_id,
                auth_header,
            )
            if skill_result is not None:
                answer, trace_id = skill_result
            else:
                # A stale/unavailable skill must not break the turn.
                intent = {
                    **intent,
                    "intent": "knowledge_qa",
                    "need_retrieval": True,
                    "skill": None,
                }

        if answer:
            pass
        elif intent.get("need_retrieval", True):
            corrected = self._corrected_answers(message, tenant_id, auth_header)
            masked_corrected = self._mask_corrected(session, corrected)
            state = self._reasoning_state(
                message=message,
                history=history,
                summary=summary,
                memories=memories,
                corrected=corrected,
                top_k=top_k,
                req=req,
                tenant_id=tenant_id,
                user_id=user_id,
                auth_header=auth_header,
                prompt_message=masked_message,
                prompt_history=masked_history,
                prompt_summary=masked_summary,
                prompt_memories=masked_memories,
                prompt_corrected=masked_corrected,
                session=session,
            )
            if state is not None:
                answer = state.get("answer") or ""
                citations = state.get("citations") or []
                trace_id = state.get("trace_id")
            elif session.enabled:
                # PII mode retrieves with the original query so searches for a
                # phone/email still hit, then generates from masked context.
                retrieved = self._graph.retrieve(
                    query=message,
                    tenant_id=tenant_id,
                    top_k=top_k,
                    kb_id=req.kb_id,
                    filters=req.filters,
                    auth_header=auth_header,
                )
                chunks = retrieved.get("chunks") or []
                answer = self._graph.generate(
                    masked_message,
                    masked_history,
                    session.mask_chunks(chunks),
                    summary=masked_summary,
                    memories=masked_memories,
                    corrected=masked_corrected,
                    tenant_id=tenant_id,
                )
                citations = self._graph.citations(chunks)
                trace_id = retrieved.get("trace_id")
            else:
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
                citations = state.get("citations") or []
                trace_id = state.get("trace_id")
        else:
            answer = self._graph.generate_intent_answer(
                str(intent.get("intent") or "clarify"),
                masked_message,
                masked_history,
                summary=masked_summary,
                memories=masked_memories,
                tenant_id=tenant_id,
            )
            citations = []
            trace_id = None

        answer = session.output(answer)
        self._audit_pii(session, tenant_id, user_id, trace_id)
        if conv is not None:
            msg = self._store.append(conv["id"], "assistant", answer, citations)
        else:
            msg = self._ephemeral_message("assistant", answer)
        self._remember(tenant_id, user_id, masked_message, answer)

        return ChatResponse(
            conversation_id=conv["id"] if conv is not None else "",
            message_id=msg["id"],
            answer=answer,
            citations=citations,
            trace_id=trace_id,
            created_at=msg["created_at"],
            usage=self._usage_payload(),
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
        if req.stateless:
            conv = None
            history = self._request_history(req)
            summary = ""
        else:
            if req.conversation_id:
                conv = await run_in_threadpool(
                    self._store.get, tenant_id, user_id, req.conversation_id
                )
                if conv is None:
                    yield {"type": "error", "error": "conversation not found"}
                    return
                if (conv.get("kb_id") or "default") != (req.kb_id or "default"):
                    yield {
                        "type": "error",
                        "error": "conversation belongs to a different knowledge base",
                    }
                    return
            else:
                conv = await run_in_threadpool(
                    self._store.create,
                    tenant_id,
                    user_id,
                    message[:40],
                    req.kb_id or "default",
                )
            history, summary = await run_in_threadpool(
                self._prepare_history, conv, tenant_id
            )

        memories = await run_in_threadpool(
            self._memory.recall,
            tenant_id,
            user_id,
            message,
            self._settings.memory_top_k,
        )
        if conv is not None:
            await run_in_threadpool(
                self._store.append, conv["id"], "user", message, []
            )

        session = self._pii_session()
        masked_message = session.mask(message)
        masked_history = self._mask_history(session, history)
        masked_summary = session.mask(summary)
        masked_memories = [session.mask(item) for item in memories]

        top_k = req.top_k or self._settings.default_top_k
        top_k = max(1, min(top_k, self._settings.max_top_k))

        yield {"type": "stage", "stage": "intent", "status": "start"}
        skills = await run_in_threadpool(self._available_skills, tenant_id)
        intent = await run_in_threadpool(
            self._intent.classify,
            masked_message,
            masked_history,
            tenant_id=tenant_id,
            skills=skills,
        )
        yield {"type": "intent", **intent}
        yield {"type": "stage", "stage": "intent", "status": "done"}

        if intent.get("intent") == "skill":
            skill = str(intent.get("skill") or "")
            yield {"type": "stage", "stage": "skill", "status": "start", "skill": skill}
            skill_result = await run_in_threadpool(
                self._invoke_skill,
                skill,
                masked_message,
                masked_history,
                tenant_id,
                user_id,
                auth_header,
            )
            if skill_result is not None:
                answer, trace_id = skill_result
                answer = session.output(answer)
                self._audit_pii(session, tenant_id, user_id, trace_id)
                yield {"type": "token", "content": answer}
                msg = await self._persist_reply(conv, answer, [])
                self._remember(tenant_id, user_id, masked_message, answer)
                yield {"type": "stage", "stage": "skill", "status": "done", "skill": skill}
                yield {
                    "type": "done",
                    "conversation_id": self._conversation_id(conv),
                    "message_id": msg["id"],
                    "answer": answer,
                    "citations": [],
                    "trace_id": trace_id,
                    "created_at": msg["created_at"],
                    "usage": self._usage_payload(),
                }
                return
            # Stale/unavailable skill: continue as knowledge QA.
            yield {"type": "stage", "stage": "skill", "status": "degraded", "skill": skill}
            intent = {
                **intent,
                "intent": "knowledge_qa",
                "need_retrieval": True,
                "skill": None,
            }

        # chitchat/clarify turns answer directly; only knowledge QA retrieves.
        if not intent.get("need_retrieval", True):
            yield {"type": "stage", "stage": "generate", "status": "start"}
            intent_parts: list[str] = []
            try:
                async for piece in self._graph.stream_intent_answer(
                    str(intent.get("intent") or "clarify"),
                    masked_message,
                    masked_history,
                    summary=masked_summary,
                    memories=masked_memories,
                    tenant_id=tenant_id,
                ):
                    intent_parts.append(piece)
                    yield {"type": "token", "content": piece}
            except Exception as exc:  # noqa: BLE001 - surface upstream LLM failures
                yield {"type": "error", "error": f"generation failed: {exc}"}
                return

            answer = session.output("".join(intent_parts))
            self._audit_pii(session, tenant_id, user_id, None)
            msg = await self._persist_reply(conv, answer, [])
            self._remember(tenant_id, user_id, masked_message, answer)
            yield {
                "type": "done",
                "conversation_id": self._conversation_id(conv),
                "message_id": msg["id"],
                "answer": answer,
                "citations": [],
                "trace_id": None,
                "created_at": msg["created_at"],
                "usage": self._usage_payload(),
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
        masked_chunks = session.mask_chunks(chunks)
        masked_corrected = self._mask_corrected(session, corrected)
        yield {"type": "sources", "citations": citations}
        yield {
            "type": "stage",
            "stage": "retrieve",
            "status": "done",
            "count": len(citations),
        }
        yield {"type": "stage", "stage": "generate", "status": "start"}

        answer = ""
        streamed = False
        if self._reasoning is not None and self._reasoning.enabled:
            try:
                async for event in self._reasoning.astream(
                    query=masked_message,
                    history=masked_history,
                    chunks=masked_chunks,
                    summary=masked_summary,
                    memories=masked_memories,
                    corrected=masked_corrected,
                    envelope={
                        "tenant_id": tenant_id,
                        "user_id": user_id,
                        "trace_id": retrieved.get("trace_id") or "",
                        "approval_id": req.approval_id or "",
                    },
                    auth_header=auth_header,
                    extra_ctx={
                        "kb_id": req.kb_id,
                        "filters": req.filters,
                        "top_k": top_k,
                    },
                ):
                    if event["type"] == "final":
                        answer = str(event.get("answer") or "")
                        if event.get("stopped") == "approval_required" and not answer:
                            answer = APPROVAL_PENDING_ANSWER
                            if event.get("approval_id"):
                                answer += f"（approval_id={event['approval_id']}）"
                            yield {"type": "token", "content": answer}
                        continue
                    yield event
                streamed = True
            except Exception:  # noqa: BLE001 - tool loop falls back to generate
                answer = ""

        if not streamed or not answer:
            parts: list[str] = []
            try:
                async for piece in self._graph.stream_answer(
                    masked_message,
                    masked_history,
                    masked_chunks,
                    summary=masked_summary,
                    memories=masked_memories,
                    corrected=masked_corrected,
                    tenant_id=tenant_id,
                ):
                    parts.append(piece)
                    yield {"type": "token", "content": piece}
            except Exception as exc:  # noqa: BLE001 - surface upstream LLM failures
                yield {"type": "error", "error": f"generation failed: {exc}"}
                return
            answer = "".join(parts)
        answer = session.output(answer)
        self._audit_pii(session, tenant_id, user_id, retrieved.get("trace_id"))
        msg = await self._persist_reply(conv, answer, citations)
        self._remember(tenant_id, user_id, masked_message, answer)
        yield {
            "type": "done",
            "conversation_id": self._conversation_id(conv),
            "message_id": msg["id"],
            "answer": answer,
            "citations": citations,
            "trace_id": retrieved.get("trace_id"),
            "created_at": msg["created_at"],
            "usage": self._usage_payload(),
        }

    def list_conversations(
        self,
        tenant_id: str,
        user_id: str,
        limit: int,
        offset: int,
        kb_id: str | None = None,
    ) -> ConversationListResponse:
        items, total = self._store.list(tenant_id, user_id, limit, offset, kb_id)
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
            "kb_id": conv.get("kb_id") or "default",
            "title": conv["title"],
            "message_count": conv.get("message_count") or len(messages),
            "created_at": conv["created_at"],
            "updated_at": conv["updated_at"],
        }
        return ConversationDetail(conversation=summary, messages=messages)
