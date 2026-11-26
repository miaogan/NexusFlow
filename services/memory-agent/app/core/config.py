from __future__ import annotations

from functools import lru_cache

from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    """Configuration for the long-term memory agent.

    All keys use the ``MEMORY_`` prefix, e.g. ``MEMORY_LLM_BASE_URL``.
    """

    model_config = SettingsConfigDict(
        env_prefix="MEMORY_",
        env_file=".env",
        env_file_encoding="utf-8",
        extra="ignore",
    )

    app_env: str = "dev"
    log_level: str = "info"
    host: str = "0.0.0.0"
    port: int = 8004

    # OpenAI-compatible endpoint for extraction; empty -> rule-based extraction.
    llm_base_url: str = ""
    llm_api_key: str = ""
    llm_model: str = "deepseek-chat"
    llm_timeout_seconds: float = 20.0

    # Optional OpenAI-compatible embeddings for semantic recall / dedupe.
    embedding_base_url: str = ""
    embedding_api_key: str = ""
    embedding_model: str = "text-embedding-3-small"
    embedding_dim: int = 1536
    embedding_timeout_seconds: float = 15.0

    # Empty -> in-memory memories (dev). Set to a Postgres DSN to persist with
    # pgvector when ``embedding_*`` is configured.
    database_url: str = ""

    # pgvector index tuning (effective when embeddings are enabled).
    # hnsw: better recall at higher build cost; ivfflat: cheaper, needs data
    # first; none: exact scan, fine for small per-user datasets.
    vector_index: str = "hnsw"
    # Drop + recreate the index so changed parameters take effect.
    vector_index_recreate: bool = False
    hnsw_m: int = 16
    hnsw_ef_construction: int = 64
    hnsw_ef_search: int = 40
    ivfflat_lists: int = 100
    ivfflat_probes: int = 10

    top_k: int = 3
    max_content_chars: int = 300
    min_content_chars: int = 4
    max_messages: int = 8
    max_message_chars: int = 2000
    # Candidates older/newer than this are scored lexically in Python.
    max_recall_candidates: int = 300
    dedupe_threshold: float = 0.85

    @property
    def llm_enabled(self) -> bool:
        return bool(self.llm_base_url.strip() and self.llm_api_key.strip())

    @property
    def embedding_enabled(self) -> bool:
        return bool(
            self.embedding_base_url.strip() and self.embedding_api_key.strip()
        )


@lru_cache
def get_settings() -> Settings:
    return Settings()
