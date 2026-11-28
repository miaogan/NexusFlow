from functools import lru_cache

from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    """Configuration for the LangGraph RAG chat service.

    All keys use the ``RAG_`` prefix, e.g. ``RAG_GATEWAY_URL``.
    """

    model_config = SettingsConfigDict(
        env_prefix="RAG_",
        env_file=".env",
        extra="ignore",
    )

    app_env: str = "dev"
    host: str = "0.0.0.0"
    port: int = 8001

    # Gateway used for retrieval (/v1/search) so chat reuses the unified
    # multi-route recall + rerank pipeline and its tracing.
    gateway_url: str = "http://127.0.0.1:8080"
    search_timeout_seconds: float = 15.0
    default_top_k: int = 5
    max_top_k: int = 50

    # Independent intent agent (LangGraph supervisor step). Empty or
    # unreachable -> fall back to knowledge QA so chat keeps working.
    intent_url: str = "http://127.0.0.1:8002"
    intent_timeout_seconds: float = 5.0

    # OpenAI-compatible chat model. Both base URL and key are required; when
    # either is missing the service falls back to an extractive answer.
    llm_base_url: str = ""
    llm_api_key: str = ""
    llm_model: str = "gpt-4o-mini"
    llm_timeout_seconds: float = 60.0
    max_context_chars: int = 6000

    # Independent summary agent for context compaction. Empty -> no
    # compaction (older turns simply fall out of the window).
    summary_url: str = "http://127.0.0.1:8003"
    summary_timeout_seconds: float = 10.0
    summary_budget_chars: int = 1200
    # Messages kept verbatim after older turns are folded into the summary.
    summary_recent_messages: int = 6

    # Independent memory agent for long-term recall / extraction. Empty or
    # unreachable -> chat works without cross-session memory.
    memory_url: str = "http://127.0.0.1:8004"
    memory_timeout_seconds: float = 5.0
    memory_top_k: int = 3

    # Conversation persistence. Empty -> in-memory (dev); set to a Postgres DSN
    # to persist conversations and messages.
    database_url: str = ""
    max_history: int = 12

    # PII shielding at the LLM boundary. Inputs (prompt, history, chunks,
    # summary, memories, corrections) are masked with stable tokens; replies
    # stay masked unless pii_restore_output restores the originals.
    pii_enabled: bool = False
    pii_restore_output: bool = False

    # Historical human corrections matched before retrieval (gateway
    # /v1/corrected-answers). Lookup failure -> chat proceeds without them.
    corrected_timeout_seconds: float = 5.0
    corrected_top_k: int = 3

    @property
    def llm_enabled(self) -> bool:
        return bool(self.llm_api_key and self.llm_base_url)


@lru_cache
def get_settings() -> Settings:
    return Settings()


settings = get_settings()
