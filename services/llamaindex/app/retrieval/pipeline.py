import asyncio
import logging
import uuid

from starlette.concurrency import run_in_threadpool

from app.core.config import settings
from app.retrieval.fusion import rrf_fuse
from app.retrieval.recall import RecallUnavailable, retrieve
from app.retrieval.rerank import rerank
from app.schemas import RecallRoute, RetrievedChunk, SearchRequest, SearchResponse

logger = logging.getLogger(__name__)


async def search(request: SearchRequest) -> SearchResponse:
    routes = request.routes or [RecallRoute(type="vector", top_k=20)]
    filters = {**request.filters, "kb_id": request.kb_id}

    results = await asyncio.gather(
        *(run_in_threadpool(retrieve, route, request.query, filters) for route in routes),
        return_exceptions=True,
    )

    lists = []
    recall_count = 0
    succeeded = 0
    for route, result in zip(routes, results):
        if isinstance(result, Exception):
            # Fail-open: one broken route must not take the query down.
            logger.warning("recall route %s failed: %s", route.type, result)
            continue
        succeeded += 1
        if result:
            lists.append(result)
            recall_count += len(result)

    if succeeded == 0:
        raise RecallUnavailable("no recall route produced results")

    if not lists:
        # Every route answered successfully; the knowledge base simply has no
        # matching chunks yet. That is an empty result, not an outage.
        return SearchResponse(
            chunks=[], trace_id=uuid.uuid4().hex, recall_count=0, rerank_applied=False
        )

    fused = rrf_fuse(lists, settings.rrf_k)

    rerank_applied = False
    if request.rerank_model and settings.rerank_url and fused:
        try:
            fused = await run_in_threadpool(
                rerank, request.query, fused, request.rerank_model, request.final_top_n
            )
            rerank_applied = True
        except Exception as exc:  # degrade to RRF (fail-open)
            logger.warning("rerank failed, falling back to RRF: %s", exc)

    fused = fused[: max(1, request.final_top_n)]

    return SearchResponse(
        chunks=[
            RetrievedChunk(
                chunk_id=chunk.chunk_id,
                content=chunk.content,
                score=chunk.score,
                meta=chunk.meta,
            )
            for chunk in fused
        ],
        trace_id=uuid.uuid4().hex,
        recall_count=recall_count,
        rerank_applied=rerank_applied,
    )
