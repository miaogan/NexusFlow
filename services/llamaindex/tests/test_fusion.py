from app.retrieval.fusion import Chunk, rrf_fuse


def test_rrf_prefers_documents_in_multiple_lists():
    fused = rrf_fuse(
        [
            [Chunk("a", "A"), Chunk("b", "B")],
            [Chunk("b", "B"), Chunk("c", "C")],
        ],
        k=60,
    )
    assert [c.chunk_id for c in fused] == ["b", "a", "c"]


def test_rrf_dedupes_within_a_list():
    fused = rrf_fuse([[Chunk("a", "A"), Chunk("a", "A")]])
    assert len(fused) == 1


def test_rrf_scores_are_monotonic():
    fused = rrf_fuse([[Chunk("a", "A"), Chunk("b", "B"), Chunk("c", "C")]])
    scores = [c.score for c in fused]
    assert scores == sorted(scores, reverse=True)
