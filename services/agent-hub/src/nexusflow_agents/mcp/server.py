from __future__ import annotations

import json
import logging
from typing import Any

from fastapi import APIRouter, HTTPException, Request
from fastapi.responses import JSONResponse, Response

from nexusflow_agents.core.config import get_hub_settings
from nexusflow_agents.core.runtime import get_tools
from nexusflow_agents.core.tooling import ApprovalRequired, ToolError

logger = logging.getLogger(__name__)

router = APIRouter(tags=["mcp"])

PROTOCOL_VERSION = "2024-11-05"
SERVER_INFO = {"name": "nexusflow-agent-hub", "version": "1.0.0"}


def _tool_snapshot() -> list[dict[str, Any]]:
    settings = get_hub_settings()
    tools = []
    for spec in get_tools().specs():
        if spec["name"] == "sandbox.execute" and not settings.mcp_allow_sandbox:
            continue
        tools.append(
            {
                "name": spec["name"],
                "description": spec["description"],
                "inputSchema": spec["input_schema"] or {"type": "object", "properties": {}},
                "annotations": {
                    "risk": spec["risk"],
                    "requiresApproval": spec["requires_approval"],
                },
            }
        )
    return tools


def _error(request_id: Any, code: int, message: str, data: Any = None) -> JSONResponse:
    error: dict[str, Any] = {"code": code, "message": message}
    if data is not None:
        error["data"] = data
    return JSONResponse(
        status_code=200,
        content={"jsonrpc": "2.0", "id": request_id, "error": error},
    )


def _result(request_id: Any, result: Any) -> JSONResponse:
    return JSONResponse(
        status_code=200,
        content={"jsonrpc": "2.0", "id": request_id, "result": result},
    )


@router.post("/mcp")
async def mcp_endpoint(request: Request) -> Response:
    settings = get_hub_settings()
    if not settings.mcp_enabled:
        raise HTTPException(status_code=404, detail="MCP is disabled")
    if settings.admin_token:
        supplied = request.headers.get("x-admin-token") or request.headers.get(
            "x-service-token"
        )
        if supplied != settings.admin_token:
            raise HTTPException(status_code=401, detail="invalid service token")

    raw = await request.body()
    if len(raw) > settings.mcp_max_bytes:
        return _error(None, -32600, "request body too large")
    try:
        payload = json.loads(raw or b"{}")
    except json.JSONDecodeError as exc:
        return _error(None, -32700, f"parse error: {exc}")
    if isinstance(payload, list):
        return _error(None, -32600, "batch requests are not supported")
    if not isinstance(payload, dict):
        return _error(None, -32600, "invalid request")

    request_id = payload.get("id")
    method = str(payload.get("method") or "")
    params = payload.get("params") or {}
    if payload.get("jsonrpc") != "2.0" or not method:
        return _error(request_id, -32600, "invalid JSON-RPC request")
    if not isinstance(params, dict):
        return _error(request_id, -32602, "params must be an object")

    if method == "initialize":
        return _result(
            request_id,
            {
                "protocolVersion": PROTOCOL_VERSION,
                "capabilities": {"tools": {"listChanged": False}},
                "serverInfo": SERVER_INFO,
            },
        )
    if method in {"notifications/initialized", "initialized"}:
        return Response(status_code=202)
    if method == "ping":
        return _result(request_id, {})
    if method == "tools/list":
        return _result(request_id, {"tools": _tool_snapshot()})
    if method == "tools/call":
        return _call_tool(request_id, params)
    return _error(request_id, -32601, f"method not found: {method}")


def _call_tool(request_id: Any, params: dict[str, Any]) -> JSONResponse:
    name = str(params.get("name") or "")
    arguments = params.get("arguments") or {}
    if not name:
        return _error(request_id, -32602, "tool name is required")
    if not isinstance(arguments, dict):
        return _error(request_id, -32602, "arguments must be an object")

    tools = get_tools()
    spec = tools.registry.get(name)
    if spec is None:
        return _error(request_id, -32602, f"unknown tool: {name}")
    if spec.name == "sandbox.execute" and not get_hub_settings().mcp_allow_sandbox:
        return _error(request_id, -32601, "tool is not exposed over MCP")

    meta = params.get("_meta") or {}
    ctx = {
        "tenant_id": str(meta.get("tenantId") or "default"),
        "user_id": str(meta.get("userId") or "anonymous"),
        "trace_id": str(meta.get("traceId") or ""),
        "approval_id": str(meta.get("approvalId") or ""),
    }
    approved = False
    if spec.requires_approval:
        from nexusflow_agents.agents.rag.services.approvals import get_approvals

        service = get_approvals()
        approved = bool(
            ctx["approval_id"] and service.is_approved(ctx["approval_id"], name)
        )
        if not approved:
            record = service.create(name, arguments, spec.risk, ctx)
            return _error(
                request_id,
                -32002,
                "approval required",
                {
                    "status": "approval_required",
                    "tool": name,
                    "risk": spec.risk,
                    "approval_id": record.id,
                },
            )
    try:
        data = tools.call(name, arguments, ctx, approved=approved)
    except ApprovalRequired as exc:
        return _error(
            request_id,
            -32002,
            "approval required",
            {"tool": exc.tool, "risk": exc.risk, "approval_id": ctx["approval_id"]},
        )
    except ToolError as exc:
        return _result(
            request_id,
            {
                "content": [{"type": "text", "text": str(exc)}],
                "isError": True,
            },
        )
    return _result(
        request_id,
        {
            "content": [
                {"type": "text", "text": json.dumps(data, ensure_ascii=False, default=str)}
            ],
            "isError": False,
        },
    )
