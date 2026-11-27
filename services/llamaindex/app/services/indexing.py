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

    # New chunks must appear in the graph route immediately, not after the TTL.
    clear_graph_cache(collection)

    return IndexResponse(
        success=True,
        chunk_count=len(nodes),
        indexed_count=len(nodes),
        deleted_count=deleted,
        collection=collection,
    )
