from __future__ import annotations

import logging

import httpx

from nexusflow_agents.agents.memory.core.config import Settings

logger = logging.getLogger(__name__)


class HttpEmbedder:
    """Optional OpenAI-compatible embeddings client.

    Returns ``None`` on any failure so callers fall back to lexical scoring.
    """

    def __init__(self, settings: Settings) -> None:
        self._base_url = settings.embedding_base_url.rstrip("/")
        self._api_key = settings.embedding_api_key
        self._model = settings.embedding_model
        self._timeout = settings.embedding_timeout_seconds
        self._enabled = settings.embedding_enabled

    @property
    def enabled(self) -> bool:
        return self._enabled

    def embed(self, texts: list[str]) -> list[list[float]] | None:
        if not self._enabled or not texts:
            return None
        try:
            resp = httpx.post(
                f"{self._base_url}/embeddings",
                headers={"Authorization": f"Bearer {self._api_key}"},
                json={"model": self._model, "input": texts},
                timeout=self._timeout,
            )
            resp.raise_for_status()
            data = resp.json().get("data") or []
            vectors = [item["embedding"] for item in data]
            if len(vectors) != len(texts):
                raise ValueError("embedding count mismatch")
            return vectors
        except Exception as exc:  # noqa: BLE001 - embeddings are best-effort
            logger.warning("embedding request failed: %s", exc)
            return None
