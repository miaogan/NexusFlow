from __future__ import annotations

from functools import lru_cache

from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(
        env_prefix="INTENT_",
        env_file=".env",
        env_file_encoding="utf-8",
        extra="ignore",
    )

    app_env: str = "dev"
    log_level: str = "info"
    host: str = "0.0.0.0"
    port: int = 8002

    # OpenAI-compatible endpoint (DeepSeek works with base https://api.deepseek.com/v1).
    llm_base_url: str = ""
    llm_api_key: str = ""
    llm_model: str = "deepseek-chat"
    llm_timeout_seconds: float = 20.0

    max_history_messages: int = 8
    max_message_chars: int = 4000

    @property
    def llm_enabled(self) -> bool:
        return bool(self.llm_base_url.strip() and self.llm_api_key.strip())


@lru_cache
def get_settings() -> Settings:
    return Settings()
