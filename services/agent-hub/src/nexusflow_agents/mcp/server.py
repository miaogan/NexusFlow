from __future__ import annotations

import asyncio
import hashlib
import json
import logging
import uuid
from typing import Any
from urllib.parse import unquote

from fastapi import APIRouter, HTTPException, Query, Request
from fastapi.responses import JSONResponse, Response, StreamingResponse

from nexusflow_agents.core.config import get_hub_settings
from nexusflow_agents.core.runtime import get_registry, get_tools
from nexusflow_agents.core.tooling import ApprovalRequired, ToolError

logger = logging.getLogger(__name__)

router = APIRouter(tags=["mcp"])

PROTOCOL_VERSION = "2024-11-05"
SERVER_INFO = {"name": "nexusflow-agent-hub", "version": "1.0.0"}

# Read-only MCP resources expose the live Hub registry and tool catalog.
RESOURCE_AGENTS = "nexusflow://agents"
RESOURCE_SKILLS = "nexusflow://skills"
RESOURCE_TOOLS = "nexusflow://tools"
AGENTS_PREFIX = RESOURCE_AGENTS + "/"
TOOLS_PREFIX = RESOURCE_TOOLS + "/"

# HTTP+SSE transport sessions: session_id -> response queue. Sessions are
# process-local; multi-replica deployments need sticky sessions or the
# streamable HTTP transport.
_sse_sessions: dict[str, asyncio.Queue[dict[str, Any]]] = {}
MAX_SSE_SESSIONS = 64

# Streamable HTTP sessions (2025-03-26 transport): server-initiated messages
# are queued for the GET stream. Subscriptions are process-local.
_stream_sessions: dict[str, asyncio.Queue[dict[str, Any]]] = {}
_stream_tenants: dict[str, str] = {}
_stream_subscriptions: dict[str, set[str]] = {}
_stream_fingerprints: dict[str, dict[str, str]] = {}
MAX_STREAM_SESSIONS = 64


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


def _request_tenant(params: dict[str, Any]) -> str:
    meta = params.get("_meta") or {}
    if not isinstance(meta, dict):
        return ""
    return str(meta.get("tenantId") or meta.get("tenant_id") or "")


def _resource_defs() -> list[dict[str, Any]]:
    return [
        {
            "uri": RESOURCE_AGENTS,
            "name": "agents",
            "description": "Registered agents with health, capabilities and skills.",
            "mimeType": "application/json",
        },
        {
            "uri": RESOURCE_SKILLS,
            "name": "skills",
            "description": "Skills visible to the caller tenant.",
            "mimeType": "application/json",
        },
        {
            "uri": RESOURCE_TOOLS,
            "name": "tools",
            "description": "Tools exposed over MCP with their input schemas.",
            "mimeType": "application/json",
        },
    ]


def _resource_templates() -> list[dict[str, Any]]:
    return [
        {
            "uriTemplate": AGENTS_PREFIX + "{name}",
            "name": "agent",
            "description": "A single registered agent by name.",
            "mimeType": "application/json",
        },
        {
            "uriTemplate": TOOLS_PREFIX + "{name}",
            "name": "tool",
            "description": "A single MCP tool by name.",
            "mimeType": "application/json",
        },
    ]


def _resource_body(uri: str, tenant: str) -> Any | None:
    """Returns the JSON body for a resource URI, or None when it is unknown."""
    if uri == RESOURCE_AGENTS:
        return {"agents": get_registry().snapshot(tenant)}
    if uri == RESOURCE_SKILLS:
        return {"skills": get_registry().available_skills(tenant)}
    if uri == RESOURCE_TOOLS:
        return {"tools": _tool_snapshot()}
    if uri.startswith(AGENTS_PREFIX):
        name = unquote(uri[len(AGENTS_PREFIX) :])
        matches = [
            item
            for item in get_registry().snapshot(tenant)
            if item.get("name") == name
        ]
        return matches[0] if matches else None
    if uri.startswith(TOOLS_PREFIX):
        name = unquote(uri[len(TOOLS_PREFIX) :])
        return next(
            (item for item in _tool_snapshot() if item.get("name") == name), None
        )
    return None


