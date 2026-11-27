from __future__ import annotations

from nexusflow_agents.agents.rag.services.llm import chunk_images


def test_chunk_images_parses_json_and_caps_at_three() -> None:
    chunk = {
        "meta": {
            "image_urls": '["/v1/media?key=a", "/v1/media?key=b", '
            '"/v1/media?key=c", "/v1/media?key=d"]'
        }
    }
    assert chunk_images(chunk) == [
        "/v1/media?key=a",
        "/v1/media?key=b",
        "/v1/media?key=c",
    ]


def test_chunk_images_accepts_legacy_csv_and_ignores_missing() -> None:
    assert chunk_images({"meta": {"image_urls": "/v1/media?key=x"}}) == [
        "/v1/media?key=x"
    ]
    assert chunk_images({"meta": {}}) == []
    assert chunk_images({}) == []
