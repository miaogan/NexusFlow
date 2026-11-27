from __future__ import annotations

from typing import Any, Literal

from fastapi import APIRouter, HTTPException, Query, Request
from pydantic import BaseModel, Field

from nexusflow_agents.agents.rag.services.approvals import (
    ApprovalConflict,
    ApprovalNotFound,
    get_approvals,
)
from nexusflow_agents.core.config import get_hub_settings

router = APIRouter(prefix="/v1", tags=["approvals"])


class DecisionBody(BaseModel):
    decision: Literal["approve", "reject"]
    reason: str = Field(default="", max_length=512)


def _authorize(request: Request) -> None:
    settings = get_hub_settings()
    if not settings.admin_token:
        return
    supplied = request.headers.get("x-admin-token") or request.headers.get(
        "x-service-token"
    )
    if supplied != settings.admin_token:
        raise HTTPException(status_code=401, detail="invalid service token")


@router.get("/approvals")
def list_approvals(
    request: Request,
    status: str = Query(default="", max_length=16),
    tenant_id: str = Query(default="", max_length=128),
) -> dict[str, Any]:
    _authorize(request)
    scope = tenant_id or request.headers.get("x-tenant-id") or ""
    return {"approvals": get_approvals().list(status=status, tenant_id=scope)}


@router.get("/approvals/{approval_id}")
def get_approval(request: Request, approval_id: str) -> dict[str, Any]:
    _authorize(request)
    record = get_approvals().get(approval_id)
    if record is None:
        raise HTTPException(status_code=404, detail="approval not found")
    return record.snapshot()


@router.post("/approvals/{approval_id}/decide")
def decide_approval(
    request: Request, approval_id: str, body: DecisionBody
) -> dict[str, Any]:
    _authorize(request)
    decided_by = request.headers.get("x-user-id") or "anonymous"
    tenant_id = request.headers.get("x-tenant-id") or ""
    try:
        record = get_approvals().decide(
            approval_id,
            body.decision,
            decided_by=decided_by,
            reason=body.reason,
            tenant_id=tenant_id,
        )
    except ApprovalNotFound as exc:
        raise HTTPException(status_code=404, detail="approval not found") from exc
    except ApprovalConflict as exc:
        raise HTTPException(status_code=409, detail=str(exc)) from exc
    return record.snapshot()
