from __future__ import annotations

from fastapi import FastAPI
from fastapi.responses import Response
from prometheus_client import CONTENT_TYPE_LATEST, generate_latest

from nexusflow_agents.agents.summary.api.routes import router
from nexusflow_agents.agents.summary.core.config import get_settings
from nexusflow_agents.agents.summary.schemas import HealthResponse

settings = get_settings()

app = FastAPI(title="NexusFlow Summary Agent", version="0.1.0")
app.include_router(router)


@app.get("/healthz", response_model=HealthResponse)
def healthz() -> HealthResponse:
    return HealthResponse(status="ok", llm_enabled=settings.llm_enabled)


@app.get("/metrics")
def metrics() -> Response:
    return Response(generate_latest(), media_type=CONTENT_TYPE_LATEST)
