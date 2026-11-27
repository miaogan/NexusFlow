from __future__ import annotations

import httpx
from llama_index.core.embeddings import BaseEmbedding
from pydantic import Field

from app.core.config import settings


class OpenAICompatibleEmbedding(BaseEmbedding):
    """Thin /embeddings client for OpenAI-compatible providers.

    LlamaIndex's built-in ``OpenAIEmbedding`` validates the model name against
    OpenAI's enum, which rejects provider-specific models such as DashScope's
    ``qwen3.7-text-embedding-flash``. This client keeps the same wire format
    (``{model, input}`` -> ``{data: [{index, embedding}]}``) without that
    validation, and honours the configured batch size.
    """

    api_base: str = Field(default="")
    api_key: str = Field(default="")
    timeout: float = Field(default=30.0)

    def _embed(self, texts: list[str]) -> list[list[float]]:
        if not self.api_key:
            raise RuntimeError("embedding api key is not configured")
        if not texts:
            return []
        resp = httpx.post(
            f"{self.api_base.rstrip('/')}/embeddings",
            headers={"Authorization": f"Bearer {self.api_key}"},
            json={"model": self.model_name, "input": texts},
            timeout=self.timeout,
        )
        resp.raise_for_status()
        items = (resp.json() or {}).get("data") or []
        items = sorted(items, key=lambda item: int(item.get("index", 0)))
        return [[float(value) for value in item.get("embedding") or []] for item in items]

    def _get_query_embedding(self, query: str) -> list[float]:
        return self._embed([query])[0]

    async def _aget_query_embedding(self, query: str) -> list[float]:
        return self._get_query_embedding(query)

    def _get_text_embedding(self, text: str) -> list[float]:
        return self._embed([text])[0]

    async def _aget_text_embedding(self, text: str) -> list[float]:
        return self._get_text_embedding(text)

    def _get_text_embeddings(self, texts: list[str]) -> list[list[float]]:
        batch = max(1, self.embed_batch_size)
        out: list[list[float]] = []
        for start in range(0, len(texts), batch):
            out.extend(self._embed(texts[start : start + batch]))
        return out

    async def _aget_text_embeddings(self, texts: list[str]) -> list[list[float]]:
        return self._get_text_embeddings(texts)


def build_embedding(model: str | None = None):
    return OpenAICompatibleEmbedding(
        model_name=model or settings.embedding_model,
        embed_batch_size=max(1, settings.embedding_batch_size),
        api_base=settings.embedding_api_base,
        api_key=settings.embedding_api_key,
    )
