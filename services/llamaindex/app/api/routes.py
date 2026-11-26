from fastapi import APIRouter, HTTPException
from starlette.concurrency import run_in_threadpool

from app.core.config import settings
from app.feedback.store import FeedbackStore
from app.retrieval.pipeline import search
from app.retrieval.recall import RecallUnavailable
from app.schemas import (
    FeedbackEvent,
    IndexRequest,
    IndexResponse,
    SearchRequest,
    SearchResponse,
)
from app.services.indexing import index_markdown

router = APIRouter(prefix="/v1", tags=["llamaindex"])
feedback_store = FeedbackStore(settings.feedback_path)


@router.post("/index", response_model=IndexResponse)
async def process_and_index(request: IndexRequest) -> IndexResponse:
    try:
        return await run_in_threadpool(index_markdown, request)
    except RuntimeError as exc:
        raise HTTPException(status_code=503, detail=str(exc)) from exc


@router.post("/search", response_model=SearchResponse)
async def search_endpoint(request: SearchRequest) -> SearchResponse:
    try:
        return await search(request)
    except RecallUnavailable as exc:
        raise HTTPException(status_code=503, detail=str(exc)) from exc


@router.post("/feedback", status_code=202)
async def submit_feedback(event: FeedbackEvent) -> dict[str, bool]:
    feedback_store.append(event)
    return {"accepted": True}
