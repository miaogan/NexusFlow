from __future__ import annotations

import logging
import threading
from typing import Protocol

import httpx

logger = logging.getLogger(__name__)


class ConversationMemory(Protocol):
    def recall(
        self,
        tenant_id: str,
        user_id: str,
        query: str,
        top_k: int | None = None,
    ) -> list[str]: ...

    def extract_async(
        self,
        tenant_id: str,
        user_id: str,
        messages: list[dict[str, str]],
    ) -> None: ...


class HttpMemoryClient:
    """Calls the independent memory agent.

    ``recall`` returns ``[]`` on any failure; ``extract_async`` runs in a
    daemon thread so writing memories never adds latency to chat responses.
    """

    def __init__(
        self,
        base_url: str,
        timeout_seconds: float = 5.0,
        top_k: int = 3,
    ) -> None:
        self._base_url = base_url.rstrip("/")
        self._timeout = timeout_seconds
        self._top_k = top_k

    @property
    def enabled(self) -> bool:
        return bool(self._base_url)

    def recall(
        self,
        tenant_id: str,
        user_id: str,
        query: str,
        top_k: int | None = None,
    ) -> list[str]:
        if not self._base_url or not query.strip():
            return []
        headers = {"x-tenant-id": tenant_id} if tenant_id else None
        try:
            resp = httpx.post(
                f"{self._base_url}/v1/memories/search",
                json={
                    "tenant_id": tenant_id,
                    "user_id": user_id,
                    "query": query,
                    "top_k": top_k or self._top_k,
                },
                headers=headers,
                timeout=self._timeout,
            )
            resp.raise_for_status()
            data = resp.json().get("memories") or []
            return [
                str(item.get("content") or "").strip()
                for item in data
                if str(item.get("content") or "").strip()
            ]
        except Exception:  # noqa: BLE001 - memory is best-effort context
            return []

    def extract_async(
        self,
        tenant_id: str,
        user_id: str,
        messages: list[dict[str, str]],
    ) -> None:
        if not self._base_url or not messages:
            return
        thread = threading.Thread(
            target=self._extract,
            args=(tenant_id, user_id, list(messages)),
            daemon=True,
        )
        thread.start()

    def _extract(
        self,
        tenant_id: str,
        user_id: str,
        messages: list[dict[str, str]],
    ) -> None:
        headers = {"x-tenant-id": tenant_id} if tenant_id else None
        try:
            httpx.post(
                f"{self._base_url}/v1/memories/extract",
                json={
                    "tenant_id": tenant_id,
                    "user_id": user_id,
                    "messages": messages,
                },
                headers=headers,
                timeout=self._timeout,
            ).raise_for_status()
        except Exception as exc:  # noqa: BLE001 - background write, log only
            logger.warning("memory extraction failed: %s", exc)
