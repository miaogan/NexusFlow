"""Real LightRAG integration (``lightrag-hku``).

The graph route can run on the LightRAG library instead of the in-house
LightRAG-style graph. LightRAG handles entity/relation extraction, chunk-level
graph storage and local/global/hybrid retrieval; this module only adapts it to
NexusFlow's ``Chunk`` contract and scopes every instance by tenant + KB.

Enabled only when ``LLAMAINDEX_LIGHTRAG_ENABLED=true`` and an
OpenAI-compatible LLM is configured. Any failure degrades to the in-house graph
so the retrieval route keeps serving.
"""
from __future__ import annotations

import asyncio
import hashlib
import logging
import os
import re
import threading
from typing import Any

import httpx

from app.core.config import settings
from app.retrieval.fusion import Chunk

logger = logging.getLogger(__name__)

_SCOPE = re.compile(r"[^a-zA-Z0-9_.-]+")
_scope_locks: dict[str, threading.Lock] = {}
_scope_locks_guard = threading.Lock()


def enabled() -> bool:
    return bool(
        settings.lightrag_enabled
        and settings.llm_base_url.strip()
        and settings.llm_api_key.strip()
    )


def workspace_for(tenant_id: str, kb_id: str) -> str:
    raw = f"{tenant_id or 'default'}__{kb_id or 'default'}"
    clean = _SCOPE.sub("_", raw).strip("_")
    return clean[:128] or "default"


def _working_dir(workspace: str) -> str:
    path = os.path.join(settings.lightrag_working_dir, workspace)
    os.makedirs(path, exist_ok=True)
    return path


def _scope_lock(workspace: str) -> threading.Lock:
    with _scope_locks_guard:
        lock = _scope_locks.get(workspace)
        if lock is None:
            lock = threading.Lock()
            _scope_locks[workspace] = lock
        return lock


async def _complete(
    prompt: str,
    system_prompt: str | None = None,
    history_messages: list[dict[str, Any]] | None = None,
    **_: Any,
) -> str:
    messages: list[dict[str, str]] = []
    if system_prompt:
        messages.append({"role": "system", "content": system_prompt})
    for item in history_messages or []:
        role = str(item.get("role") or "user")
        messages.append({"role": role, "content": str(item.get("content") or "")})
    messages.append({"role": "user", "content": prompt})
    async with httpx.AsyncClient(timeout=settings.llm_timeout_seconds) as client:
        resp = await client.post(
            f"{settings.llm_base_url.rstrip('/')}/chat/completions",
            headers={"Authorization": f"Bearer {settings.llm_api_key}"},
            json={"model": settings.llm_model, "temperature": 0, "messages": messages},
        )
        resp.raise_for_status()
        data = resp.json()
    return str(data["choices"][0]["message"]["content"] or "")


async def _embed(texts: list[str]) -> Any:
    import numpy as np

    if not texts:
        return np.zeros((0, settings.lightrag_embedding_dim), dtype=np.float32)
    async with httpx.AsyncClient(timeout=settings.embedding_timeout_seconds) as client:
        resp = await client.post(
            f"{settings.embedding_api_base.rstrip('/')}/embeddings",
            headers={"Authorization": f"Bearer {settings.embedding_api_key}"},
            json={"model": settings.embedding_model, "input": texts},
        )
        resp.raise_for_status()
        data = resp.json()
    items = sorted(data.get("data") or [], key=lambda item: int(item.get("index", 0)))
    vectors = [[float(value) for value in item.get("embedding") or []] for item in items]
    return np.asarray(vectors, dtype=np.float32)


def _embedding_func() -> Any:
    from lightrag.utils import EmbeddingFunc

    return EmbeddingFunc(
        embedding_dim=settings.lightrag_embedding_dim,
        max_token_size=8192,
        func=_embed,
        model_name=settings.embedding_model,
    )


def _storage_names() -> tuple[str, str, str, str]:
    if settings.lightrag_storage_driver.strip().lower() == "postgres":
        # PGTableGraphStorage keeps the graph in plain Postgres tables (no
        # Apache AGE); PGGraphStorage remains available when AGE is installed.
        graph = settings.lightrag_graph_storage.strip() or "PGTableGraphStorage"
        return "PGKVStorage", "PGVectorStorage", graph, "PGDocStatusStorage"
    return "JsonKVStorage", "NanoVectorDBStorage", "NetworkXStorage", "JsonDocStatusStorage"


def _build_rag(workspace: str) -> Any:
    """Builds one LightRAG instance for a tenant + KB workspace."""
    from lightrag import LightRAG

    kv, vector, graph, doc_status = _storage_names()
    return LightRAG(
        working_dir=_working_dir(workspace),
        workspace=workspace,
        kv_storage=kv,
        vector_storage=vector,
        graph_storage=graph,
        doc_status_storage=doc_status,
        llm_model_func=_complete,
        llm_model_name=settings.llm_model,
        llm_model_max_async=2,
        embedding_func=_embedding_func(),
        embedding_batch_num=max(1, settings.embedding_batch_size),
        top_k=max(1, settings.lightrag_top_k),
        chunk_top_k=max(1, settings.lightrag_top_k),
    )


