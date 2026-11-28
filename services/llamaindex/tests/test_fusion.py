from app.retrieval import pipeline
from app.retrieval.fusion import Chunk, rrf_fuse
from app.schemas import RecallRoute, SearchRequest


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


def test_search_returns_empty_when_routes_succeed(monkeypatch):
    import asyncio

    monkeypatch.setattr(pipeline, "retrieve", lambda route, query, filters: [])
    response = asyncio.run(
        pipeline.search(SearchRequest(query="anything", routes=[RecallRoute(type="vector")]))
    )
    assert response.chunks == []
    assert response.recall_count == 0


def test_search_raises_when_every_route_fails(monkeypatch):
    import asyncio

    import pytest

    def boom(route, query, filters):
        raise RuntimeError("qdrant down")

    monkeypatch.setattr(pipeline, "retrieve", boom)
    with pytest.raises(pipeline.RecallUnavailable):
        asyncio.run(
            pipeline.search(SearchRequest(query="anything", routes=[RecallRoute(type="vector")]))
        )


def test_recall_returns_empty_for_a_missing_collection(monkeypatch):
    from app.retrieval import recall

    class MissingCollection:
        def collection_exists(self, name):
            return False

    monkeypatch.setattr(recall, "_qdrant_client", lambda collection: MissingCollection())
    filters = {"kb_id": "brand-new-kb", "tenant_id": "t1"}
    assert recall._vector_recall("anything", 5, filters) == []
    assert recall._scroll_documents(filters) == []
