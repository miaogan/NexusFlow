from __future__ import annotations

import logging
from typing import Any, TypedDict

from langgraph.graph import END, START, StateGraph
from prometheus_client import Counter

from nexusflow_agents.agents.summary.core.config import Settings
from nexusflow_agents.agents.summary.schemas import SummaryResult
from nexusflow_agents.agents.summary.summarize import extractive_summary

logger = logging.getLogger(__name__)

SUMMARIES = Counter(
    "summary_agent_summaries_total",
    "Conversation summaries by strategy",
    ["strategy"],
)

SYSTEM_PROMPT = (
    "你是 NexusFlow 的对话摘要器。将「此前摘要」与「新增早期对话」合并成一段简洁的中文摘要，"
    "保留关键事实、实体、结论与未解决的问题；不要编造，不要回答问题，只输出摘要正文。"
)


class SummaryState(TypedDict, total=False):
    previous_summary: str
    messages: list[dict[str, str]]
    budget_chars: int
    tenant_id: str
    summary: str
    strategy: str
    dropped_messages: int


class SummaryGraph:
    """Two-node LangGraph: normalize the batch, then summarize it.

    Without an LLM the ``summarize`` node falls back to a deterministic
    extractive digest so the service is usable offline.
    """

    def __init__(self, settings: Settings) -> None:
        self._settings = settings
        self._llm: Any | None = None
        self._tenant_llms: dict[str, Any] = {}
        if settings.llm_enabled:
            self._llm = self._build_llm("")

        builder = StateGraph(SummaryState)
        builder.add_node("normalize", self._normalize)
        builder.add_node("summarize", self._summarize)
        builder.add_edge(START, "normalize")
        builder.add_edge("normalize", "summarize")
        builder.add_edge("summarize", END)
        self._compiled = builder.compile()

    def _build_llm(self, tenant_id: str) -> Any:
        from langchain_openai import ChatOpenAI

        kwargs: dict[str, Any] = {}
        if tenant_id:
            kwargs["default_headers"] = {"x-tenant-id": tenant_id}
        return ChatOpenAI(
            model=self._settings.llm_model,
            base_url=self._settings.llm_base_url,
            api_key=self._settings.llm_api_key,
            temperature=0,
            timeout=self._settings.llm_timeout_seconds,
            **kwargs,
        )

    def _llm_for(self, tenant_id: str) -> Any:
        if not tenant_id or self._llm is None:
            return self._llm
        cached = self._tenant_llms.get(tenant_id)
        if cached is None:
            if len(self._tenant_llms) >= 32:
                self._tenant_llms.clear()
            cached = self._build_llm(tenant_id)
            self._tenant_llms[tenant_id] = cached
        return cached

    def _normalize(self, state: SummaryState) -> dict[str, Any]:
        previous = (state.get("previous_summary") or "").strip()[:8000]
        messages: list[dict[str, str]] = []
        for message in (state.get("messages") or [])[-self._settings.max_messages :]:
            role = str(message.get("role") or "")
            content = (message.get("content") or "").strip()
            if role not in {"user", "assistant"} or not content:
                continue
            messages.append(
                {"role": role, "content": content[: self._settings.max_message_chars]}
            )
        budget = int(state.get("budget_chars") or self._settings.budget_chars)
        return {
            "previous_summary": previous,
            "messages": messages,
            "budget_chars": max(100, budget),
        }

    def _summarize(self, state: SummaryState) -> dict[str, Any]:
        previous = state.get("previous_summary") or ""
        messages = state.get("messages") or []
        budget = state.get("budget_chars") or self._settings.budget_chars

        if self._llm is not None and messages:
            try:
                summary = self._summarize_with_llm(
                    previous, messages, budget, state.get("tenant_id") or ""
                )
                if summary:
                    return self._payload(summary[:budget], "llm", messages)
            except Exception as exc:  # noqa: BLE001 - fall back to extractive
                logger.warning("summary llm failed, using extractive: %s", exc)

        summary = extractive_summary(previous, messages, budget)
        return self._payload(summary, "extractive", messages)

    def _payload(
        self, summary: str, strategy: str, messages: list[dict[str, str]]
    ) -> dict[str, Any]:
        SUMMARIES.labels(strategy=strategy).inc()
        return {
            "summary": summary,
            "strategy": strategy,
            "dropped_messages": len(messages),
        }

    def _summarize_with_llm(
        self,
        previous: str,
        messages: list[dict[str, str]],
        budget: int,
        tenant_id: str = "",
    ) -> str:
        from langchain_core.messages import HumanMessage, SystemMessage

        lines: list[str] = []
        if previous:
            lines.append(f"此前摘要：{previous}")
        lines.append("新增早期对话：")
        for message in messages:
            label = "用户" if message["role"] == "user" else "助手"
            lines.append(f"{label}：{message['content']}")

        result = self._llm_for(tenant_id).invoke(  # type: ignore[union-attr]
            [
                SystemMessage(content=f"{SYSTEM_PROMPT}\n摘要不超过 {budget} 个汉字。"),
                HumanMessage(content="\n".join(lines)),
            ]
        )
        return str(getattr(result, "content", result)).strip()

    def invoke(
        self,
        previous_summary: str,
        messages: list[dict[str, str]],
        budget_chars: int | None = None,
        tenant_id: str = "",
    ) -> dict[str, Any]:
        state = self._compiled.invoke(
            {
                "previous_summary": previous_summary,
                "messages": messages,
                "budget_chars": budget_chars,
                "tenant_id": tenant_id,
            }
        )
        return SummaryResult(
            summary=state.get("summary", ""),
            strategy=state.get("strategy", "extractive"),
            dropped_messages=int(state.get("dropped_messages", 0)),
            budget_chars=int(state.get("budget_chars") or self._settings.budget_chars),
        ).model_dump()
