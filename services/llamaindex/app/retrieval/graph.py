"""LightRAG-style graph recall.

Entity/relation extraction prefers the configured LLM (``LLAMAINDEX_LLM_*``).
Without an LLM key — or whenever the call fails — it degrades to a co-occurrence
word graph built from the same chunks, so the ``graph`` recall route keeps
contributing candidates to RRF instead of failing.
"""
from __future__ import annotations

import json
import logging
import math
import re
import threading
import time
from collections.abc import Callable
from dataclasses import dataclass, field

import httpx

from app.core.config import settings
from app.retrieval.fusion import Chunk

logger = logging.getLogger(__name__)

_WORD = re.compile(r"[a-z][a-z0-9_+\-]{1,}")
_CJK = re.compile(r"[\u4e00-\u9fff]+")

_EN_STOP = {
    "the", "and", "for", "with", "that", "this", "from", "are", "was", "were",
    "have", "has", "had", "not", "but", "you", "your", "our", "its", "their",
    "will", "can", "may", "into", "than", "then", "when", "where", "which",
    "how", "what", "why", "who", "all", "any", "use", "used", "using", "via",
}
_CN_STOP = {
    "的是", "一个", "这个", "那个", "我们", "你们", "他们", "以及", "或者",
    "如果", "因为", "所以", "可以", "进行", "通过", "没有", "不是", "就是",
}

_LLM_PROMPT = (
    "你是知识图谱抽取器。从给定文本中抽取关键实体（术语、系统、组件、概念）"
    "以及它们之间的关系。只输出 JSON："
    '{"entities": ["实体A", "实体B"], "relations": [["实体A", "实体B"]]}，'
    "不要输出任何解释。"
)


@dataclass
class GraphNode:
    term: str
    chunk_ids: set[str] = field(default_factory=set)
    weight: float = 0.0


@dataclass
class DocumentGraph:
    chunks: dict[str, Chunk] = field(default_factory=dict)
    nodes: dict[str, GraphNode] = field(default_factory=dict)
    edges: dict[tuple[str, str], float] = field(default_factory=dict)
    adjacency: dict[str, dict[str, float]] = field(default_factory=dict)
    mode: str = "cooccurrence"

    def add_edge(self, source: str, target: str, weight: float = 1.0) -> None:
        if source == target:
            return
        key = (source, target) if source < target else (target, source)
        self.edges[key] = self.edges.get(key, 0.0) + weight
        row = self.adjacency.setdefault(source, {})
        row[target] = row.get(target, 0.0) + weight
        row = self.adjacency.setdefault(target, {})
        row[source] = row.get(source, 0.0) + weight

    def entities(self, limit: int = 50) -> list[dict[str, object]]:
        nodes = sorted(
            self.nodes.values(), key=lambda n: (-len(n.chunk_ids), n.term)
        )
        return [
            {
                "term": n.term,
                "chunk_count": len(n.chunk_ids),
                "weight": round(n.weight, 2),
            }
            for n in nodes[:limit]
        ]

    def relations(self, limit: int = 100) -> list[dict[str, object]]:
        edges = sorted(self.edges.items(), key=lambda kv: (-kv[1], kv[0]))
        return [
            {"source": a, "target": b, "weight": round(w, 2)}
            for (a, b), w in edges[:limit]
        ]


def extract_terms(text: str) -> list[str]:
    """Tokenises text into graph terms: ASCII words plus CJK bigrams.

    Bigrams keep the graph useful for Chinese without an external segmenter.
    """
    lowered = (text or "").lower()
    terms: list[str] = []
    for word in _WORD.findall(lowered):
        if word not in _EN_STOP:
            terms.append(word)
    for run in _CJK.findall(lowered):
        if len(run) < 2:
            continue
        terms.extend(run[i : i + 2] for i in range(len(run) - 1))
    seen: set[str] = set()
    out: list[str] = []
    for term in terms:
        if term in seen or term in _CN_STOP:
            continue
        seen.add(term)
        out.append(term)
    return out


def _chunk_key(doc: Chunk) -> str:
    return doc.chunk_id or str(abs(hash(doc.content)))


