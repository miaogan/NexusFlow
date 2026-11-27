from __future__ import annotations

import logging
import re
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
    "- skill：用户请求明确匹配某个可用技能，need_retrieval=false，并在 skill 中填写技能名。\n"
    "只能选择“可用技能”列表中存在的技能，不要编造技能名。\n"
    "判断依据：最后一条用户消息，结合最近对话上下文。"
)

_SKILL = re.compile(r"^[a-z0-9][a-z0-9._-]{1,63}$")


class IntentState(TypedDict, total=False):
    message: str
    history: list[dict[str, str]]
    skills: list[str]
    intent: str
    confidence: float
    need_retrieval: bool
    skill: str | None
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
        skills: list[str] = []
        for raw in state.get("skills") or []:
            skill = str(raw).strip()
            if skill and skill not in skills and _SKILL.match(skill):
                skills.append(skill)
        return {"message": message, "history": history, "skills": skills[:32]}

    def _classify(self, state: IntentState) -> dict[str, Any]:
        message = state.get("message") or ""
        skills = state.get("skills") or []
        if self._llm is not None and message:
            try:
                result = self._classify_with_llm(
                    message, state.get("history") or [], skills
                )
                return self._result_payload(result, source="llm", skills=skills)
            except Exception as exc:  # noqa: BLE001 - fall back to rules
                logger.warning("intent llm failed, using rules: %s", exc)

        return self._result_payload(
            classify_rules(message, skills), source="rules", skills=skills
        )

    def _classify_with_llm(
        self,
        message: str,
        history: list[dict[str, str]],
        skills: list[str],
    ) -> IntentResult:
        from langchain_core.messages import AIMessage, HumanMessage, SystemMessage

        system = SYSTEM_PROMPT
        if skills:
            system += "\n可用技能：\n" + "\n".join(f"- {skill}" for skill in skills)
        else:
            system += "\n当前没有可用技能，不要输出 intent=skill。"
        messages: list[Any] = [SystemMessage(content=system)]
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

    def _result_payload(
        self,
        result: IntentResult,
        *,
        source: str,
        skills: list[str],
    ) -> dict[str, Any]:
        intent = result.intent
        skill = (result.skill or "").strip() or None
        reason = result.reason
        if intent == "skill":
            if not skill or skill not in skills:
                intent = "clarify"
                skill = None
                reason = "请求的技能不可用或未匹配"
        else:
            skill = None
        # Keep need_retrieval consistent with the label even if the model slips.
        need_retrieval = intent == "knowledge_qa"
        payload = {
            "intent": intent,
            "confidence": float(result.confidence),
            "need_retrieval": need_retrieval,
            "skill": skill,
            "reason": reason,
            "source": source,
        }
        CLASSIFICATIONS.labels(intent=intent, source=source).inc()
        return payload

    def invoke(
        self,
        message: str,
        history: list[dict[str, str]],
        skills: list[str] | None = None,
    ) -> dict[str, Any]:
        state = self._compiled.invoke(
            {"message": message, "history": history, "skills": skills or []}
        )
        return {
            "intent": state.get("intent", "knowledge_qa"),
            "confidence": float(state.get("confidence", 0.0)),
            "need_retrieval": bool(state.get("need_retrieval", True)),
            "skill": state.get("skill"),
            "reason": state.get("reason", ""),
            "source": state.get("source", "rules"),
        }
