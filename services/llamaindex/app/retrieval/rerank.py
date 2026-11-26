import json

import httpx

from app.core.config import settings
from app.retrieval.fusion import Chunk


def rerank(query: str, chunks: list[Chunk], model: str, top_n: int) -> list[Chunk]:
    """Call a Cross-Encoder rerank service. Raises on failure so the caller
    can degrade to RRF ordering (fail-open)."""
    if not settings.rerank_url:
        raise RuntimeError("rerank url is not configured")

    payload = {
        "model": model,
        "query": query,
        "documents": [c.content for c in chunks],
        "top_n": top_n,
    }
    resp = httpx.post(
        settings.rerank_url,
        json=payload,
        timeout=settings.rerank_timeout_seconds,
    )
    resp.raise_for_status()
    data = json.loads(resp.text)

    reranked: list[Chunk] = []
    for item in data.get("results", []):
        idx = int(item.get("index", -1))
        if 0 <= idx < len(chunks):
            chunk = chunks[idx]
            chunk.score = float(item.get("relevance_score", 0.0))
            reranked.append(chunk)
    reranked.sort(key=lambda c: -c.score)
    return reranked[:top_n]
