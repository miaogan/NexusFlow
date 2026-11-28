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
    # DashScope-compatible embeddings may cap batch size; keep it configurable.
    embedding_batch_size: int = 16

    qdrant_url: str = "http://127.0.0.1:6333"
    collection_prefix: str = "nexusflow"
    # Qdrant sparse recall. Disable to fall back to the legacy scroll + BM25
    # implementation (useful when the companion sparse collection is absent).
    sparse_enabled: bool = True

    rerank_url: str = ""
    rerank_model: str = "bge-reranker-v2-m3"
    rerank_timeout_seconds: float = 2.0
    # generic (bge/Cohere style) | dashscope (Aliyun text-rerank).
    rerank_protocol: str = "generic"
    rerank_api_key: str = ""
    # Candidate pool sent to the reranker; 0 keeps every fused candidate.
    rerank_pool: int = 0

    rrf_k: int = 60
    feedback_path: str = "/tmp/nexusflow-feedback.jsonl"

    # LightRAG-style graph route. With an LLM configured, entities/relations are
    # extracted by the model; otherwise (or on failure) the route degrades to a
    # co-occurrence word graph built from the same chunks.
    graph_cache_seconds: float = 60.0
    graph_llm_timeout_seconds: float = 20.0
    graph_max_llm_chunks: int = 20
    # Persistent graph snapshots shared by replicas. memory keeps the process
    # cache only; postgres stores snapshots in rag_graph_snapshots.
    graph_store_driver: str = "memory"
    graph_database_url: str = ""
    # Real LightRAG integration (lightrag-hku >= 1.5.8). Disabled by default;
    # needs an OpenAI-compatible LLM. file keeps JSON/NanoVector/NetworkX under
    # working_dir; postgres uses PGKV/PGVector/PGDocStatus plus the
    # PostgreSQL-native PGTableGraphStorage (no Apache AGE required; set
    # lightrag_graph_storage=PGGraphStorage only when AGE is installed).
    lightrag_enabled: bool = False
    lightrag_storage_driver: str = "file"
    lightrag_graph_storage: str = ""
    lightrag_working_dir: str = "/tmp/nexusflow-lightrag"
    lightrag_query_mode: str = "hybrid"
    lightrag_embedding_dim: int = 1536
    lightrag_top_k: int = 40
    lightrag_query_timeout_seconds: float = 20.0
    lightrag_insert_timeout_seconds: float = 300.0
    llm_base_url: str = ""
    llm_api_key: str = ""
    llm_model: str = "gpt-4o-mini"
    llm_timeout_seconds: float = 120.0
    embedding_timeout_seconds: float = 30.0


@lru_cache
def get_settings() -> Settings:
    return Settings()


settings = get_settings()
