from __future__ import annotations

from functools import lru_cache

from nexusflow_agents.agents.summary.core.config import get_settings
from nexusflow_agents.agents.summary.graph import SummaryGraph


@lru_cache
def get_graph() -> SummaryGraph:
    return SummaryGraph(get_settings())
