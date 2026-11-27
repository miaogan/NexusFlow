from __future__ import annotations

from functools import lru_cache

from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    """Configuration for the conversation summary agent.

    All keys use the ``SUMMARY_`` prefix, e.g. ``SUMMARY_LLM_BASE_URL``.
    """

    model_config = SettingsConfigDict(
        env_prefix="SUMMARY_",
        env_file=".env",
        env_file_encoding="utf-8",
        extra="ignore",
    )

    app_env: str = "dev"
    log_level: str = "info"
    host: str = "0.0.0.0"
    port: int = 8003

    # OpenAI-compatible endpoint (DeepSeek works with base https://api.deepseek.com/v1).
    # When unset the agent degrades to a deterministic extractive summary.
    llm_base_url: str = ""
    llm_api_key: str = ""
    llm_model: str = "deepseek-chat"
    llm_timeout_seconds: float = 20.0

    # Default output budget; the caller may override per request.
    budget_chars: int = 1200
    # Guardrails on the incoming batch.
    max_message_chars: int = 2000
    max_messages: int = 40

    @property
    def llm_enabled(self) -> bool:
        return bool(self.llm_base_url.strip() and self.llm_api_key.strip())


@lru_cache
def get_settings() -> Settings:
    return Settings()
