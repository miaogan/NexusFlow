from app.core.config import settings


def build_embedding(model: str | None = None):
    from llama_index.embeddings.openai import OpenAIEmbedding

    if not settings.embedding_api_key:
        raise RuntimeError("embedding api key is not configured")

    return OpenAIEmbedding(
        model=model or settings.embedding_model,
        api_key=settings.embedding_api_key,
        api_base=settings.embedding_api_base,
        embed_batch_size=settings.embedding_batch_size,
    )
