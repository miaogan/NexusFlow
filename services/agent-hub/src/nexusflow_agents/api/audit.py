from __future__ import annotations

from datetime import UTC, datetime
from typing import Any

from fastapi import APIRouter, HTTPException, Query, Request

from nexusflow_agents.core.audit import get_audit
from nexusflow_agents.core.config import get_hub_settings

router = APIRouter(prefix="/v1", tags=["audit"])


def _authorize(request: Request) -> None:
    settings = get_hub_settings()
    if not settings.admin_token:
        return
    supplied = request.headers.get("x-admin-token") or request.headers.get(
        "x-service-token"
    )
    if supplied != settings.admin_token:
        raise HTTPException(status_code=401, detail="invalid service token")


@router.get("/audit")
def list_audit(
    request: Request,
    limit: int = Query(default=100, ge=1, le=500),
    action: str = Query(default="", max_length=64),
    decision: str = Query(default="", max_length=32),
    since: str = Query(default="", max_length=40),
    until: str = Query(default="", max_length=40),
) -> dict[str, Any]:
    _authorize(request)
    try:
        since_epoch = _epoch(since)
        until_epoch = _epoch(until)
    except ValueError as exc:
        raise HTTPException(status_code=400, detail=f"invalid time range: {exc}") from exc
    tenant_id = request.headers.get("x-tenant-id") or ""
    return {
        "events": get_audit().list(
            limit,
            tenant_id=tenant_id,
            action=action,
            decision=decision,
            since=since_epoch,
            until=until_epoch,
        )
    }


def _epoch(value: str) -> float:
    """Accepts unix seconds or RFC3339 (``Z`` suffix included)."""
    text = value.strip()
    if not text:
        return 0.0
    try:
        return float(text)
    except ValueError:
        pass
    parsed = datetime.fromisoformat(text)
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=UTC)
    return parsed.timestamp()
