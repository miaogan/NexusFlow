from __future__ import annotations

INDEX_NAME = "rag_memories_embedding_idx"
VALID_INDEX_TYPES = {"hnsw", "ivfflat", "none"}


def normalize_index_type(value: str) -> str:
    index_type = (value or "").strip().lower()
    return index_type if index_type in VALID_INDEX_TYPES else "hnsw"


def _clamp(value: int, low: int, high: int) -> int:
    return max(low, min(int(value), high))


def index_statements(
    *,
    index_type: str,
    dim: int,
    m: int,
    ef_construction: int,
    lists: int,
    recreate: bool = False,
) -> list[str]:
    """Builds pgvector index DDL.

    HNSW trades build time / memory for recall (``m``, ``ef_construction``);
    IVFFlat trades probe count for recall (``lists``). ``recreate`` drops the
    index first so changed parameters take effect (CREATE INDEX IF NOT EXISTS
    would otherwise keep the old definition).
    """
    kind = normalize_index_type(index_type)
    if not dim or kind == "none":
        return []

    statements: list[str] = []
    if recreate:
        statements.append(f"DROP INDEX IF EXISTS {INDEX_NAME}")

    if kind == "ivfflat":
        lists = _clamp(lists, 1, 10000)
        statements.append(
            f"CREATE INDEX IF NOT EXISTS {INDEX_NAME} "
            "ON rag_memories USING ivfflat (embedding vector_cosine_ops) "
            f"WITH (lists = {lists})"
        )
    else:
        m = _clamp(m, 4, 64)
        ef_construction = _clamp(ef_construction, 16, 512)
        statements.append(
            f"CREATE INDEX IF NOT EXISTS {INDEX_NAME} "
            "ON rag_memories USING hnsw (embedding vector_cosine_ops) "
            f"WITH (m = {m}, ef_construction = {ef_construction})"
        )
    return statements


def runtime_settings(
    *, index_type: str, ef_search: int, probes: int
) -> dict[str, str]:
    """Per-connection search parameters for pgvector.

    These are the knobs to tune after go-live: raising them improves recall at
    the cost of latency. Applied with ``set_config`` on each query connection.
    """
    kind = normalize_index_type(index_type)
    if kind == "hnsw":
        return {"hnsw.ef_search": str(_clamp(ef_search, 1, 1000))}
    if kind == "ivfflat":
        return {"ivfflat.probes": str(_clamp(probes, 1, 1000))}
    return {}
