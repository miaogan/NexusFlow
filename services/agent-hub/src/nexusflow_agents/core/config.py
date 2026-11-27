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
    # Custom SubAgent registry: memory for dev, postgres for durable multi-replica.
    registry_driver: Literal["memory", "postgres"] = "memory"
    registry_database_url: str = ""
    # Durable HITL approvals and compliance audit trail. Postgres keeps
    # decisions shared across replicas; state_database_url is required for it.
    approval_driver: Literal["memory", "postgres"] = "memory"
    audit_driver: Literal["memory", "postgres"] = "memory"
    state_database_url: str = ""
    background_workers: int = 8
    # P0/P2–P5 capability switches. All default off for safe rollout.
    tools_enabled: bool = False
    reasoning_enabled: bool = False
    mcp_enabled: bool = False
    hitl_enabled: bool = False
    workflow_enabled: bool = False
    workflow_url: str = "http://127.0.0.1:8081"
    workflow_service_token: str = ""
    workflow_timeout_seconds: float = 15.0
    reasoning_max_turns: int = 6
    reasoning_timeout_seconds: float = 60.0
    reasoning_tool_budget: int = 12
    reasoning_token_budget: int = 20000
    mcp_servers: str = ""
    mcp_timeout_seconds: float = 10.0
    mcp_max_bytes: int = 1 << 20
    mcp_allow_sandbox: bool = False


@lru_cache
def get_hub_settings() -> HubSettings:
    return HubSettings()
