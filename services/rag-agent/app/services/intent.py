from __future__ import annotations

from typing import Any, Protocol

import httpx

VALID_INTENTS = {"knowledge_qa", "chitchat", "clarify"}


class IntentClassifier(Protocol):
    def classify(
        self, message: str, history: list[dict[str, str]]
    ) -> dict[str, Any]: ...


def fallback_intent(reason: str = "意图服务不可用，按知识库问答处理") -> dict[str, Any]:
    return {
        "intent": "knowledge_qa",
        "confidence": 0.0,
        "need_retrieval": True,
        "reason": reason,
        "source": "fallback",
    }


class HttpIntentClassifier:
    """Calls the independent intent agent over HTTP, degrading to knowledge QA."""

    def __init__(self, base_url: str, timeout_seconds: float = 5.0) -> None:
        self._base_url = base_url.rstrip("/")
        self._timeout = timeout_seconds

    def classify(
        self, message: str, history: list[dict[str, str]]
    ) -> dict[str, Any]:
        if not self._base_url:
            return fallback_intent("未配置意图服务，按知识库问答处理")
        try:
            resp = httpx.post(
                f"{self._base_url}/v1/intent",
                json={"message": message, "history": history},
                timeout=self._timeout,
            )
            resp.raise_for_status()
            data = resp.json()
            intent = str(data.get("intent") or "")
            if intent not in VALID_INTENTS:
                raise ValueError(f"unknown intent: {intent!r}")
            return {
                "intent": intent,
                "confidence": float(data.get("confidence") or 0.0),
                "need_retrieval": bool(data.get("need_retrieval", intent == "knowledge_qa")),
                "reason": str(data.get("reason") or ""),
                "source": str(data.get("source") or "llm"),
            }
        except Exception:  # noqa: BLE001 - network/validation errors both degrade
            return fallback_intent()
