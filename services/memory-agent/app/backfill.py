"""One-time backfill of legacy pgvector memories into Qdrant.

Run inside the memory-agent environment:

    python -m app.backfill --batch-size 500
"""

from __future__ import annotations

import argparse

from app.core.config import get_settings
from app.deps import get_store
from app.store import QdrantMemoryStore


def main() -> int:
    parser = argparse.ArgumentParser(description="Backfill memory vectors into Qdrant")
    parser.add_argument("--batch-size", type=int, default=500)
    args = parser.parse_args()

    settings = get_settings()
    if settings.store_kind != "qdrant":
        raise SystemExit("MEMORY_DRIVER must resolve to qdrant for backfill")

    store = get_store()
    if not isinstance(store, QdrantMemoryStore):
        raise SystemExit("store is not a QdrantMemoryStore")
    if not store.ready:
        raise SystemExit("qdrant memory store is not ready; check MEMORY_QDRANT_URL")

    total = store.backfill(max(1, args.batch_size))
    print(f"backfilled {total} memory vectors")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
