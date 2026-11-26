from __future__ import annotations

from fastapi import FastAPI
from fastapi.responses import Response
from prometheus_client import CONTENT_TYPE_LATEST, generate_latest

from app.api.routes import router
from app.core.config import get_settings
from app.schemas import HealthResponse
from app.vector_index import normalize_index_type

settings = get_settings()

app = FastAPI(title="NexusFlow Memory Agent", version="0.1.0")
app.include_router(router)


@app.get("/healthz", response_model=HealthResponse)
def healthz() -> HealthResponse:
    return HealthResponse(
        status="ok",
        llm_enabled=settings.llm_enabled,
        embedding_enabled=settings.embedding_enabled,
        store="postgres" if settings.database_url else "memory",
        vector_index=(
            normalize_index_type(settings.vector_index)
            if settings.embedding_enabled
            else "none"
        ),
    )


@app.get("/metrics")
def metrics() -> Response:
    return Response(generate_latest(), media_type=CONTENT_TYPE_LATEST)
