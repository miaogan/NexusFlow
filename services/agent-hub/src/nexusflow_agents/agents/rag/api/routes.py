from __future__ import annotations

import json

from fastapi import APIRouter, Depends, HTTPException, Query, Request
from fastapi.responses import StreamingResponse
from starlette.concurrency import run_in_threadpool

from nexusflow_agents.agents.rag.deps import get_service
from nexusflow_agents.agents.rag.schemas import (
    ChatRequest,
    ChatResponse,
    ConversationDetail,
    ConversationListResponse,
)
from nexusflow_agents.agents.rag.services.chat import ChatService
from nexusflow_agents.agents.rag.services.llm import (
    GenerationOptions,
    reset_generation_options,
    reset_usage,
    set_generation_options,
    start_usage,
)
from nexusflow_agents.agents.rag.services.retrieval import RetrievalUnavailable
from nexusflow_agents.agents.rag.services.store import ConversationNotFound

router = APIRouter(prefix="/v1", tags=["rag"])


def generation_options(req: ChatRequest) -> GenerationOptions:
    return GenerationOptions(
        system_prompt=req.system_prompt or "",
        temperature=req.temperature,
        max_tokens=req.max_tokens,
    )


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
    token = set_generation_options(generation_options(req))
    usage_token = start_usage()
    try:
        return await run_in_threadpool(
            service.chat, req, tenant_id, user_id, auth_header
        )
    except ConversationNotFound as exc:
        raise HTTPException(status_code=404, detail=str(exc)) from exc
    except RetrievalUnavailable as exc:
        raise HTTPException(status_code=503, detail=str(exc)) from exc
    finally:
        reset_usage(usage_token)
        reset_generation_options(token)


@router.post("/chat/stream")
async def chat_stream(
    req: ChatRequest,
    request: Request,
    service: ChatService = Depends(service_dep),
) -> StreamingResponse:
    tenant_id, user_id, auth_header = caller_identity(request)

    async def event_source():
        token = set_generation_options(generation_options(req))
        usage_token = start_usage()
        try:
            async for event in service.chat_stream(
                req, tenant_id, user_id, auth_header
            ):
                yield f"data: {json.dumps(event, ensure_ascii=False)}\n\n"
        except Exception as exc:  # noqa: BLE001 - keep the stream well-formed
            payload = json.dumps({"type": "error", "error": str(exc)}, ensure_ascii=False)
            yield f"data: {payload}\n\n"
        finally:
            reset_usage(usage_token)
            reset_generation_options(token)

    return StreamingResponse(
        event_source(),
        media_type="text/event-stream",
        headers={
            "Cache-Control": "no-cache",
            "Connection": "keep-alive",
            "X-Accel-Buffering": "no",
        },
    )


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
