from app.services.embedding import build_embedding


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
    for node in nodes:
        node.metadata.update(metadata)
        node.metadata.setdefault("tenant_id", metadata.get("tenant_id", "default"))
    return nodes
