from __future__ import annotations

from typing import Any

from nexusflow_agents.core.registry import (
    CAPABILITY_INTENT,
    CAPABILITY_MEMORY_EXTRACT,
    CAPABILITY_MEMORY_RECALL,
    CAPABILITY_SUMMARY,
)


def intent_classify(payload: dict[str, Any], ctx: dict[str, Any]) -> dict[str, Any]:
    from nexusflow_agents.agents.intent.deps import get_graph

    return get_graph().invoke(
        str(payload.get("message") or ""),
        list(payload.get("history") or []),
    )


def summary_compact(payload: dict[str, Any], ctx: dict[str, Any]) -> dict[str, Any]:
    from nexusflow_agents.agents.summary.deps import get_graph

    return get_graph().invoke(
        previous_summary=str(payload.get("previous_summary") or ""),
        messages=list(payload.get("messages") or []),
        budget_chars=payload.get("budget_chars"),
    )


def memory_recall(payload: dict[str, Any], ctx: dict[str, Any]) -> dict[str, Any]:
    from nexusflow_agents.agents.memory.deps import get_recall_graph

    return get_recall_graph().invoke(
        str(payload.get("tenant_id") or ctx.get("tenant_id") or "default"),
        str(payload.get("user_id") or ctx.get("user_id") or "anonymous"),
        str(payload.get("query") or ""),
        payload.get("top_k"),
    )


def memory_extract(payload: dict[str, Any], ctx: dict[str, Any]) -> dict[str, Any]:
    from nexusflow_agents.agents.memory.deps import get_extract_graph

    return get_extract_graph().invoke(
        str(payload.get("tenant_id") or ctx.get("tenant_id") or "default"),
        str(payload.get("user_id") or ctx.get("user_id") or "anonymous"),
        list(payload.get("messages") or []),
    )


LOCAL_HANDLERS = {
    CAPABILITY_INTENT: intent_classify,
    CAPABILITY_SUMMARY: summary_compact,
    CAPABILITY_MEMORY_RECALL: memory_recall,
    CAPABILITY_MEMORY_EXTRACT: memory_extract,
}
