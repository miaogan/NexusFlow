from __future__ import annotations

import logging
from typing import Any, TypedDict

from langgraph.graph import END, START, StateGraph
from prometheus_client import Counter

from nexusflow_agents.agents.intent.core.config import Settings
from nexusflow_agents.agents.intent.rules import classify_rules
from nexusflow_agents.agents.intent.schemas import IntentResult

logger = logging.getLogger(__name__)

CLASSIFICATIONS = Counter(
    "intent_agent_classifications_total",
    "Intent classifications by label and source",
    ["intent", "source"],
)

SYSTEM_PROMPT = (
    "你是 NexusFlow 的意图分类器。只输出结构化结果，不要回答问题。\n"
    "可选意图：\n"
    "- knowledge_qa：需要查询知识库/文档才能回答的问题，need_retrieval=true。\n"
    "- chitchat：问候、感谢、寒暄，need_retrieval=false。\n"
    "- clarify：意图不明确、指代不清，或请求当前系统不支持的操作，need_retrieval=false。\n"
    "判断依据：最后一条用户消息，结合最近对话上下文。"
)


class IntentState(TypedDict, total=False):
    message: str
    history: list[dict[str, str]]
    intent: str
    confidence: float
    need_retrieval: bool
    reason: str
    source: str


class IntentGraph:
    """Two-node LangGraph: normalize the input, then classify intent."""

    def __init__(self, settings: Settings) -> None:
        self._settings = settings
        self._llm: Any | None = None
        if settings.llm_enabled:
            from langchain_openai import ChatOpenAI

            llm = ChatOpenAI(
                model=settings.llm_model,
                base_url=settings.llm_base_url,
                api_key=settings.llm_api_key,
                temperature=0,
                timeout=settings.llm_timeout_seconds,
            )
            self._llm = llm.with_structured_output(IntentResult)

        builder = StateGraph(IntentState)
        builder.add_node("normalize", self._normalize)
        builder.add_node("classify", self._classify)
        builder.add_edge(START, "normalize")
        builder.add_edge("normalize", "classify")
        builder.add_edge("classify", END)
        self._compiled = builder.compile()

    def _normalize(self, state: IntentState) -> dict[str, Any]:
        message = (state.get("message") or "").strip()[: self._settings.max_message_chars]
        history = [
            {"role": m.get("role", "user"), "content": (m.get("content") or "")[:2000]}
            for m in (state.get("history") or [])
            if m.get("role") in {"user", "assistant"}
        ][-self._settings.max_history_messages :]
        return {"message": message, "history": history}

    def _classify(self, state: IntentState) -> dict[str, Any]:
        message = state.get("message") or ""
        if self._llm is not None and message:
            try:
                result = self._classify_with_llm(message, state.get("history") or [])
                return self._result_payload(result, source="llm")
            except Exception as exc:  # noqa: BLE001 - fall back to rules
                logger.warning("intent llm failed, using rules: %s", exc)

        return self._result_payload(classify_rules(message), source="rules")

    def _classify_with_llm(self, message: str, history: list[dict[str, str]]) -> IntentResult:
        from langchain_core.messages import AIMessage, HumanMessage, SystemMessage

        messages: list[Any] = [SystemMessage(content=SYSTEM_PROMPT)]
        for item in history:
            if item.get("role") == "user":
                messages.append(HumanMessage(content=item.get("content") or ""))
            else:
                messages.append(AIMessage(content=item.get("content") or ""))
        messages.append(HumanMessage(content=f"最后一条用户消息：{message}"))

        result = self._llm.invoke(messages)  # type: ignore[union-attr]
        if isinstance(result, dict):
            result = IntentResult(**result)
        if not isinstance(result, IntentResult):
            raise TypeError(f"unexpected structured output: {type(result)!r}")
        return result

    def _result_payload(self, result: IntentResult, *, source: str) -> dict[str, Any]:
        # Keep need_retrieval consistent with the label even if the model slips.
        need_retrieval = result.intent == "knowledge_qa"
        payload = {
            "intent": result.intent,
            "confidence": float(result.confidence),
            "need_retrieval": need_retrieval,
            "reason": result.reason,
            "source": source,
        }
        CLASSIFICATIONS.labels(intent=result.intent, source=source).inc()
        return payload

    def invoke(self, message: str, history: list[dict[str, str]]) -> dict[str, Any]:
        state = self._compiled.invoke({"message": message, "history": history})
        return {
            "intent": state.get("intent", "knowledge_qa"),
            "confidence": float(state.get("confidence", 0.0)),
            "need_retrieval": bool(state.get("need_retrieval", True)),
            "reason": state.get("reason", ""),
            "source": state.get("source", "rules"),
        }
