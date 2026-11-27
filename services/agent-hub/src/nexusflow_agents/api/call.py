from __future__ import annotations

from typing import Any

from fastapi import APIRouter, HTTPException, Request
from pydantic import BaseModel, Field

from nexusflow_agents.core.config import get_hub_settings
from nexusflow_agents.core.envelope import CallEnvelope
from nexusflow_agents.core.runtime import get_hub, get_tools
from nexusflow_agents.core.tooling import (
    ApprovalRequired,
    ToolError,
    validate_payload,
)

router = APIRouter(prefix="/v1", tags=["call"])


class AgentTarget(BaseModel):
    name: str = Field(default="", max_length=128)
    capability: str = Field(default="", max_length=128)
    input: dict[str, Any] = Field(default_factory=dict)


class ToolTarget(BaseModel):
    name: str = Field(min_length=1, max_length=128)
    input: dict[str, Any] = Field(default_factory=dict)


class CallRequest(BaseModel):
    agent: AgentTarget | None = None
    tool: ToolTarget | None = None
    envelope: CallEnvelope = Field(default_factory=CallEnvelope)


def _authorize(request: Request) -> None:
    """The hub is internal; a configured admin token also guards /v1/call."""
    settings = get_hub_settings()
    if not settings.admin_token:
        return
    supplied = request.headers.get("x-admin-token") or request.headers.get(
        "x-service-token"
    )
    if supplied != settings.admin_token:
        raise HTTPException(status_code=401, detail="invalid service token")


def _envelope(request: Request, body: CallEnvelope) -> CallEnvelope:
    """Body fields win; gateway headers fill in the gaps."""
    updates = {
        "tenant_id": request.headers.get("x-tenant-id"),
        "user_id": request.headers.get("x-user-id"),
        "trace_id": request.headers.get("x-trace-id"),
        "request_id": request.headers.get("x-request-id"),
        "approval_id": request.headers.get("x-approval-id"),
    }
    deadline = request.headers.get("x-deadline-ms")
    data = body.model_dump()
    provided = body.model_fields_set
    for key, value in updates.items():
        if value and key not in provided:
            data[key] = value
    if deadline and "deadline_ms" not in provided:
        try:
            data["deadline_ms"] = max(0, int(deadline))
        except ValueError:
            pass
    return CallEnvelope(**data)


def _enabled() -> None:
    settings = get_hub_settings()
    if not (settings.tools_enabled or settings.workflow_enabled):
        raise HTTPException(status_code=404, detail="/v1/call is disabled")


@router.get("/tools")
def list_tools() -> dict[str, Any]:
    _enabled()
    return {"tools": get_tools().specs()}


@router.post("/call")
def call(request: Request, body: CallRequest) -> dict[str, Any]:
    _enabled()
    _authorize(request)
    envelope = _envelope(request, body.envelope)
    ctx = envelope.context()

    if body.tool is not None:
        return _call_tool(body.tool, ctx)
    if body.agent is not None:
        return _call_agent(body.agent, ctx)
    raise HTTPException(status_code=422, detail="either agent or tool is required")


def _call_tool(target: ToolTarget, ctx: dict[str, Any]) -> dict[str, Any]:
    tools = get_tools()
    spec = tools.registry.get(target.name)
    if spec is None:
        raise HTTPException(status_code=404, detail=f"unknown tool: {target.name}")
    approved = False
    if spec.requires_approval:
        from nexusflow_agents.agents.rag.services.approvals import get_approvals

        service = get_approvals()
        approval_id = str(ctx.get("approval_id") or "")
        approved = bool(approval_id and service.is_approved(approval_id, target.name))
        if not approved:
            record = service.create(target.name, target.input, spec.risk, ctx)
            raise HTTPException(
                status_code=202,
                detail={
                    "status": "approval_required",
                    "tool": target.name,
                    "risk": spec.risk,
                    "approval_id": record.id,
                },
            )
    try:
        validate_payload(spec.input_schema, target.input)
        data = tools.call(target.name, target.input, ctx, approved=approved)
    except ApprovalRequired as exc:
        raise HTTPException(
            status_code=202,
            detail={
                "status": "approval_required",
                "tool": exc.tool,
                "risk": exc.risk,
                "approval_id": ctx.get("approval_id") or "",
            },
        ) from exc
    except ToolError as exc:
        message = str(exc)
        status = 429 if ("budget" in message or "deadline" in message) else 400
        raise HTTPException(status_code=status, detail=message) from exc
    return {"ok": True, "tool": target.name, "data": data}


def _call_agent(target: AgentTarget, ctx: dict[str, Any]) -> dict[str, Any]:
    hub = get_hub()
    if not target.capability and not target.name:
        raise HTTPException(status_code=422, detail="agent.capability is required")
    if target.name:
        result = hub.call_agent(
            target.name,
            target.capability,
            target.input,
            ctx,
            required=False,
        )
    else:
        result = hub.call(
            target.capability,
            target.input,
            ctx,
            required=False,
        )
    payload = {
        "ok": result.ok,
        "agent": result.agent or target.name,
        "data": result.data,
        "error": result.error,
        "latency_ms": result.latency_ms,
        "degraded": result.degraded,
        "attempts": result.attempts,
    }
    if not result.ok:
        raise HTTPException(status_code=503, detail=payload)
    return payload
