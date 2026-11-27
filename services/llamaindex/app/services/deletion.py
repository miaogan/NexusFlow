"""Cascade cleanup for a deleted document.

A document's chunks and its LightRAG-style graph nodes share the same Qdrant
collection, so removing the chunks and dropping the cached graph removes every
retrieval-side trace of the document in one call.
"""

from __future__ import annotations

from app.core.config import settings
from app.retrieval.graph import clear_graph_cache
from app.schemas import DeleteRequest, DeleteResponse


def collection_for(kb_id: str | None) -> str:
    return f"{settings.collection_prefix}_{kb_id or 'default'}"


def _task_filter(task_id: str):
    from qdrant_client import models as qmodels

    return qmodels.Filter(
        must=[
            qmodels.FieldCondition(
                key="task_id", match=qmodels.MatchValue(value=task_id)
            )
        ]
    )


def delete_document(request: DeleteRequest, client=None) -> DeleteResponse:
    """Deletes the document's vectors and invalidates its graph cache."""
    collection = collection_for(request.kb_id)
    if client is None:
        from qdrant_client import QdrantClient

        client = QdrantClient(url=settings.qdrant_url)

    # Qdrant deletes are idempotent: a missing collection simply means the
    # document was never indexed (or was already deleted).
    if not client.collection_exists(collection):
        clear_graph_cache(collection)
        return DeleteResponse(success=True, deleted=0, collection=collection)

    flt = _task_filter(request.task_id)
    deleted = 0
    try:
        deleted = int(client.count(collection_name=collection, count_filter=flt, exact=True).count)
    except Exception:  # noqa: BLE001 - counting is advisory, deletion still proceeds
        deleted = 0

    client.delete(collection_name=collection, points_selector=flt, wait=True)
    clear_graph_cache(collection)
    return DeleteResponse(success=True, deleted=deleted, collection=collection)
