from __future__ import annotations

from functools import lru_cache

from app.core.config import get_settings
from app.graph import IntentGraph


@lru_cache
def get_graph() -> IntentGraph:
    return IntentGraph(get_settings())
