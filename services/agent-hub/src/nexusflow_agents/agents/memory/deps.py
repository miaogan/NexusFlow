from __future__ import annotations

import logging
from functools import lru_cache

from nexusflow_agents.agents.memory.core.config import Settings, get_settings
from nexusflow_agents.agents.memory.embedding import HttpEmbedder
from nexusflow_agents.agents.memory.graph import ExtractGraph, RecallGraph
from nexusflow_agents.agents.memory.store import (
    InMemoryMemoryStore,
    MemoryStore,
    PostgresMemoryStore,
    QdrantMemoryStore,
)

logger = logging.getLogger(__name__)


@lru_cache
def get_embedder() -> HttpEmbedder:
    return HttpEmbedder(get_settings())


def build_store(settings: Settings) -> MemoryStore:
    kind = settings.store_kind
    if kind == "memory":
        return InMemoryMemoryStore(settings.dedupe_threshold)

    if not settings.database_url.strip():
        raise ValueError(f"MEMORY_DRIVER={kind} requires MEMORY_DATABASE_URL")

    if kind == "qdrant":
        if not settings.qdrant_url.strip():
            raise ValueError("MEMORY_DRIVER=qdrant requires MEMORY_QDRANT_URL")
        return QdrantMemoryStore(
            settings.database_url,
            settings.qdrant_url,
            collection=settings.qdrant_collection,
            embedding_dim=settings.embedding_dim if settings.embedding_enabled else 0,
            dedupe_threshold=settings.dedupe_threshold,
            max_candidates=settings.max_recall_candidates,
        )

    if not settings.driver.strip():
        logger.warning(
            "MEMORY_DRIVER is empty and MEMORY_QDRANT_URL is not set; "
            "using the legacy pgvector store"
        )
    return PostgresMemoryStore(
        settings.database_url,
        embedding_dim=settings.embedding_dim if settings.embedding_enabled else 0,
        dedupe_threshold=settings.dedupe_threshold,
        max_candidates=settings.max_recall_candidates,
        vector_index=settings.vector_index,
        vector_index_recreate=settings.vector_index_recreate,
        hnsw_m=settings.hnsw_m,
        hnsw_ef_construction=settings.hnsw_ef_construction,
        hnsw_ef_search=settings.hnsw_ef_search,
        ivfflat_lists=settings.ivfflat_lists,
        ivfflat_probes=settings.ivfflat_probes,
    )


@lru_cache
def get_store() -> MemoryStore:
    return build_store(get_settings())


@lru_cache
def get_recall_graph() -> RecallGraph:
    return RecallGraph(get_settings(), get_store(), get_embedder())


@lru_cache
def get_extract_graph() -> ExtractGraph:
    return ExtractGraph(get_settings(), get_store(), get_embedder())
