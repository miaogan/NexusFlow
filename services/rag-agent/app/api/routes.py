from __future__ import annotations

from fastapi import APIRouter, Depends, HTTPException, Query, Request
from starlette.concurrency import run_in_threadpool

from app.deps import get_service
from app.schemas import (
    ChatRequest,
    ChatResponse,
    ConversationDetail,
    ConversationListResponse,
)
from app.services.chat import ChatService
from app.services.retrieval import RetrievalUnavailable
from app.services.store import ConversationNotFound

router = APIRouter(prefix="/v1", tags=["rag"])


def caller_identity(request: Request) -> tuple[str, str, str | None]:
    """Reads the principal injected by the Go gateway."""
    tenant_id = request.headers.get("x-tenant-id") or "default"
    user_id = request.headers.get("x-user-id") or "anonymous"
    return tenant_id, user_id, request.headers.get("authorization")


def service_dep() -> ChatService:
    return get_service()


@router.post("/chat", response_model=ChatResponse)
async def chat(
    req: ChatRequest,
    request: Request,
    service: ChatService = Depends(service_dep),
) -> ChatResponse:
    tenant_id, user_id, auth_header = caller_identity(request)
    try:
        return await run_in_threadpool(
            service.chat, req, tenant_id, user_id, auth_header
        )
    except ConversationNotFound as exc:
        raise HTTPException(status_code=404, detail=str(exc)) from exc
    except RetrievalUnavailable as exc:
        raise HTTPException(status_code=503, detail=str(exc)) from exc


@router.get("/conversations", response_model=ConversationListResponse)
async def list_conversations(
    request: Request,
    limit: int = Query(20, ge=1, le=100),
    offset: int = Query(0, ge=0),
    service: ChatService = Depends(service_dep),
) -> ConversationListResponse:
    tenant_id, user_id, _ = caller_identity(request)
    return await run_in_threadpool(
        service.list_conversations, tenant_id, user_id, limit, offset
    )


@router.get("/conversations/{conversation_id}", response_model=ConversationDetail)
async def get_conversation(
    conversation_id: str,
    request: Request,
    service: ChatService = Depends(service_dep),
) -> ConversationDetail:
    tenant_id, user_id, _ = caller_identity(request)
    try:
        return await run_in_threadpool(
            service.get_conversation, tenant_id, user_id, conversation_id
        )
    except ConversationNotFound as exc:
        raise HTTPException(status_code=404, detail=str(exc)) from exc
