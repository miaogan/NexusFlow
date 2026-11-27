from __future__ import annotations

from functools import lru_cache
from typing import Literal

from pydantic_settings import BaseSettings, SettingsConfigDict

VALID_ROLES = {"all", "rag", "intent", "summary", "memory"}
VALID_MODES = {"local", "remote", "hybrid"}


class HubSettings(BaseSettings):
    """Settings for the star-topology scheduler around the local agents."""

    model_config = SettingsConfigDict(
        env_prefix="AGENT_HUB_",
        env_file=".env",
        env_file_encoding="utf-8",
        extra="ignore",
    )

    role: Literal["all", "rag", "intent", "summary", "memory"] = "all"
    mode: Literal["local", "remote", "hybrid"] = "local"
    prefer: Literal["local", "remote"] = "local"
    port: int = 8001
    health_interval_seconds: float = 10.0
    health_timeout_seconds: float = 2.0
    registry_ttl_seconds: int = 30
    fail_threshold: int = 3
    max_attempts: int = 2
    dynamic_registration: bool = False
    admin_token: str = ""
    background_workers: int = 8


@lru_cache
def get_hub_settings() -> HubSettings:
    return HubSettings()