def build_cooccurrence_graph(
    docs: list[Chunk], max_terms_per_chunk: int = 32
) -> DocumentGraph:
    """Fallback graph: terms are nodes, terms sharing a chunk get an edge."""
    graph = DocumentGraph(mode="cooccurrence")
    for doc in docs:
        key = _chunk_key(doc)
        graph.chunks[key] = Chunk(
            chunk_id=key,
            content=doc.content,
            score=doc.score,
            meta=dict(doc.meta),
            source="graph",
        )
        terms = extract_terms(doc.content)[:max_terms_per_chunk]
        for term in terms:
            node = graph.nodes.setdefault(term, GraphNode(term=term))
            node.chunk_ids.add(key)
            node.weight += 1.0
        for idx, left in enumerate(terms):
            for right in terms[idx + 1 :]:
                graph.add_edge(left, right)
    return graph


def _parse_json_object(raw: str) -> dict | None:
    text = raw.strip()
    if text.startswith("```"):
        text = "\n".join(
            line for line in text.splitlines() if not line.strip().startswith("```")
        )
    start, end = text.find("{"), text.rfind("}")
    if start < 0 or end <= start:
        return None
    try:
        parsed = json.loads(text[start : end + 1])
    except json.JSONDecodeError:
        return None
    return parsed if isinstance(parsed, dict) else None


def build_llm_graph(docs: list[Chunk]) -> DocumentGraph | None:
    """Extracts entities/relations with an OpenAI-compatible LLM.

    Returns ``None`` when no LLM is configured or the call/output is unusable,
    which makes the caller fall back to the co-occurrence graph.
    """
    if not (settings.llm_base_url and settings.llm_api_key) or not docs:
        return None

    batch = docs[: settings.graph_max_llm_chunks]
    text = "\n---\n".join(doc.content[:1200] for doc in batch)
    try:
        resp = httpx.post(
            f"{settings.llm_base_url.rstrip('/')}/chat/completions",
            headers={"Authorization": f"Bearer {settings.llm_api_key}"},
            json={
                "model": settings.llm_model,
                "temperature": 0,
                "messages": [
                    {"role": "system", "content": _LLM_PROMPT},
                    {"role": "user", "content": text},
                ],
            },
            timeout=settings.graph_llm_timeout_seconds,
        )
        resp.raise_for_status()
        content = resp.json()["choices"][0]["message"]["content"]
    except Exception as exc:  # noqa: BLE001 - any failure degrades the graph
        logger.warning(
            "LLM graph extraction failed, falling back to co-occurrence: %s", exc
        )
        return None

    data = _parse_json_object(str(content))
    if data is None:
        return None

    relations: list[tuple[str, str]] = []
    for item in data.get("relations") or []:
        if isinstance(item, (list, tuple)) and len(item) == 2:
            source, target = str(item[0]).strip(), str(item[1]).strip()
            if source and target and source != target:
                relations.append((source, target))

    graph = DocumentGraph(mode="llm")
    for doc in docs:
        key = _chunk_key(doc)
        graph.chunks[key] = Chunk(
            chunk_id=key,
            content=doc.content,
            score=doc.score,
            meta=dict(doc.meta),
            source="graph",
        )

    # Link entities back to chunks by substring evidence; entities absent from
    # every chunk are dropped so the graph only contains grounded terms.
    lowered = {cid: c.content.lower() for cid, c in graph.chunks.items()}
    for raw_term in data.get("entities") or []:
        term = str(raw_term).strip()
        needle = term.lower()
        if len(needle) < 2:
            continue
        chunk_ids = {cid for cid, content in lowered.items() if needle in content}
        if chunk_ids:
            graph.nodes[term] = GraphNode(
                term=term, chunk_ids=chunk_ids, weight=float(len(chunk_ids))
            )

    if not graph.nodes:
        return None
    for source, target in relations:
        if source in graph.nodes and target in graph.nodes:
            graph.add_edge(source, target)
    return graph


def build_graph(docs: list[Chunk]) -> DocumentGraph:
    return build_llm_graph(docs) or build_cooccurrence_graph(docs)


