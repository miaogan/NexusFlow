from __future__ import annotations

from typing import Protocol

import httpx


class ConversationSummarizer(Protocol):
    def summarize(
        self,
        previous_summary: str,
        messages: list[dict[str, str]],
    ) -> str | None: ...


class HttpSummarizer:
    """Calls the independent summary agent, degrading to no compaction.

    Any failure (unset URL, network error, empty summary) returns ``None`` so
    the chat pipeline keeps the raw history window instead of breaking.
    """

    def __init__(
        self,
        base_url: str,
        timeout_seconds: float = 10.0,
        budget_chars: int = 1200,
    ) -> None:
        self._base_url = base_url.rstrip("/")
        self._timeout = timeout_seconds
        self._budget = budget_chars

    def summarize(
        self,
        previous_summary: str,
        messages: list[dict[str, str]],
    ) -> str | None:
        if not self._base_url or not messages:
            return None
        try:
            resp = httpx.post(
                f"{self._base_url}/v1/summarize",
                json={
                    "previous_summary": previous_summary,
                    "messages": messages,
                    "budget_chars": self._budget,
                },
                timeout=self._timeout,
            )
            resp.raise_for_status()
            data = resp.json()
            summary = str(data.get("summary") or "").strip()
            return summary or None
        except Exception:  # noqa: BLE001 - network/validation errors both degrade
            return None
