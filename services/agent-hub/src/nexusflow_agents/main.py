from __future__ import annotations

import logging
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager

import uvicorn
from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import JSONResponse
from prometheus_client import make_asgi_app

from nexusflow_agents.api.admin import router as admin_router
from nexusflow_agents.core.config import get_hub_settings
from nexusflow_agents.core.runtime import get_hub, get_lifecycle, get_registry

hub = get_hub_settings()


@asynccontextmanager
async def lifespan(_: FastAPI) -> AsyncIterator[None]:
    lifecycle = get_lifecycle()
    lifecycle.start()
    try:
        yield
    finally:
        lifecycle.stop()
        get_hub().shutdown()


app = FastAPI(
    title="NexusFlow Agent Hub",
    version="1.0.0",
    description="Star-topology scheduler for RAG, intent, summary and memory agents.",
    lifespan=lifespan,
)
app.mount("/metrics", make_asgi_app())
app.include_router(admin_router)

if hub.tools_enabled or hub.workflow_enabled:
    from nexusflow_agents.api.call import router as call_router

    app.include_router(call_router)

if hub.hitl_enabled or hub.tools_enabled or hub.workflow_enabled:
    from nexusflow_agents.api.approvals import router as approvals_router

    app.include_router(approvals_router)

if hub.mcp_enabled:
    from nexusflow_agents.mcp.server import router as mcp_router

    app.include_router(mcp_router)

if hub.tools_enabled or hub.workflow_enabled or hub.hitl_enabled:
    from nexusflow_agents.api.audit import router as audit_router

    app.include_router(audit_router)

if hub.role in {"all", "rag"}:
    from nexusflow_agents.agents.rag.api.routes import router as rag_router

    app.include_router(rag_router)

if hub.role in {"all", "intent"}:
    from nexusflow_agents.agents.intent.api.routes import router as intent_router

    app.include_router(intent_router)

if hub.role in {"all", "summary"}:
    from nexusflow_agents.agents.summary.api.routes import router as summary_router

    app.include_router(summary_router)

if hub.role in {"all", "memory"}:
    from nexusflow_agents.agents.memory.api.routes import router as memory_router

    app.include_router(memory_router)


@app.exception_handler(HTTPException)
async def http_exception_handler(_: Request, exc: HTTPException) -> JSONResponse:
    # Keep the gateway/web error shape consistent with the Go services.
    return JSONResponse(status_code=exc.status_code, content={"error": exc.detail})


def health_payload() -> dict[str, object]:
    agents = get_registry().snapshot()
    # A tenant's custom SubAgent failing must not mark the shared hub unhealthy.
    shared = [agent for agent in agents if not agent.get("tenant_id")]
    degraded = not shared or any(not agent["healthy"] for agent in shared)
    return {
        "status": "degraded" if degraded else "ok",
        "service": "agent-hub",
        "role": hub.role,
        "mode": hub.mode,
        "agents": agents,
    }


@app.get("/healthz")
def healthz() -> dict[str, object]:
    return health_payload()


@app.get("/readyz")
def readyz() -> dict[str, object]:
    return {"status": "ok", "role": hub.role, "mode": hub.mode}


if __name__ == "__main__":
    logging.basicConfig(level=logging.INFO)
    uvicorn.run("nexusflow_agents.main:app", host="0.0.0.0", port=hub.port)
