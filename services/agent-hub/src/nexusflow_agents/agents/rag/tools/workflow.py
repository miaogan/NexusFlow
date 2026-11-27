from __future__ import annotations

from typing import Any

import httpx

from nexusflow_agents.agents.rag.core.config import settings as rag_settings
from nexusflow_agents.core.config import get_hub_settings
from nexusflow_agents.core.tooling import ToolError, ToolService, ToolSpec

MAX_NODES = 50
MAX_EDGES = 100
ALLOWED_TYPES = {
    "noop",
    "set",
    "ingest",
    "parse",
    "index",
    "sandbox",
    "query",
    "nl2sql",
    "agent",
    "human_approval",
}


def _validate_dag(dag: dict[str, Any]) -> None:
    nodes = dag.get("nodes") or []
    edges = dag.get("edges") or []
    if not isinstance(nodes, list) or not isinstance(edges, list):
        raise ToolError("dag.nodes and dag.edges must be arrays")
    if len(nodes) > MAX_NODES or len(edges) > MAX_EDGES:
        raise ToolError("workflow exceeds size limits")
    seen: set[str] = set()
    for node in nodes:
        node_id = str(node.get("id") or "")
        node_type = str(node.get("type") or "")
        if not node_id or node_id in seen:
            raise ToolError("workflow nodes need unique ids")
        if node_type not in ALLOWED_TYPES:
            raise ToolError(f"workflow node type is not allowed: {node_type}")
        seen.add(node_id)
    for edge in edges:
        if str(edge.get("from")) not in seen or str(edge.get("to")) not in seen:
            raise ToolError("workflow edge references an unknown node")


def run_workflow(payload: dict[str, Any], ctx: dict[str, Any]) -> dict[str, Any]:
    dag = payload.get("dag")
    if not isinstance(dag, dict):
        raise ToolError("dag is required")
    _validate_dag(dag)
    settings = get_hub_settings()
    headers: dict[str, str] = {}
    # Prefer the gateway when the caller has a bearer token so workflow RBAC
    # applies; otherwise use the internal service token.
    if ctx.get("auth_header"):
        base = rag_settings.gateway_url.rstrip("/")
        headers["authorization"] = str(ctx["auth_header"])
    else:
        base = settings.workflow_url.rstrip("/")
        if settings.workflow_service_token:
            headers["x-service-token"] = settings.workflow_service_token
    for key, header in (("trace_id", "x-trace-id"), ("tenant_id", "x-tenant-id")):
        if ctx.get(key):
            headers[header] = str(ctx[key])
    try:
        resp = httpx.post(
            f"{base}/v1/workflows/run",
            json={"dag": dag, "input": payload.get("input") or {}},
            headers=headers,
            timeout=settings.workflow_timeout_seconds,
        )
        resp.raise_for_status()
        data = resp.json()
    except httpx.HTTPError as exc:
        raise ToolError(f"workflow run failed: {exc}") from exc
    if not isinstance(data, dict):
        raise ToolError("unexpected workflow response")
    return data


WORKFLOW_SPEC = ToolSpec(
    name="workflow.run",
    description="Run a whitelisted NexusFlow DAG. Input: {dag, input?}.",
    input_schema={
        "type": "object",
        "required": ["dag"],
        "properties": {
            "dag": {"type": "object"},
            "input": {"type": "object"},
        },
    },
    risk="execute",
    timeout_ms=15000,
    requires_approval=True,
)


def register_workflow_tools(service: ToolService) -> None:
    service.register(WORKFLOW_SPEC, run_workflow)
