from __future__ import annotations

import re
from typing import Any, Literal

from fastapi import APIRouter, HTTPException, Request
from pydantic import BaseModel, Field

from nexusflow_agents.core.config import get_hub_settings
from nexusflow_agents.core.registry import VALID_CAPABILITIES, AgentSpec
from nexusflow_agents.core.runtime import get_registry

router = APIRouter(prefix="/v1", tags=["hub"])

_NAME = re.compile(r"^[a-z0-9][a-z0-9._-]{1,63}$")


class AgentRegistration(BaseModel):
    name: str = Field(min_length=2, max_length=64)
    capabilities: list[str] = Field(min_length=1)
    endpoint: str = Field(min_length=1, max_length=512)
    timeout_ms: int = Field(default=5000, ge=100, le=120000)
    priority: int = Field(default=100, ge=0, le=1000)
    ttl_seconds: int = Field(default=0, ge=0, le=86400)
    # Standard agent contract shared with /v1/call and MCP.
    version: str = Field(default="1.0", max_length=32)
    input_schema: dict[str, Any] = Field(default_factory=dict)
    output_schema: dict[str, Any] = Field(default_factory=dict)
    permissions: list[str] = Field(default_factory=list)
    tenant_scope: str = Field(default="", max_length=128)
    tools: list[str] = Field(default_factory=list)
    model: str = Field(default="", max_length=128)
    max_turns: int = Field(default=0, ge=0, le=50)
    token_budget: int = Field(default=0, ge=0)
    risk: Literal["read", "write", "execute", "external"] = "read"


def _authorize(request: Request) -> None:
    settings = get_hub_settings()
    if not settings.dynamic_registration:
        raise HTTPException(status_code=403, detail="dynamic registration is disabled")
    if settings.admin_token and request.headers.get("x-admin-token") != settings.admin_token:
        raise HTTPException(status_code=401, detail="invalid admin token")


@router.get("/agents")
def list_agents() -> dict[str, object]:
    return {"agents": get_registry().snapshot(), "mode": get_hub_settings().mode}


@router.post("/agents", status_code=201)
def register_agent(request: Request, body: AgentRegistration) -> dict[str, object]:
    _authorize(request)
    if not _NAME.match(body.name):
        raise HTTPException(status_code=422, detail="invalid agent name")
    if not body.endpoint.startswith(("http://", "https://")):
        raise HTTPException(status_code=422, detail="endpoint must be http(s)")
    invalid = sorted(set(body.capabilities) - VALID_CAPABILITIES)
    if invalid:
        raise HTTPException(
            status_code=422, detail=f"unknown capabilities: {', '.join(invalid)}"
        )
    settings = get_hub_settings()
    ttl = body.ttl_seconds or settings.registry_ttl_seconds
    get_registry().register(
        AgentSpec(
            name=body.name,
            capabilities=frozenset(body.capabilities),
            kind="http",
            endpoint=body.endpoint,
            timeout_ms=body.timeout_ms,
            priority=body.priority,
            ttl_seconds=ttl,
            version=body.version,
            input_schema=body.input_schema,
            output_schema=body.output_schema,
            permissions=frozenset(body.permissions),
            tenant_scope=body.tenant_scope,
            tools=frozenset(body.tools),
            model=body.model,
            max_turns=body.max_turns,
            token_budget=body.token_budget,
            risk=body.risk,
        )
    )
    return {"registered": body.name, "ttl_seconds": ttl}


@router.delete("/agents/{name}")
def deregister_agent(request: Request, name: str) -> dict[str, object]:
    _authorize(request)
    return {"removed": name, "found": get_registry().deregister(name)}


@router.post("/agents/{name}/heartbeat")
def heartbeat(request: Request, name: str) -> dict[str, object]:
    _authorize(request)
    settings = get_hub_settings()
    ok = get_registry().heartbeat(name, settings.registry_ttl_seconds)
    if not ok:
        raise HTTPException(status_code=404, detail="agent not found")
    return {"agent": name, "ttl_seconds": settings.registry_ttl_seconds}
