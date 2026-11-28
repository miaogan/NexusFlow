from __future__ import annotations

import re
from typing import Any, Literal

from fastapi import APIRouter, HTTPException, Request
from pydantic import BaseModel, Field

from nexusflow_agents.core.config import get_hub_settings
from nexusflow_agents.core.executor import validate_sandbox_config
from nexusflow_agents.core.registry import VALID_CAPABILITIES, AgentSpec
from nexusflow_agents.core.runtime import get_agent_store, get_registry

router = APIRouter(prefix="/v1", tags=["hub"])

_NAME = re.compile(r"^[a-z0-9][a-z0-9._-]{1,63}$")
# Built-in spokes are shared; a custom agent must not shadow their names.
RESERVED_PREFIXES = ("local.", "remote.")
MAX_SKILLS = 16


class AgentRegistration(BaseModel):
    name: str = Field(min_length=2, max_length=64)
    capabilities: list[str] = Field(min_length=1)
    kind: Literal["http", "sandbox"] = "http"
    # Required for http agents; unused for sandbox agents.
    endpoint: str = Field(default="", max_length=512)
    # Sandbox agents: {code, runtime, timeout_seconds, packages, env_vars}.
    sandbox: dict[str, Any] = Field(default_factory=dict)
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
    # Names exposed to dynamic skill routing, e.g. ["expense.approval"].
    skills: list[str] = Field(default_factory=list, max_length=MAX_SKILLS)
    model: str = Field(default="", max_length=128)
    max_turns: int = Field(default=0, ge=0, le=50)
    token_budget: int = Field(default=0, ge=0)
    risk: Literal["read", "write", "execute", "external"] = "read"


def _tenant(request: Request) -> str:
    value = (request.headers.get("x-tenant-id") or "default").strip()
    return value[:64] or "default"


def _authorize(request: Request) -> None:
    settings = get_hub_settings()
    if not settings.dynamic_registration:
        raise HTTPException(status_code=403, detail="dynamic registration is disabled")
    # The gateway relays admin writes with x-service-token (never exposing the
    # admin token to browsers); accept both headers like /v1/call does.
    supplied = request.headers.get("x-admin-token") or request.headers.get(
        "x-service-token"
    )
    if settings.admin_token and supplied != settings.admin_token:
        raise HTTPException(status_code=401, detail="invalid admin token")


def _find_agent(tenant_id: str, name: str) -> dict[str, Any] | None:
    items = [
        item
        for item in get_registry().snapshot(tenant_id)
        if item["name"] == name
    ]
    if not items:
        return None
    exact = [item for item in items if item["tenant_id"] == tenant_id]
    return (exact or items)[0]


@router.get("/agents")
def list_agents(request: Request) -> dict[str, object]:
    tenant_id = _tenant(request)
    return {
        "agents": get_registry().snapshot(tenant_id),
        "mode": get_hub_settings().mode,
        "tenant_id": tenant_id,
    }


@router.get("/skills")
def list_skills(request: Request) -> dict[str, Any]:
    tenant_id = _tenant(request)
    return {
        "tenant_id": tenant_id,
        "skills": get_registry().available_skills(tenant_id),
    }


@router.get("/agents/{name}")
def get_agent(request: Request, name: str) -> dict[str, Any]:
    tenant_id = _tenant(request)
    item = _find_agent(tenant_id, name)
    if item is None:
        raise HTTPException(status_code=404, detail="agent not found")
    return item


@router.post("/agents", status_code=201)
def register_agent(request: Request, body: AgentRegistration) -> dict[str, object]:
    _authorize(request)
    tenant_id = _tenant(request)
    if not _NAME.match(body.name):
        raise HTTPException(status_code=422, detail="invalid agent name")
    if body.name.startswith(RESERVED_PREFIXES):
        raise HTTPException(status_code=422, detail="agent name is reserved")
    invalid = sorted(set(body.capabilities) - VALID_CAPABILITIES)
    if invalid:
        raise HTTPException(
            status_code=422, detail=f"unknown capabilities: {', '.join(invalid)}"
        )
    invalid_skills = sorted(
        skill for skill in body.skills if not _NAME.match(skill)
    )
    if invalid_skills:
        raise HTTPException(
            status_code=422,
            detail=f"invalid skill names: {', '.join(invalid_skills)}",
        )

    sandbox_config: dict[str, Any] = {}
    if body.kind == "sandbox":
        sandbox_input = dict(body.sandbox)
        # The code is never returned by list/get, so an update that changes
        # only skills/priority can omit it and reuse the stored payload.
        if not sandbox_input.get("code"):
            existing = get_registry().get(body.name, tenant_id)
            if existing is not None and existing.kind == "sandbox":
                sandbox_input = {**existing.sandbox, **sandbox_input}
        try:
            sandbox_config = validate_sandbox_config(sandbox_input)
        except (TypeError, ValueError) as exc:
            raise HTTPException(status_code=422, detail=str(exc)) from exc
    elif not body.endpoint.startswith(("http://", "https://")):
        raise HTTPException(status_code=422, detail="endpoint must be http(s)")

    spec = AgentSpec(
        name=body.name,
        capabilities=frozenset(body.capabilities),
        kind=body.kind,
        endpoint="" if body.kind == "sandbox" else body.endpoint,
        timeout_ms=body.timeout_ms,
        priority=body.priority,
        # Custom agents persist until deleted unless a heartbeat TTL is asked for.
        ttl_seconds=body.ttl_seconds,
        version=body.version,
        input_schema=body.input_schema,
        output_schema=body.output_schema,
        permissions=frozenset(body.permissions),
        tenant_scope=body.tenant_scope,
        tools=frozenset(body.tools),
        skills=frozenset(body.skills),
        sandbox=sandbox_config,
        model=body.model,
        max_turns=body.max_turns,
        token_budget=body.token_budget,
        # Running user code is always an execute-risk agent.
        risk="execute" if body.kind == "sandbox" else body.risk,
        tenant_id=tenant_id,
    )
    try:
        get_agent_store().upsert(
            spec, created_by=request.headers.get("x-user-id") or "anonymous"
        )
    except Exception as exc:
        raise HTTPException(
            status_code=500, detail=f"persist agent failed: {exc}"
        ) from exc
    get_registry().register(spec)
    return {
        "registered": body.name,
        "tenant_id": tenant_id,
        "kind": body.kind,
        "ttl_seconds": body.ttl_seconds,
    }


@router.delete("/agents/{name}")
def deregister_agent(request: Request, name: str) -> dict[str, object]:
    _authorize(request)
    tenant_id = _tenant(request)
    try:
        deleted = get_agent_store().delete(name, tenant_id)
    except Exception as exc:
        raise HTTPException(
            status_code=500, detail=f"delete agent failed: {exc}"
        ) from exc
    removed = get_registry().deregister(name, tenant_id)
    return {
        "removed": name,
        "tenant_id": tenant_id,
        "found": bool(deleted or removed),
    }


@router.post("/agents/{name}/heartbeat")
def heartbeat(request: Request, name: str) -> dict[str, object]:
    _authorize(request)
    tenant_id = _tenant(request)
    settings = get_hub_settings()
    ok = get_registry().heartbeat(name, settings.registry_ttl_seconds, tenant_id)
    if not ok:
        raise HTTPException(status_code=404, detail="agent not found")
    return {"agent": name, "tenant_id": tenant_id, "ttl_seconds": settings.registry_ttl_seconds}
