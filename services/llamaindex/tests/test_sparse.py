from __future__ import annotations

from app.services.sparse import (
    encode_sparse,
    sparse_collection_name,
    term_index,
    tokenize,
)


def test_tokenize_mixed_ascii_and_cjk() -> None:
    tokens = tokenize("NexusFlow 支持 Dense+Sparse 混合")

    assert "nexusflow" in tokens
    assert "dense" in tokens and "sparse" in tokens
    assert "支持" in tokens
    assert "混合" in tokens


def test_encode_sparse_is_deterministic_and_aggregates_terms() -> None:
    first = encode_sparse("dense dense sparse")
    second = encode_sparse("dense dense sparse")

    assert first == second
    indices, values = first
    assert len(indices) == 2
    assert sum(values) == 3
    assert indices == sorted(indices)


def test_encode_sparse_without_tokens_returns_empty() -> None:
    assert encode_sparse("   ") == ([], [])


def test_term_index_is_stable_uint32() -> None:
    value = term_index("知识库")

    assert 0 <= value < 2**32
    assert value == term_index("知识库")


def test_sparse_collection_name_is_companion() -> None:
    assert sparse_collection_name("nexusflow_default") == "nexusflow_default__sparse"
