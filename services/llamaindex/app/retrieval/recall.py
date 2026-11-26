import json
from functools import lru_cache

from app.core.config import settings
from app.retrieval.fusion import Chunk


class RecallUnavailable(RuntimeError):
    pass


def _collection(filters: dict[str, str]) -> str:
    return f"{settings.collection_prefix}_{filters.get('kb_id', 'default')}"


@lru_cache
def _qdrant_client(collection: str):
    from qdrant_client import QdrantClient

    return QdrantClient(url=settings.qdrant_url)


def _embedding():
    from app.services.embedding import build_embedding

    return build_embedding(None)


def _metadata_filters(filters: dict[str, str]):
    from llama_index.core.vector_stores import FilterOperator, MetadataFilter, MetadataFilters

    conditions = [
        MetadataFilter(key=key, value=value, operator=FilterOperator.EQ)
        for key, value in filters.items()
        if key != "kb_id"
    ]
    return MetadataFilters(filters=conditions) if conditions else None


def _vector_recall(query: str, top_k: int, filters: dict[str, str]) -> list[Chunk]:
    from llama_index.core import VectorStoreIndex
    from llama_index.vector_stores.qdrant import QdrantVectorStore

    collection = _collection(filters)
    store = QdrantVectorStore(client=_qdrant_client(collection), collection_name=collection)
    index = VectorStoreIndex.from_vector_store(store, embed_model=_embedding())

    kwargs = {"similarity_top_k": top_k}
    metadata_filters = _metadata_filters(filters)
    if metadata_filters is not None:
        kwargs["filters"] = metadata_filters

    nodes = index.as_retriever(**kwargs).retrieve(query)
    return [
        Chunk(
            chunk_id=node.node.node_id,
            content=node.node.get_content(),
            score=float(node.score or 0.0),
            meta={key: str(value) for key, value in node.node.metadata.items()},
            source="vector",
        )
        for node in nodes
    ]


def _tokenize(text: str) -> list[str]:
    lowered = text.lower()
    if len(lowered) < 2:
        return [lowered] if lowered else []
    return [lowered[i : i + 2] for i in range(len(lowered) - 1)]


def _scroll_documents(filters: dict[str, str]) -> list[Chunk]:
    client = _qdrant_client(_collection(filters))
    points, _ = client.scroll(
        collection_name=_collection(filters),
        limit=1000,
        with_payload=True,
    )
    docs: list[Chunk] = []
    for point in points:
        payload = point.payload or {}
        meta: dict[str, str] = {}
        content = payload.get("text", "")
        raw = payload.get("_node_content")
        if raw:
            try:
                node = json.loads(raw)
                content = node.get("text", content)
                meta = {k: str(v) for k, v in node.get("metadata", {}).items()}
            except json.JSONDecodeError:
                pass
        if content:
            docs.append(Chunk(chunk_id=str(point.id), content=content, meta=meta, source="bm25"))
    return docs


def _bm25_recall(query: str, top_k: int, filters: dict[str, str]) -> list[Chunk]:
    from rank_bm25 import BM25Okapi

    docs = _scroll_documents(filters)
    if not docs:
        return []

    corpus = [_tokenize(doc.content) for doc in docs]
    bm25 = BM25Okapi(corpus)
    scores = bm25.get_scores(_tokenize(query))

    ranked = sorted(zip(docs, scores), key=lambda item: -item[1])[:top_k]
    results = []
    for doc, score in ranked:
        doc.score = float(score)
        results.append(doc)
    return results


def retrieve(route, query: str, filters: dict[str, str]) -> list[Chunk]:
    """Dispatch one recall route. Heavy imports stay inside the branches so
    the API can boot without optional integrations installed."""
    if route.type == "bm25":
        return _bm25_recall(query, route.top_k, filters)
    if route.type == "graph":
        raise RecallUnavailable("graph recall requires the LightRAG adapter")
    return _vector_recall(query, route.top_k, filters)
