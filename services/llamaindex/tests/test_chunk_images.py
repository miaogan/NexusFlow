from __future__ import annotations

import json

from app.services.chunking import build_nodes


def test_build_nodes_attaches_image_urls() -> None:
    markdown = (
        "# Overview\n\n"
        "![architecture](/v1/media?key=default%2Fkb%2Fparsed%2Ft1%2Fimages%2Fa.png)\n\n"
        "Some text about the pipeline.\n\n"
        "## Details\n\nNo figures here.\n"
    )
    nodes = build_nodes(markdown, "markdown", {"task_id": "t1", "kb_id": "kb"})

    with_images = [node for node in nodes if "image_urls" in node.metadata]
    assert len(with_images) == 1
    assert json.loads(with_images[0].metadata["image_urls"]) == [
        "/v1/media?key=default%2Fkb%2Fparsed%2Ft1%2Fimages%2Fa.png"
    ]


def test_build_nodes_without_images_has_no_metadata_key() -> None:
    nodes = build_nodes("# Plain\n\ntext\n", "markdown", {"task_id": "t2"})
    assert nodes
    assert all("image_urls" not in node.metadata for node in nodes)
