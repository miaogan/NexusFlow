import uvicorn
from fastapi import FastAPI
from prometheus_client import make_asgi_app

from app.api.routes import router
from app.core.config import settings

app = FastAPI(
    title="NexusFlow LlamaIndex Service",
    version="4.0.0",
    description="Chunking, indexing, multi-route recall and rerank for NexusFlow.",
)
app.include_router(router)
app.mount("/metrics", make_asgi_app())


@app.get("/healthz")
def healthz() -> dict[str, str]:
    return {"status": "ok", "service": "llamaindex"}


if __name__ == "__main__":
    uvicorn.run("app.main:app", host=settings.host, port=settings.port)