def query_graph(graph: DocumentGraph, query: str, top_k: int) -> list[Chunk]:
    """Ranks chunks by matched entities plus one-hop neighbours."""
    if top_k <= 0 or not graph.nodes:
        return []
    terms = extract_terms(query)
    matched: dict[str, float] = {}
    for term in terms:
        if term in graph.nodes:
            matched[term] = matched.get(term, 0.0) + 1.0
    if not matched:
        # Partial matches keep the graph useful for long natural-language turns.
        for term in graph.nodes:
            if any(len(t) >= 2 and (t in term or term in t) for t in terms):
                matched[term] = 0.5
    if not matched:
        return []

    scores: dict[str, float] = {}
    for term, base in matched.items():
        node = graph.nodes.get(term)
        if node is None:
            continue
        # Rare entities are more specific than ubiquitous ones.
        weight = base * (1.0 + 1.0 / math.log(2 + len(node.chunk_ids)))
        for chunk_id in node.chunk_ids:
            scores[chunk_id] = scores.get(chunk_id, 0.0) + weight
        for neighbour, edge_weight in graph.adjacency.get(term, {}).items():
            linked = graph.nodes.get(neighbour)
            if linked is None:
                continue
            hop = weight * 0.5 * math.log1p(edge_weight)
            for chunk_id in linked.chunk_ids:
                scores[chunk_id] = scores.get(chunk_id, 0.0) + hop

    ranked = sorted(scores.items(), key=lambda kv: (-kv[1], kv[0]))
    out: list[Chunk] = []
    for chunk_id, score in ranked[:top_k]:
        base_chunk = graph.chunks.get(chunk_id)
        if base_chunk is None:
            continue
        out.append(
            Chunk(
                chunk_id=base_chunk.chunk_id,
                content=base_chunk.content,
                score=score,
                meta=dict(base_chunk.meta),
                source="graph",
            )
        )
    return out


def extract_markdown_graph(markdown: str, max_chunks: int = 50) -> DocumentGraph:
    """Builds a graph from markdown paragraphs for the inspection endpoint."""
    paragraphs = [p.strip() for p in re.split(r"\n\s*\n", markdown or "") if p.strip()]
    docs = [
        Chunk(chunk_id=f"extract-{idx}", content=text)
        for idx, text in enumerate(paragraphs[:max_chunks])
    ]
    return build_graph(docs)


_graph_cache: dict[str, tuple[float, DocumentGraph]] = {}
_cache_lock = threading.Lock()


def _graph_store():
    from app.retrieval.graph_store import get_graph_store

    return get_graph_store()


def _remember(scope: str, graph: DocumentGraph) -> DocumentGraph:
    ttl = max(0.0, settings.graph_cache_seconds)
    with _cache_lock:
        _graph_cache[scope] = (time.monotonic() + ttl, graph)
    return graph


def cached_graph(collection: str, build: Callable[[], DocumentGraph]) -> DocumentGraph:
    """Caches the per-scope graph in-process and, when configured, in PostgreSQL.

    ``collection`` is tenant-scoped by the caller so graphs never cross tenants.
    """
    now = time.monotonic()
    with _cache_lock:
        cached = _graph_cache.get(collection)
        if cached is not None and cached[0] > now:
            return cached[1]

    store = _graph_store()
    graph: DocumentGraph | None = None
    if store is not None:
        payload = store.load(collection)
        if payload:
            from app.retrieval.graph_store import graph_from_payload

            graph = graph_from_payload(payload)
    if graph is None:
        graph = build()
        if store is not None:
            from app.retrieval.graph_store import graph_to_payload

            store.save(collection, graph_to_payload(graph))
    return _remember(collection, graph)


def clear_graph_cache(collection: str | None = None) -> None:
    """Drops cached graphs (in-process and persistent) after indexing/deletion."""
    with _cache_lock:
        if collection is None:
            _graph_cache.clear()
        else:
            prefix = collection + "|"
            for key in [
                key
                for key in _graph_cache
                if key == collection or key.startswith(prefix)
            ]:
                _graph_cache.pop(key, None)

    store = _graph_store()
    if store is None:
        return
    try:
        if collection is None:
            store.clear()
        else:
            store.delete_prefix(collection)
    except Exception as exc:  # noqa: BLE001 - retrieval must keep working
        logger.warning("graph snapshot invalidation failed: %s", exc)
