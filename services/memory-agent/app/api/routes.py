from __future__ import annotations

from fastapi import APIRouter, Depends, Query

from app.deps import get_extract_graph, get_recall_graph, get_store
from app.graph import ExtractGraph, RecallGraph
from app.schemas import (
    ExtractRequest,
    ExtractResponse,
    MemoryItem,
    SearchRequest,
    SearchResponse,
)
from app.store import MemoryStore

router = APIRouter(prefix="/v1", tags=["memory"])


def _item(row: dict) -> MemoryItem:
    return MemoryItem(
        id=str(row.get("id") or ""),
        content=str(row.get("content") or ""),
        kind=row.get("kind") or "fact",
        score=float(row.get("score") or 0.0),
    )


@router.post("/memories/search", response_model=SearchResponse)
def search(
    req: SearchRequest,
    graph: RecallGraph = Depends(get_recall_graph),
) -> SearchResponse:
    result = graph.invoke(req.tenant_id, req.user_id, req.query, req.top_k)
    return SearchResponse(
        memories=[_item(row) for row in result["memories"]],
        source=result["source"],
    )


@router.post("/memories/extract", response_model=ExtractResponse)
def extract(
    req: ExtractRequest,
    graph: ExtractGraph = Depends(get_extract_graph),
) -> ExtractResponse:
    messages = [message.model_dump() for message in req.messages]
    result = graph.invoke(req.tenant_id, req.user_id, messages)
    return ExtractResponse(
        created=result["created"],
        merged=result["merged"],
        memories=[_item(row) for row in result["memories"]],
    )


@router.get("/memories", response_model=list[MemoryItem])
def list_memories(
    tenant_id: str = Query(min_length=1, max_length=128),
    user_id: str = Query(min_length=1, max_length=128),
    limit: int = Query(20, ge=1, le=100),
    store: MemoryStore = Depends(get_store),
) -> list[MemoryItem]:
    return [_item(row) for row in store.list(tenant_id, user_id, limit)]
