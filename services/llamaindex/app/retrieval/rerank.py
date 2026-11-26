import json

import httpx

from app.core.config import settings
from app.retrieval.fusion import Chunk


def rerank(query: str, chunks: list[Chunk], model: str, top_n: int) -> list[Chunk]:
    """Call a Cross-Encoder rerank service. Raises on failure so the caller
    can degrade to RRF ordering (fail-open).

    Supports two wire protocols: ``generic`` (bge / Cohere style, the original
    payload) and ``dashscope`` (Aliyun text-rerank, authenticated bearer call).
    """
    if not settings.rerank_url:
        raise RuntimeError("rerank url is not configured")

    candidates = chunks
    if settings.rerank_pool > 0 and len(candidates) > settings.rerank_pool:
        candidates = candidates[: settings.rerank_pool]
    if not candidates:
        return []

    documents = [c.content for c in candidates]
    protocol = (settings.rerank_protocol or "generic").strip().lower()
    headers: dict[str, str] = {}
    if protocol == "dashscope":
        payload = {
            "model": model,
            "input": {"query": query, "documents": documents},
            "parameters": {"return_documents": False, "top_n": top_n},
        }
        if settings.rerank_api_key:
            headers["Authorization"] = f"Bearer {settings.rerank_api_key}"
    else:
        payload = {
            "model": model,
            "query": query,
            "documents": documents,
            "top_n": top_n,
        }

    resp = httpx.post(
        settings.rerank_url,
        json=payload,
        headers=headers or None,
        timeout=settings.rerank_timeout_seconds,
    )
    resp.raise_for_status()
    data = json.loads(resp.text)
    items = (
        (data.get("output") or {}).get("results", [])
        if protocol == "dashscope"
        else data.get("results", [])
    )

    reranked: list[Chunk] = []
    for item in items:
        idx = int(item.get("index", -1))
        if 0 <= idx < len(candidates):
            chunk = candidates[idx]
            chunk.score = float(item.get("relevance_score", 0.0))
            reranked.append(chunk)
    reranked.sort(key=lambda c: -c.score)
    return reranked[:top_n]
