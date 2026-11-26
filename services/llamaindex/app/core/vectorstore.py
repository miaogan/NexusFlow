from functools import lru_cache

from app.core.config import settings


@lru_cache
def get_vector_store(collection: str):
    from llama_index.vector_stores.qdrant import QdrantVectorStore
    from qdrant_client import QdrantClient

    client = QdrantClient(url=settings.qdrant_url)
    return QdrantVectorStore(client=client, collection_name=collection)
