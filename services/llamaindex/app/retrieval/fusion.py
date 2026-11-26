from dataclasses import dataclass, field


@dataclass
class Chunk:
    chunk_id: str
    content: str
    score: float = 0.0
    meta: dict[str, str] = field(default_factory=dict)
    source: str = ""


def _key(chunk: Chunk) -> str:
    return chunk.chunk_id or str(hash(chunk.content))


def rrf_fuse(lists: list[list[Chunk]], k: int = 60) -> list[Chunk]:
    """Reciprocal Rank Fusion with de-duplication by chunk id.

    score = Σ 1 / (k + rank), rank starting at 1.
    """
    if k <= 0:
        k = 60

    scores: dict[str, float] = {}
    best: dict[str, Chunk] = {}

    for chunks in lists:
        seen: set[str] = set()
        for rank, chunk in enumerate(chunks, start=1):
            key = _key(chunk)
            if key in seen:
                continue
            seen.add(key)
            scores[key] = scores.get(key, 0.0) + 1.0 / (k + rank)
            current = best.get(key)
            if current is None:
                best[key] = chunk
                continue
            if not current.content and chunk.content:
                current.content = chunk.content
            if not current.meta and chunk.meta:
                current.meta = chunk.meta
            if not current.source:
                current.source = chunk.source

    fused = []
    for key, score in scores.items():
        chunk = best[key]
        chunk.score = score
        fused.append(chunk)
    fused.sort(key=lambda c: (-c.score, _key(c)))
    return fused