def _read_resource(request_id: Any, params: dict[str, Any]) -> dict[str, Any]:
    uri = params.get("uri")
    if not isinstance(uri, str) or not uri:
        return error_payload(request_id, -32602, "resource uri is required")
    body = _resource_body(uri, _request_tenant(params))
    if body is None:
        return error_payload(request_id, -32002, f"resource not found: {uri}")
    return _result_payload(
        request_id,
        {
            "contents": [
                {
                    "uri": uri,
                    "mimeType": "application/json",
                    "text": json.dumps(body, ensure_ascii=False, default=str),
                }
            ]
        },
    )


def _fingerprint(body: Any) -> str:
    raw = json.dumps(body, ensure_ascii=False, sort_keys=True, default=str)
    return hashlib.sha256(raw.encode("utf-8")).hexdigest()


def _subscribe_resource(
    request_id: Any, params: dict[str, Any], session_id: str
) -> dict[str, Any]:
    if not session_id or session_id not in _stream_sessions:
        return error_payload(
            request_id, -32602, "resources/subscribe requires a streamable HTTP session"
        )
    uri = params.get("uri")
    if not isinstance(uri, str) or not uri:
        return error_payload(request_id, -32602, "resource uri is required")
    body = _resource_body(uri, _stream_tenants.get(session_id, ""))
    if body is None:
        return error_payload(request_id, -32002, f"resource not found: {uri}")
    _stream_subscriptions.setdefault(session_id, set()).add(uri)
    _stream_fingerprints.setdefault(session_id, {})[uri] = _fingerprint(body)
    return _result_payload(request_id, {})


def _unsubscribe_resource(
    request_id: Any, params: dict[str, Any], session_id: str
) -> dict[str, Any]:
    if not session_id or session_id not in _stream_sessions:
        return error_payload(
            request_id,
            -32602,
            "resources/unsubscribe requires a streamable HTTP session",
        )
    uri = params.get("uri")
    if not isinstance(uri, str) or not uri:
        return error_payload(request_id, -32602, "resource uri is required")
    _stream_subscriptions.get(session_id, set()).discard(uri)
    _stream_fingerprints.get(session_id, {}).pop(uri, None)
    return _result_payload(request_id, {})


def _collect_updates(session_id: str) -> list[dict[str, Any]]:
    """Diffs subscribed resources against their last fingerprint."""
    uris = _stream_subscriptions.get(session_id)
    if not uris:
        return []
    tenant = _stream_tenants.get(session_id, "")
    seen = _stream_fingerprints.setdefault(session_id, {})
    updates: list[dict[str, Any]] = []
    for uri in list(uris):
        body = _resource_body(uri, tenant)
        if body is None:
            continue
        digest = _fingerprint(body)
        if seen.get(uri) == digest:
            continue
        seen[uri] = digest
        updates.append(
            {
                "jsonrpc": "2.0",
                "method": "notifications/resources/updated",
                "params": {"uri": uri},
            }
        )
    return updates


def _drop_stream_session(session_id: str) -> None:
    _stream_sessions.pop(session_id, None)
    _stream_tenants.pop(session_id, None)
    _stream_subscriptions.pop(session_id, None)
    _stream_fingerprints.pop(session_id, None)


def _prompt_defs() -> list[dict[str, Any]]:
    return [
        {
            "name": "rag_answer",
            "description": "Answer a question strictly from supplied context.",
            "arguments": [
                {"name": "question", "description": "User question.", "required": True},
                {
                    "name": "context",
                    "description": "Retrieved context blocks.",
                    "required": False,
                },
                {
                    "name": "system",
                    "description": "Extra system instructions.",
                    "required": False,
                },
            ],
        },
        {
            "name": "summarize",
            "description": "Compress a conversation or document into a summary.",
            "arguments": [
                {"name": "text", "description": "Text to summarize.", "required": True},
                {
                    "name": "max_chars",
                    "description": "Target length in characters (100-4000).",
                    "required": False,
                },
            ],
        },
        {
            "name": "intent_classify",
            "description": "Classify a user message into the Hub intent space.",
            "arguments": [
                {"name": "message", "description": "User message.", "required": True},
                {
                    "name": "skills",
                    "description": "Comma-separated skill names.",
                    "required": False,
                },
            ],
        },
    ]


