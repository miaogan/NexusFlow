import logging

import uvicorn
from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import JSONResponse
from prometheus_client import make_asgi_app

from nexusflow_agents.agents.rag.api.routes import router
from nexusflow_agents.agents.rag.core.config import settings

app = FastAPI(
    title="NexusFlow RAG Agent",
    version="1.0.0",
    description="LangGraph RAG chat service linking user identity and retrieval.",
)
app.include_router(router)
app.mount("/metrics", make_asgi_app())


@app.exception_handler(HTTPException)
async def http_exception_handler(_: Request, exc: HTTPException) -> JSONResponse:
    # The web console reads {"error": ...}; keep the error shape consistent
    # with the Go gateway instead of FastAPI's default {"detail": ...}.
    return JSONResponse(status_code=exc.status_code, content={"error": exc.detail})


@app.get("/healthz")
def healthz() -> dict[str, object]:
    return {
        "status": "ok",
        "service": "rag-agent",
        "llm": settings.llm_enabled,
        "store": "postgres" if settings.database_url else "memory",
    }


if __name__ == "__main__":
    logging.basicConfig(level=logging.INFO)
    uvicorn.run("nexusflow_agents.agents.rag.main:app", host=settings.host, port=settings.port)
