from functools import lru_cache

from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(
        env_prefix="LLAMAINDEX_",
        env_file=".env",
        extra="ignore",
    )

    app_env: str = "dev"
    host: str = "0.0.0.0"
    port: int = 8000

    embedding_model: str = "text-embedding-3-large"
    embedding_api_base: str = "https://api.openai.com/v1"
    embedding_api_key: str = ""

    qdrant_url: str = "http://127.0.0.1:6333"
    collection_prefix: str = "nexusflow"

    rerank_url: str = ""
    rerank_model: str = "bge-reranker-v2-m3"
    rerank_timeout_seconds: float = 2.0

    rrf_k: int = 60
    feedback_path: str = "/tmp/nexusflow-feedback.jsonl"


@lru_cache
def get_settings() -> Settings:
    return Settings()


settings = get_settings()
