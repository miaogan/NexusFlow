import hashlib
import uuid

from app.services.embedding import build_embedding


def stable_node_id(task_id: str, index: int, content: str) -> str:
    """Deterministic Qdrant-compatible id for one chunk of a document.

    Re-indexing the same content yields the same id, so upserts overwrite the
    previous point instead of accumulating duplicates. The id stays a UUID
    because Qdrant only accepts UUID or unsigned integer point ids.
    """
    digest = hashlib.sha256(content.encode("utf-8")).hexdigest()[:16]
    return str(uuid.uuid5(uuid.NAMESPACE_URL, f"{task_id}:{index}:{digest}"))


def build_nodes(markdown: str, strategy: str, metadata: dict[str, str]):
    """Chunk structured Markdown with LlamaIndex node parsers."""
    from llama_index.core import Document
    from llama_index.core.node_parser import MarkdownNodeParser, SentenceSplitter

    document = Document(text=markdown, metadata=dict(metadata))

    if strategy == "semantic":
        from llama_index.core.node_parser import SemanticSplitterNodeParser

        parser = SemanticSplitterNodeParser(
            buffer_size=1,
            breakpoint_percentile_threshold=95,
            embed_model=build_embedding(None),
        )
    elif strategy == "fixed":
        parser = SentenceSplitter(chunk_size=512, chunk_overlap=64)
    else:
        parser = MarkdownNodeParser()

    nodes = parser.get_nodes_from_documents([document])
    task_id = str(metadata.get("task_id", "")).strip()
    for index, node in enumerate(nodes):
        node.metadata.update(metadata)
        node.metadata.setdefault("tenant_id", metadata.get("tenant_id", "default"))
        if task_id:
            node.id_ = stable_node_id(task_id, index, node.get_content())
    return nodes
