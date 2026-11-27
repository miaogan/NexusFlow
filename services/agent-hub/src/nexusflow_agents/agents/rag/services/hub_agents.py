from __future__ import annotations

from typing import Any

from nexusflow_agents.agents.rag.services.intent import (
    VALID_INTENTS,
    fallback_intent,
)
from nexusflow_agents.core.hub import Hub
from nexusflow_agents.core.registry import (
    CAPABILITY_INTENT,
    CAPABILITY_MEMORY_EXTRACT,
    CAPABILITY_MEMORY_RECALL,
    CAPABILITY_SUMMARY,
)
from nexusflow_agents.core.runtime import get_hub


class HubIntentClassifier:
    """Routes intent classification through the star-topology hub."""

    def __init__(self, hub: Hub | None = None) -> None:
        self._hub = hub or get_hub()

    def classify(
        self,
        message: str,
        history: list[dict[str, str]],
        tenant_id: str = "",
        skills: list[str] | None = None,
    ) -> dict[str, Any]:
        offered = list(skills or [])
        result = self._hub.call(
            CAPABILITY_INTENT,
            {
                "message": message,
                "history": history,
                "tenant_id": tenant_id,
                "skills": offered,
            },
            {"tenant_id": tenant_id},
        )
        data = result.data if result.ok else {}
        intent = str(data.get("intent") or "")
        if intent not in VALID_INTENTS:
            return fallback_intent(result.error or "意图 Agent 不可用")
        skill = str(data.get("skill") or "").strip() or None
        if intent == "skill" and skill not in offered:
            return fallback_intent("意图 Agent 返回了不可用技能")
        return {
            "intent": intent,
            "confidence": float(data.get("confidence") or 0.0),
            "need_retrieval": bool(
                data.get("need_retrieval", intent == "knowledge_qa")
            ),
            "skill": skill,
            "reason": str(data.get("reason") or ""),
            "source": str(data.get("source") or "llm"),
        }


class HubSummarizer:
    """Routes context compaction through the hub, degrading to no summary."""

    def __init__(self, hub: Hub | None = None, budget_chars: int = 1200) -> None:
        self._hub = hub or get_hub()
        self._budget = budget_chars

    def summarize(
        self,
        previous_summary: str,
        messages: list[dict[str, str]],
    ) -> str | None:
        if not messages:
            return None
        result = self._hub.call(
            CAPABILITY_SUMMARY,
            {
                "previous_summary": previous_summary,
                "messages": messages,
                "budget_chars": self._budget,
            },
        )
        if not result.ok:
            return None
        summary = str((result.data or {}).get("summary") or "").strip()
        return summary or None


class HubMemory:
    """Routes memory recall/extract through the hub."""

    def __init__(self, hub: Hub | None = None, top_k: int = 3) -> None:
        self._hub = hub or get_hub()
        self._top_k = top_k

    def recall(
        self,
        tenant_id: str,
        user_id: str,
        query: str,
        top_k: int | None = None,
    ) -> list[str]:
        if not query.strip():
            return []
        result = self._hub.call(
            CAPABILITY_MEMORY_RECALL,
            {
                "tenant_id": tenant_id,
                "user_id": user_id,
                "query": query,
                "top_k": top_k or self._top_k,
            },
            {"tenant_id": tenant_id, "user_id": user_id},
        )
        if not result.ok:
            return []
        memories = (result.data or {}).get("memories") or []
        return [
            str(item.get("content") or "").strip()
            for item in memories
            if str(item.get("content") or "").strip()
        ]

    def extract_async(
        self,
        tenant_id: str,
        user_id: str,
        messages: list[dict[str, str]],
    ) -> None:
        if not messages:
            return
        self._hub.submit_background(
            CAPABILITY_MEMORY_EXTRACT,
            {
                "tenant_id": tenant_id,
                "user_id": user_id,
                "messages": messages,
            },
            {"tenant_id": tenant_id, "user_id": user_id},
        )
