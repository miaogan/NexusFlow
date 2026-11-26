from app.core.config import settings
from app.retrieval.graph import clear_graph_cache
from app.schemas import IndexRequest, IndexResponse
from app.services.chunking import build_nodes
from app.services.embedding import build_embedding


def index_markdown(request: IndexRequest) -> IndexResponse:
    """Chunk, embed and persist a parsed document into the vector store."""
    collection = f"{settings.collection_prefix}_{request.metadata.get('kb_id', 'default')}"
    nodes = build_nodes(request.markdown, request.split_strategy, request.metadata)
    if not nodes:
        return IndexResponse(success=False, error_msg="no nodes produced", collection=collection)

    from llama_index.core import StorageContext, VectorStoreIndex

    from app.core.vectorstore import get_vector_store

    vector_store = get_vector_store(collection)
    storage_context = StorageContext.from_defaults(vector_store=vector_store)
    VectorStoreIndex(
        nodes,
        storage_context=storage_context,
        embed_model=build_embedding(request.embedding_model),
    )
    # New chunks must appear in the graph route immediately, not after the TTL.
    clear_graph_cache(collection)

    return IndexResponse(
        success=True,
        chunk_count=len(nodes),
        indexed_count=len(nodes),
        collection=collection,
    )