def _prompt_text(request_id: Any, params: dict[str, Any]) -> tuple[str, str] | dict:
    name = str(params.get("name") or "")
    arguments = params.get("arguments") or {}
    if not name:
        return error_payload(request_id, -32602, "prompt name is required")
    if not isinstance(arguments, dict):
        return error_payload(request_id, -32602, "arguments must be an object")

    def arg(key: str) -> str:
        value = arguments.get(key)
        return str(value).strip() if value is not None else ""

    if name == "rag_answer":
        question = arg("question")
        if not question:
            return error_payload(request_id, -32602, "question is required")
        from nexusflow_agents.agents.rag.services.llm import SYSTEM_PROMPT

        parts = [SYSTEM_PROMPT]
        extra = arg("system")
        if extra:
            parts.append(extra)
        context = arg("context")
        if context:
            parts.append(f"资料（位于 <context> 标签内）：\n<context>\n{context}\n</context>")
        parts.append(f"问题：{question}")
        return name, "\n\n".join(parts)

    if name == "summarize":
        text = arg("text")
        if not text:
            return error_payload(request_id, -32602, "text is required")
        raw_budget = arg("max_chars")
        if raw_budget:
            try:
                budget = int(raw_budget)
            except ValueError:
                return error_payload(request_id, -32602, "max_chars must be an integer")
        else:
            budget = 1200
        budget = max(100, min(budget, 4000))
        return (
            name,
            f"请将以下内容压缩为不超过 {budget} 字的中文摘要，"
            "保留关键事实、结论与未决问题：\n\n" + text,
        )

    if name == "intent_classify":
        message = arg("message")
        if not message:
            return error_payload(request_id, -32602, "message is required")
        skills = arg("skills") or "无"
        return (
            name,
            (
                "请对用户消息做意图分类，只输出 JSON："
                '{"intent": "knowledge|chitchat|clarify|skill", '
                '"skill": "<技能名或空>"}。\n'
                f"可用技能：{skills}\n用户消息：{message}"
            ),
        )

    return error_payload(request_id, -32602, f"unknown prompt: {name}")


def _get_prompt(request_id: Any, params: dict[str, Any]) -> dict[str, Any]:
    result = _prompt_text(request_id, params)
    if isinstance(result, dict):
        return result
    name, text = result
    description = next(
        (item["description"] for item in _prompt_defs() if item["name"] == name), ""
    )
    return _result_payload(
        request_id,
        {
            "description": description,
            "messages": [
                {"role": "user", "content": {"type": "text", "text": text}}
            ],
        },
    )


def error_payload(
    request_id: Any, code: int, message: str, data: Any = None
) -> dict[str, Any]:
    error: dict[str, Any] = {"code": code, "message": message}
    if data is not None:
        error["data"] = data
    return {"jsonrpc": "2.0", "id": request_id, "error": error}


def _result_payload(request_id: Any, result: Any) -> dict[str, Any]:
    return {"jsonrpc": "2.0", "id": request_id, "result": result}


