"""Write-then-clean indexing.

Chunks get deterministic ids (see ``stable_node_id``), so re-indexing a
document upserts the same points. After the new points are written, any point
of that task whose id is no longer part of the new chunk set is deleted. The
old index stays readable until the new points land, so retrieval never sees an
empty window.
"""

from __future__ import annotations

import logging

from app.core.config import settings
from app.retrieval.graph import clear_graph_cache
from app.schemas import IndexRequest, IndexResponse
from app.services.chunking import build_nodes
from app.services.embedding import build_embedding
from app.services.sparse import (
    SPARSE_VECTOR_NAME,
    encode_sparse,
    sparse_collection_name,
)

logger = logging.getLogger(__name__)


def collection_for(kb_id: str | None) -> str:
    return f"{settings.collection_prefix}_{kb_id or 'default'}"


def stale_ids(old_ids: set[str], new_ids: set[str]) -> list[str]:
    """Ids that existed before the rebuild and are no longer produced."""
    return sorted(old_ids - new_ids)


def _task_point_ids(client, collection: str, task_id: str) -> set[str]:
    from qdrant_client import models as qmodels

    if not client.collection_exists(collection):
        return set()
    flt = qmodels.Filter(
        must=[
            qmodels.FieldCondition(
                key="task_id", match=qmodels.MatchValue(value=task_id)
            )
        ]
    )
    ids: set[str] = set()
    offset = None
    while True:
        points, offset = client.scroll(
            collection_name=collection,
            scroll_filter=flt,
            limit=256,
            offset=offset,
            with_payload=False,
        )
        ids.update(str(point.id) for point in points)
        if offset is None:
            break
    return ids


def _sparse_payload(task_id: str, node) -> dict[str, str]:
    payload: dict[str, str] = {}
    for key, value in node.metadata.items():
        if value is not None:
            payload[str(key)] = str(value)
    # The task id is the cascade-delete and reindex key, so never let metadata
    # shadow it.
    payload["task_id"] = task_id
    return payload


def _sparse_points(task_id: str, nodes) -> list:
    from qdrant_client import models as qmodels

    points = []
    for node in nodes:
        indices, values = encode_sparse(node.get_content())
        if not indices:
            continue
        points.append(
            qmodels.PointStruct(
                id=node.node_id,
                vector={
                    SPARSE_VECTOR_NAME: qmodels.SparseVector(
                        indices=indices, values=values
                    )
                },
                payload=_sparse_payload(task_id, node),
            )
        )
    return points


def _ensure_sparse_collection(client, collection: str) -> str:
    from qdrant_client import models as qmodels

    sparse_collection = sparse_collection_name(collection)
    if not client.collection_exists(sparse_collection):
        try:
            client.create_collection(
                collection_name=sparse_collection,
                vectors_config={},
                sparse_vectors_config={
                    SPARSE_VECTOR_NAME: qmodels.SparseVectorParams(
                        modifier=qmodels.Modifier.IDF
                    )
                },
            )
            logger.info("created sparse collection %s", sparse_collection)
        except Exception:
            # Another replica may have created it between the check and create.
            if not client.collection_exists(sparse_collection):
                raise
    return sparse_collection


def _write_sparse(client, collection: str, task_id: str, nodes) -> bool:
    """Best-effort sparse companion write; dense retrieval stays usable on failure."""
    try:
        sparse_collection = _ensure_sparse_collection(client, collection)
        points = _sparse_points(task_id, nodes)
        for start in range(0, len(points), 256):
            client.upsert(
                collection_name=sparse_collection,
                points=points[start : start + 256],
                wait=True,
            )
        return True
    except Exception as exc:  # noqa: BLE001 - bm25 route can fall back to scroll
        logger.warning("sparse indexing failed for %s: %s", collection, exc)
        return False


def _delete_sparse_points(client, collection: str, ids: list[str]) -> None:
    if not ids:
        return
    sparse_collection = sparse_collection_name(collection)
    try:
        if not client.collection_exists(sparse_collection):
            return
        from qdrant_client import models as qmodels

        client.delete(
            collection_name=sparse_collection,
            points_selector=qmodels.PointIdsList(points=ids),
            wait=True,
        )
    except Exception as exc:  # noqa: BLE001 - stale sparse points retry next run
        logger.warning(
            "delete stale sparse points failed for %s: %s", sparse_collection, exc
        )


def index_markdown(request: IndexRequest) -> IndexResponse:
    """Chunk, embed and persist a parsed document into the vector store."""
    collection = collection_for(request.metadata.get("kb_id"))
    nodes = build_nodes(request.markdown, request.split_strategy, request.metadata)
    if not nodes:
        return IndexResponse(success=False, error_msg="no nodes produced", collection=collection)

    from llama_index.core import StorageContext, VectorStoreIndex

    from app.core.vectorstore import get_vector_store

    qdrant = None
    old_ids: set[str] = set()
    try:
        from qdrant_client import QdrantClient

        qdrant = QdrantClient(url=settings.qdrant_url)
        # Snapshot the previous points before the upsert so the cleanup below
        # only removes what the rebuild actually replaced.
        old_ids = _task_point_ids(qdrant, collection, request.task_id)
    except Exception as exc:  # noqa: BLE001 - cleanup is best-effort
        logger.warning("snapshot old points failed for %s: %s", collection, exc)
        qdrant = None

    vector_store = get_vector_store(collection)
    storage_context = StorageContext.from_defaults(vector_store=vector_store)
    VectorStoreIndex(
        nodes,
        storage_context=storage_context,
        embed_model=build_embedding(request.embedding_model),
    )

    sparse_written = False
    if settings.sparse_enabled and qdrant is not None:
        sparse_written = _write_sparse(qdrant, collection, request.task_id, nodes)

    deleted = 0
    if qdrant is not None:
        stale = stale_ids(old_ids, {str(node.node_id) for node in nodes})
        if stale:
            try:
                from qdrant_client import models as qmodels

                qdrant.delete(
                    collection_name=collection,
                    points_selector=qmodels.PointIdsList(points=stale),
                    wait=True,
                )
                deleted = len(stale)
            except Exception as exc:  # noqa: BLE001 - stale points retry next run
                logger.warning("delete stale points failed for %s: %s", collection, exc)
            if sparse_written:
                _delete_sparse_points(qdrant, collection, stale)

    # New chunks must appear in the graph route immediately, not after the TTL.
    clear_graph_cache(collection)

    return IndexResponse(
        success=True,
        chunk_count=len(nodes),
        indexed_count=len(nodes),
        deleted_count=deleted,
        collection=collection,
    )
