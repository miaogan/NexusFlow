import json
import logging
from functools import lru_cache

from app.core.config import settings
from app.retrieval import lightrag_engine
from app.retrieval.fusion import Chunk
from app.retrieval.graph import build_graph, cached_graph, query_graph
from app.services.sparse import (
    SPARSE_VECTOR_NAME,
    encode_sparse,
    sparse_collection_name,
)

logger = logging.getLogger(__name__)


class RecallUnavailable(RuntimeError):
    pass


def _collection(filters: dict[str, str]) -> str:
    return f"{settings.collection_prefix}_{filters.get('kb_id', 'default')}"


def _graph_scope(filters: dict[str, str]) -> str:
    """Tenant-scoped cache/store key: one KB collection can span tenants."""
    collection = _collection(filters)
    tenant = filters.get("tenant_id", "")
    return f"{collection}|tenant={tenant}" if tenant else collection


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
        if key != "kb_id" and value
    ]
    return MetadataFilters(filters=conditions) if conditions else None


def _payload_filter(filters: dict[str, str]):
    """Qdrant filter for the sparse companion collection (metadata is string)."""
    from qdrant_client import models as qmodels

    conditions = [
        qmodels.FieldCondition(key=key, match=qmodels.MatchValue(value=value))
        for key, value in filters.items()
        if key != "kb_id" and value
    ]
    return qmodels.Filter(must=conditions) if conditions else None


def _chunk_from_point(point, source: str) -> Chunk | None:
    payload = point.payload or {}
    content = payload.get("text", "")
    meta: dict[str, str] = {}
    raw = payload.get("_node_content")
    if raw:
        try:
            node = json.loads(raw)
            content = node.get("text", content)
            meta = {k: str(v) for k, v in node.get("metadata", {}).items()}
        except json.JSONDecodeError:
            pass
    if not content:
        return None
    return Chunk(chunk_id=str(point.id), content=content, meta=meta, source=source)


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
    # The collection is shared by KB id; tenant/metadata filters must be
    # applied here too, otherwise BM25 and the graph route leak other tenants.
    points, _ = client.scroll(
        collection_name=_collection(filters),
        scroll_filter=_payload_filter(filters),
        limit=1000,
        with_payload=True,
    )
    docs: list[Chunk] = []
    for point in points:
        chunk = _chunk_from_point(point, "bm25")
        if chunk is not None:
            docs.append(chunk)
    return docs


def _sparse_recall(query: str, top_k: int, filters: dict[str, str]) -> list[Chunk]:
    """Qdrant sparse recall over the companion collection.

    Returns an empty list when sparse indexing is disabled, the companion
    collection does not exist yet, or the query has no tokens, so the caller
    can fall back to the legacy scroll + BM25 implementation.
    """
    if not settings.sparse_enabled:
        return []

    from qdrant_client import models as qmodels

    collection = _collection(filters)
    sparse_collection = sparse_collection_name(collection)
    indices, values = encode_sparse(query)
    if not indices:
        return []

    client = _qdrant_client(collection)
    try:
        if not client.collection_exists(sparse_collection):
            return []
        result = client.query_points(
            collection_name=sparse_collection,
            query=qmodels.SparseVector(indices=indices, values=values),
            using=SPARSE_VECTOR_NAME,
            query_filter=_payload_filter(filters),
            limit=top_k,
            with_payload=False,
        )
        points = getattr(result, "points", result) or []
    except Exception as exc:  # noqa: BLE001 - fall back to scroll BM25
        logger.warning("sparse recall failed for %s: %s", sparse_collection, exc)
        return []
    if not points:
        return []

    ids = [str(point.id) for point in points]
    try:
        records = client.retrieve(
            collection_name=collection, ids=ids, with_payload=True
        )
    except Exception as exc:  # noqa: BLE001 - fall back to scroll BM25
        logger.warning("sparse content lookup failed for %s: %s", collection, exc)
        return []

    chunks = {}
    for record in records:
        chunk = _chunk_from_point(record, "bm25")
        if chunk is not None:
            chunks[chunk.chunk_id] = chunk

    out: list[Chunk] = []
    for point in points:
        chunk = chunks.get(str(point.id))
        if chunk is None:
            continue
        chunk.score = float(point.score or 0.0)
        out.append(chunk)
    return out


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


def _graph_recall(query: str, top_k: int, filters: dict[str, str]) -> list[Chunk]:
    if lightrag_engine.enabled():
        chunks = lightrag_engine.query(
            filters.get("tenant_id", ""),
            filters.get("kb_id", "default"),
            query,
            top_k,
        )
        if chunks:
            return chunks
        logger.warning("lightrag returned no chunks, using the in-house graph")
    scope = _graph_scope(filters)
    graph = cached_graph(scope, lambda: build_graph(_scroll_documents(filters)))
    return query_graph(graph, query, top_k)


def retrieve(route, query: str, filters: dict[str, str]) -> list[Chunk]:
    """Dispatch one recall route. Heavy imports stay inside the branches so
    the API can boot without optional integrations installed."""
    if route.type == "bm25":
        sparse = _sparse_recall(query, route.top_k, filters)
        if len(sparse) >= route.top_k:
            return sparse
        # Documents indexed before the sparse companion collection existed have
        # no sparse vector yet; merge legacy results until they are reindexed.
        legacy = _bm25_recall(query, route.top_k, filters)
        if not sparse:
            return legacy
        seen = {chunk.chunk_id for chunk in sparse}
        merged = sparse + [chunk for chunk in legacy if chunk.chunk_id not in seen]
        return merged[: route.top_k]
    if route.type == "graph":
        return _graph_recall(query, route.top_k, filters)
    return _vector_recall(query, route.top_k, filters)
