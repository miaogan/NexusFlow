from __future__ import annotations

from functools import lru_cache

from nexusflow_agents.agents.intent.core.config import get_settings
from nexusflow_agents.agents.intent.graph import IntentGraph


@lru_cache
def get_graph() -> IntentGraph:
    return IntentGraph(get_settings())
