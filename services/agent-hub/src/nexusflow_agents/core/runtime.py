from __future__ import annotations

from functools import lru_cache

from nexusflow_agents.core.bootstrap import Runtime, build_runtime
from nexusflow_agents.core.hub import Hub
from nexusflow_agents.core.lifecycle import AgentLifecycle
from nexusflow_agents.core.registry import AgentRegistry


@lru_cache
def get_runtime() -> Runtime:
    return build_runtime()


def get_hub() -> Hub:
    return get_runtime().hub


def get_registry() -> AgentRegistry:
    return get_runtime().registry


def get_lifecycle() -> AgentLifecycle:
    return get_runtime().lifecycle