def _chunk_id(item: dict[str, Any], index: int) -> str:
    raw = str(item.get("chunk_id") or item.get("reference_id") or "")
    if raw:
        return raw
    digest = hashlib.sha1(
        str(item.get("content") or index).encode("utf-8")
    ).hexdigest()[:16]
    return f"lightrag-{digest}"


def _to_chunks(data: dict[str, Any], limit: int) -> list[Chunk]:
    section = data.get("data") if isinstance(data, dict) else None
    items = (section or {}).get("chunks") or []
    out: list[Chunk] = []
    for index, item in enumerate(items):
        content = str(item.get("content") or "").strip()
        if not content:
            continue
        out.append(
            Chunk(
                chunk_id=_chunk_id(item, index),
                content=content,
                score=max(0.0, 1.0 - index * 0.01),
                meta={
                    "file_path": str(item.get("file_path") or ""),
                    "retriever": "lightrag",
                },
                source="graph",
            )
        )
        if len(out) >= limit:
            break
    return out


async def _query_async(
    workspace: str, query: str, mode: str, top_k: int
) -> list[Chunk]:
    from lightrag import QueryParam

    rag = _build_rag(workspace)
    await rag.initialize_storages()
    try:
        data = await rag.aquery_data(
            query,
            param=QueryParam(mode=mode, top_k=top_k, chunk_top_k=top_k),
        )
    finally:
        await rag.finalize_storages()
    return _to_chunks(dict(data or {}), top_k)


def query(
    tenant_id: str,
    kb_id: str,
    query: str,
    top_k: int,
    mode: str = "",
) -> list[Chunk]:
    """Returns LightRAG chunk evidence, or ``[]`` when disabled/unavailable."""
    if not enabled() or not query.strip():
        return []
    workspace = workspace_for(tenant_id, kb_id)
    try:
        return asyncio.run(
            asyncio.wait_for(
                _query_async(
                    workspace, query, mode or settings.lightrag_query_mode, top_k
                ),
                timeout=settings.lightrag_query_timeout_seconds,
            )
        )
    except Exception as exc:  # noqa: BLE001 - fall back to the in-house graph
        logger.warning("lightrag query failed for %s: %s", workspace, exc)
        return []


async def _insert_async(
    workspace: str, doc_id: str, file_name: str, markdown: str
) -> None:
    rag = _build_rag(workspace)
    await rag.initialize_storages()
    try:
        # Re-indexing reuses the task id; delete first so stale entities and
        # chunks are replaced instead of being skipped as duplicates.
        await rag.adelete_by_doc_id(doc_id)
        await rag.ainsert(markdown, ids=doc_id, file_paths=file_name or doc_id)
    finally:
        await rag.finalize_storages()


def _insert_worker(
    workspace: str, doc_id: str, file_name: str, markdown: str
) -> None:
    lock = _scope_lock(workspace)
    with lock:
        try:
            asyncio.run(
                asyncio.wait_for(
                    _insert_async(workspace, doc_id, file_name, markdown),
                    timeout=settings.lightrag_insert_timeout_seconds,
                )
            )
            logger.info("lightrag indexed doc %s in %s", doc_id, workspace)
        except Exception as exc:  # noqa: BLE001 - indexing must keep working
            logger.warning("lightrag index failed for %s/%s: %s", workspace, doc_id, exc)


def insert_document(
    tenant_id: str,
    kb_id: str,
    doc_id: str,
    file_name: str,
    markdown: str,
) -> None:
    """Indexes a document in the background; failures never block indexing."""
    if not enabled() or not markdown.strip():
        return
    workspace = workspace_for(tenant_id, kb_id)
    thread = threading.Thread(
        target=_insert_worker,
        args=(workspace, doc_id, file_name, markdown),
        daemon=True,
        name="lightrag-index",
    )
    thread.start()


async def _delete_async(workspace: str, doc_id: str) -> None:
    rag = _build_rag(workspace)
    await rag.initialize_storages()
    try:
        await rag.adelete_by_doc_id(doc_id)
    finally:
        await rag.finalize_storages()


def delete_document(tenant_id: str, kb_id: str, doc_id: str) -> None:
    if not enabled() or not doc_id:
        return
    workspace = workspace_for(tenant_id, kb_id)
    lock = _scope_lock(workspace)
    with lock:
        try:
            asyncio.run(_delete_async(workspace, doc_id))
        except Exception as exc:  # noqa: BLE001 - cascade delete is best-effort
            logger.warning("lightrag delete failed for %s/%s: %s", workspace, doc_id, exc)