def handle_payload(payload: Any, session_id: str = "") -> dict[str, Any] | None:
    """Dispatches one JSON-RPC payload and returns the response object.

    Returns None for notifications (no response is written). Shared by the
    JSON-RPC, legacy SSE, streamable HTTP and stdio transports; session_id is
    only set for transports that support server-initiated subscriptions.
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
                "capabilities": {
                    "tools": {"listChanged": False},
                    "resources": {"subscribe": True, "listChanged": False},
                    "prompts": {"listChanged": False},
                },
                "serverInfo": SERVER_INFO,
            },
        )
    if method == "ping":
        return _result_payload(request_id, {})
    if method == "tools/list":
        return _result_payload(request_id, {"tools": _tool_snapshot()})
    if method == "tools/call":
        return _call_tool(request_id, params)
    if method == "resources/list":
        return _result_payload(request_id, {"resources": _resource_defs()})
    if method == "resources/templates/list":
        return _result_payload(
            request_id, {"resourceTemplates": _resource_templates()}
        )
    if method == "resources/read":
        return _read_resource(request_id, params)
    if method == "resources/subscribe":
        return _subscribe_resource(request_id, params, session_id)
    if method == "resources/unsubscribe":
        return _unsubscribe_resource(request_id, params, session_id)
    if method == "prompts/list":
        return _result_payload(request_id, {"prompts": _prompt_defs()})
    if method == "prompts/get":
        return _get_prompt(request_id, params)
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
    """Streamable HTTP POST: one JSON-RPC round trip, optionally as SSE.

    initialize creates a session and returns its id in ``Mcp-Session-Id`` so
    the client can open GET /mcp for server-initiated notifications.
    """
    _check_access(request)
    settings = get_hub_settings()
    raw = await request.body()
    payload, early = _decode_body(raw, settings.mcp_max_bytes)
    if early is not None:
        return JSONResponse(status_code=200, content=early)

    session_id = request.headers.get("mcp-session-id", "")
    if session_id and session_id not in _stream_sessions:
        raise HTTPException(status_code=404, detail="unknown MCP session")
    if (
        not session_id
        and isinstance(payload, dict)
        and payload.get("method") == "initialize"
    ):
        if len(_stream_sessions) >= MAX_STREAM_SESSIONS:
            raise HTTPException(status_code=503, detail="too many MCP sessions")
        session_id = uuid.uuid4().hex
        _stream_sessions[session_id] = asyncio.Queue()
        params = payload.get("params") or {}
        _stream_tenants[session_id] = (
            _request_tenant(params) if isinstance(params, dict) else ""
        )

    response = handle_payload(payload, session_id)
    headers = {"Mcp-Session-Id": session_id} if session_id else {}
    if response is None:
        return Response(status_code=202, headers=headers)
    if "text/event-stream" in request.headers.get("accept", ""):
        async def one_shot():
            data = json.dumps(response, ensure_ascii=False, default=str)
            yield f"event: message\ndata: {data}\n\n"

        return StreamingResponse(
            one_shot(),
            media_type="text/event-stream",
            headers={
                **headers,
                "Cache-Control": "no-cache",
                "X-Accel-Buffering": "no",
            },
        )
    return JSONResponse(status_code=200, content=response, headers=headers)


@router.get("/mcp")
async def mcp_stream(request: Request) -> StreamingResponse:
    """Streamable HTTP GET: server-initiated messages for a session.

    Subscribed resources are diffed on every keepalive tick and emitted as
    ``notifications/resources/updated`` when their snapshot changes.
    """
    _check_access(request)
    if "text/event-stream" not in request.headers.get("accept", ""):
        raise HTTPException(
            status_code=406, detail="Accept must include text/event-stream"
        )
    session_id = request.headers.get("mcp-session-id", "")
    if session_id:
        if session_id not in _stream_sessions:
            raise HTTPException(status_code=404, detail="unknown MCP session")
    else:
        if len(_stream_sessions) >= MAX_STREAM_SESSIONS:
            raise HTTPException(status_code=503, detail="too many MCP sessions")
        session_id = uuid.uuid4().hex
        _stream_sessions[session_id] = asyncio.Queue()
        _stream_tenants[session_id] = ""
    queue = _stream_sessions[session_id]

    async def event_stream():
        try:
            yield ": connected\n\n"
            while True:
                if await request.is_disconnected():
                    break
                try:
                    message = await asyncio.wait_for(queue.get(), timeout=15.0)
                except TimeoutError:
                    for update in _collect_updates(session_id):
                        data = json.dumps(update, ensure_ascii=False, default=str)
                        yield f"event: message\ndata: {data}\n\n"
                    yield ": keepalive\n\n"
                    continue
                data = json.dumps(message, ensure_ascii=False, default=str)
                yield f"event: message\ndata: {data}\n\n"
        finally:
            _drop_stream_session(session_id)

    return StreamingResponse(
        event_stream(),
        media_type="text/event-stream",
        headers={
            "Mcp-Session-Id": session_id,
            "Cache-Control": "no-cache",
            "Connection": "keep-alive",
            "X-Accel-Buffering": "no",
        },
    )


@router.delete("/mcp")
async def mcp_close(request: Request) -> Response:
    """Terminates a streamable HTTP session and its subscriptions."""
    _check_access(request)
    session_id = request.headers.get("mcp-session-id", "")
    if not session_id or session_id not in _stream_sessions:
        raise HTTPException(status_code=404, detail="unknown MCP session")
    _drop_stream_session(session_id)
    return Response(status_code=204)


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
