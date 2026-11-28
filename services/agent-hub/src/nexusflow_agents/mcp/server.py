from __future__ import annotations

import asyncio
import json
import logging
import uuid
from typing import Any

from fastapi import APIRouter, HTTPException, Query, Request
from fastapi.responses import JSONResponse, Response, StreamingResponse

from nexusflow_agents.core.config import get_hub_settings
from nexusflow_agents.core.runtime import get_tools
from nexusflow_agents.core.tooling import ApprovalRequired, ToolError

logger = logging.getLogger(__name__)

router = APIRouter(tags=["mcp"])

PROTOCOL_VERSION = "2024-11-05"
SERVER_INFO = {"name": "nexusflow-agent-hub", "version": "1.0.0"}

# HTTP+SSE transport sessions: session_id -> response queue. Sessions are
# process-local; multi-replica deployments need sticky sessions or the
# streamable HTTP transport.
_sse_sessions: dict[str, asyncio.Queue[dict[str, Any]]] = {}
MAX_SSE_SESSIONS = 64


def _check_access(request: Request) -> None:
    settings = get_hub_settings()
    if not settings.mcp_enabled:
        raise HTTPException(status_code=404, detail="MCP is disabled")
    if settings.admin_token:
        supplied = request.headers.get("x-admin-token") or request.headers.get(
            "x-service-token"
        )
        if supplied != settings.admin_token:
            raise HTTPException(status_code=401, detail="invalid service token")


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


def error_payload(
    request_id: Any, code: int, message: str, data: Any = None
) -> dict[str, Any]:
    error: dict[str, Any] = {"code": code, "message": message}
    if data is not None:
        error["data"] = data
    return {"jsonrpc": "2.0", "id": request_id, "error": error}


def _result_payload(request_id: Any, result: Any) -> dict[str, Any]:
    return {"jsonrpc": "2.0", "id": request_id, "result": result}


def handle_payload(payload: Any) -> dict[str, Any] | None:
    """Dispatches one JSON-RPC payload and returns the response object.

    Returns None for notifications (no response is written). Shared by the
    HTTP, SSE and stdio transports.
    """
    if isinstance(payload, list):
        return error_payload(None, -32600, "batch requests are not supported")
    if not isinstance(payload, dict):
        return error_payload(None, -32600, "invalid request")

    request_id = payload.get("id")
    method = str(payload.get("method") or "")
    params = payload.get("params") or {}
    if payload.get("jsonrpc") != "2.0" or not method:
        return error_payload(request_id, -32600, "invalid JSON-RPC request")
    if not isinstance(params, dict):
        return error_payload(request_id, -32602, "params must be an object")

    if method in {"notifications/initialized", "initialized"}:
        return None
    if method.startswith("notifications/"):
        return None
    if "id" not in payload:
        # JSON-RPC notifications never receive a response.
        return None

    if method == "initialize":
        return _result_payload(
            request_id,
            {
                "protocolVersion": PROTOCOL_VERSION,
                "capabilities": {"tools": {"listChanged": False}},
                "serverInfo": SERVER_INFO,
            },
        )
    if method == "ping":
        return _result_payload(request_id, {})
    if method == "tools/list":
        return _result_payload(request_id, {"tools": _tool_snapshot()})
    if method == "tools/call":
        return _call_tool(request_id, params)
    return error_payload(request_id, -32601, f"method not found: {method}")


def _call_tool(request_id: Any, params: dict[str, Any]) -> dict[str, Any]:
    name = str(params.get("name") or "")
    arguments = params.get("arguments") or {}
    if not name:
        return error_payload(request_id, -32602, "tool name is required")
    if not isinstance(arguments, dict):
        return error_payload(request_id, -32602, "arguments must be an object")

    tools = get_tools()
    spec = tools.registry.get(name)
    if spec is None:
        return error_payload(request_id, -32602, f"unknown tool: {name}")
    if spec.name == "sandbox.execute" and not get_hub_settings().mcp_allow_sandbox:
        return error_payload(request_id, -32601, "tool is not exposed over MCP")

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
            return error_payload(
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
        return error_payload(
            request_id,
            -32002,
            "approval required",
            {"tool": exc.tool, "risk": exc.risk, "approval_id": ctx["approval_id"]},
        )
    except ToolError as exc:
        return _result_payload(
            request_id,
            {
                "content": [{"type": "text", "text": str(exc)}],
                "isError": True,
            },
        )
    return _result_payload(
        request_id,
        {
            "content": [
                {"type": "text", "text": json.dumps(data, ensure_ascii=False, default=str)}
            ],
            "isError": False,
        },
    )


def _decode_body(raw: bytes, max_bytes: int) -> tuple[Any, dict[str, Any] | None]:
    """Parses a request body, returning (payload, early_error_response)."""
    if len(raw) > max_bytes:
        return None, error_payload(None, -32600, "request body too large")
    try:
        return json.loads(raw or b"{}"), None
    except json.JSONDecodeError as exc:
        return None, error_payload(None, -32700, f"parse error: {exc}")


@router.post("/mcp")
async def mcp_endpoint(request: Request) -> Response:
    """HTTP JSON-RPC transport (request/response in one round trip)."""
    _check_access(request)
    settings = get_hub_settings()
    raw = await request.body()
    payload, early = _decode_body(raw, settings.mcp_max_bytes)
    if early is not None:
        return JSONResponse(status_code=200, content=early)
    response = handle_payload(payload)
    if response is None:
        return Response(status_code=202)
    return JSONResponse(status_code=200, content=response)


@router.get("/mcp/sse")
async def mcp_sse(request: Request) -> StreamingResponse:
    """HTTP+SSE transport: the stream announces the POST endpoint, then
    relays JSON-RPC responses produced by /mcp/messages for this session."""
    _check_access(request)
    if len(_sse_sessions) >= MAX_SSE_SESSIONS:
        raise HTTPException(status_code=503, detail="too many MCP sessions")
    session_id = uuid.uuid4().hex
    queue: asyncio.Queue[dict[str, Any]] = asyncio.Queue()
    _sse_sessions[session_id] = queue
    endpoint = f"/mcp/messages?session_id={session_id}"

    async def event_stream():
        try:
            yield f"event: endpoint\ndata: {endpoint}\n\n"
            while True:
                if await request.is_disconnected():
                    break
                try:
                    message = await asyncio.wait_for(queue.get(), timeout=15.0)
                except TimeoutError:
                    yield ": keepalive\n\n"
                    continue
                data = json.dumps(message, ensure_ascii=False, default=str)
                yield f"event: message\ndata: {data}\n\n"
        finally:
            _sse_sessions.pop(session_id, None)

    return StreamingResponse(
        event_stream(),
        media_type="text/event-stream",
        headers={
            "Cache-Control": "no-cache",
            "Connection": "keep-alive",
            "X-Accel-Buffering": "no",
        },
    )


@router.post("/mcp/messages")
async def mcp_messages(
    request: Request, session_id: str = Query(default="", max_length=64)
) -> Response:
    """Accepts JSON-RPC for an SSE session and pushes the response upstream."""
    _check_access(request)
    queue = _sse_sessions.get(session_id)
    if queue is None:
        raise HTTPException(status_code=404, detail="unknown MCP session")
    settings = get_hub_settings()
    raw = await request.body()
    payload, early = _decode_body(raw, settings.mcp_max_bytes)
    response = early if early is not None else handle_payload(payload)
    if response is not None:
        queue.put_nowait(response)
    return Response(status_code=202)
