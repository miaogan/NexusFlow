from __future__ import annotations

from typing import Any

import httpx

from nexusflow_agents.agents.rag.core.config import settings as rag_settings
from nexusflow_agents.core.tooling import ToolError, ToolService, ToolSpec


def _headers(ctx: dict[str, Any]) -> dict[str, str]:
    headers = {}
    for key, header in (
        ("auth_header", "authorization"),
        ("trace_id", "x-trace-id"),
        ("tenant_id", "x-tenant-id"),
        ("user_id", "x-user-id"),
    ):
        value = ctx.get(key)
        if value:
            headers[header] = str(value)
    return headers


def search_query(payload: dict[str, Any], ctx: dict[str, Any]) -> dict[str, Any]:
    body = {
        "query": str(payload.get("query") or ""),
        "routes": [
            {"type": "vector", "top_k": int(payload.get("top_k") or 20)},
            {"type": "bm25", "top_k": int(payload.get("top_k") or 20)},
        ],
        "final_top_n": int(payload.get("final_top_n") or 5),
        "kb_id": str(payload.get("kb_id") or "default"),
    }
    try:
        resp = httpx.post(
            f"{rag_settings.gateway_url.rstrip('/')}/v1/search",
            json=body,
            headers=_headers(ctx),
            timeout=rag_settings.search_timeout_seconds,
        )
        resp.raise_for_status()
        data = resp.json()
    except httpx.HTTPError as exc:
        raise ToolError(f"search failed: {exc}") from exc
    return data if isinstance(data, dict) else {"chunks": []}


def sandbox_execute(payload: dict[str, Any], ctx: dict[str, Any]) -> dict[str, Any]:
    body = {
        "code": str(payload.get("code") or ""),
        "runtime": str(payload.get("runtime") or "python"),
        "timeout_seconds": int(payload.get("timeout_seconds") or 10),
    }
    try:
        resp = httpx.post(
            f"{rag_settings.gateway_url.rstrip('/')}/v1/sandbox/execute",
            json=body,
            headers=_headers(ctx),
            timeout=float(payload.get("timeout_seconds") or 10) + 5,
        )
        resp.raise_for_status()
        data = resp.json()
    except httpx.HTTPError as exc:
        raise ToolError(f"sandbox execution failed: {exc}") from exc
    return data if isinstance(data, dict) else {"output": ""}


SEARCH_SPEC = ToolSpec(
    name="search.query",
    description="Hybrid retrieval over the current knowledge base.",
    input_schema={
        "type": "object",
        "required": ["query"],
        "properties": {
            "query": {"type": "string"},
            "top_k": {"type": "integer"},
            "final_top_n": {"type": "integer"},
            "kb_id": {"type": "string"},
        },
    },
    risk="read",
    timeout_ms=15000,
)

SANDBOX_SPEC = ToolSpec(
    name="sandbox.execute",
    description="Execute code in an isolated sandbox.",
    input_schema={
        "type": "object",
        "required": ["code"],
        "properties": {
            "code": {"type": "string"},
            "runtime": {"type": "string", "enum": ["python", "node"]},
            "timeout_seconds": {"type": "integer"},
        },
    },
    risk="execute",
    timeout_ms=20000,
    requires_approval=True,
)


def register_gateway_tools(service: ToolService) -> None:
    service.register(SEARCH_SPEC, search_query)
    service.register(SANDBOX_SPEC, sandbox_execute)
