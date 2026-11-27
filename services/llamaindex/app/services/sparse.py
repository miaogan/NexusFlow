"""Deterministic sparse vectors for Qdrant lexical recall.

The encoder is deliberately dependency-free: ASCII words are kept as-is and
CJK runs are expanded into character bigrams, which works without a Chinese
segmentation model. Token frequencies are hashed into uint32 indices, making
the output stable across processes so re-indexing overwrites the same sparse
point instead of accumulating duplicates.
"""

from __future__ import annotations

import hashlib
import re
from collections import Counter

_WORD = re.compile(r"[a-z0-9_]+")
_CJK = re.compile(r"[\u4e00-\u9fff]+")

SPARSE_COLLECTION_SUFFIX = "__sparse"
SPARSE_VECTOR_NAME = "sparse"


def tokenize(text: str) -> list[str]:
    lowered = text.lower()
    tokens = _WORD.findall(lowered)
    for run in _CJK.findall(lowered):
        if len(run) == 1:
            tokens.append(run)
            continue
        tokens.extend(run[index : index + 2] for index in range(len(run) - 1))
    return tokens


def term_index(token: str) -> int:
    """Stable 32-bit sparse index for a token (Qdrant requires uint32)."""
    digest = hashlib.blake2b(token.encode("utf-8"), digest_size=4).digest()
    return int.from_bytes(digest, "big")


def encode_sparse(text: str) -> tuple[list[int], list[float]]:
    """Return (indices, values) with term frequencies and merged collisions."""
    counts = Counter(tokenize(text))
    if not counts:
        return [], []

    merged: dict[int, float] = {}
    for token, count in counts.items():
        index = term_index(token)
        merged[index] = merged.get(index, 0.0) + float(count)

    indices = sorted(merged)
    return indices, [merged[index] for index in indices]


def sparse_collection_name(collection: str) -> str:
    """Companion collection that holds sparse vectors for one KB collection."""
    return f"{collection}{SPARSE_COLLECTION_SUFFIX}"
