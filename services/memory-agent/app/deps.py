from __future__ import annotations

from functools import lru_cache

from app.core.config import get_settings
from app.embedding import HttpEmbedder
from app.graph import ExtractGraph, RecallGraph
from app.store import InMemoryMemoryStore, MemoryStore, PostgresMemoryStore


@lru_cache
def get_embedder() -> HttpEmbedder:
    return HttpEmbedder(get_settings())


@lru_cache
def get_store() -> MemoryStore:
    settings = get_settings()
    if settings.database_url:
        return PostgresMemoryStore(
            settings.database_url,
            embedding_dim=settings.embedding_dim if settings.embedding_enabled else 0,
            dedupe_threshold=settings.dedupe_threshold,
            max_candidates=settings.max_recall_candidates,
        )
    return InMemoryMemoryStore(settings.dedupe_threshold)


@lru_cache
def get_recall_graph() -> RecallGraph:
    return RecallGraph(get_settings(), get_store(), get_embedder())


@lru_cache
def get_extract_graph() -> ExtractGraph:
    return ExtractGraph(get_settings(), get_store(), get_embedder())
