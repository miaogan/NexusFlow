from __future__ import annotations

import logging
from time import perf_counter
from typing import Any, TypedDict

from langgraph.graph import END, START, StateGraph
from prometheus_client import Counter, Histogram
from pydantic import BaseModel

from nexusflow_agents.agents.memory.core.config import Settings
from nexusflow_agents.agents.memory.embedding import HttpEmbedder
from nexusflow_agents.agents.memory.rules import clean_content, extract_rules
from nexusflow_agents.agents.memory.store import MemoryStore

logger = logging.getLogger(__name__)

RECALLS = Counter(
    "memory_agent_recalls_total",
    "Memory recalls by source",
    ["source"],
)
RECALL_LATENCY = Histogram(
    "memory_agent_recall_seconds",
    "Memory recall latency by source",
    ["source"],
    buckets=(0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5),
)
EXTRACTIONS = Counter(
    "memory_agent_extractions_total",
    "Extracted memories by source",
    ["source"],
)

EXTRACT_PROMPT = (
    "你是 NexusFlow 的长期记忆抽取器。从用户的发言中抽取值得跨会话记住的稳定信息："
    "用户画像（姓名、职业、所在地）、偏好与习惯、明确要求记住的事实。"
    "忽略一次性的问题、临时任务和助手说的话；不要编造；每条记忆用简洁完整的中文陈述句。"
)

VALID_KINDS = {"profile", "preference", "fact"}


class ExtractedMemory(BaseModel):
    content: str
    kind: str = "fact"


class ExtractedMemories(BaseModel):
    memories: list[ExtractedMemory] = []


class RecallState(TypedDict, total=False):
    tenant_id: str
    user_id: str
    query: str
    top_k: int
    memories: list[dict[str, Any]]
    source: str


class RecallGraph:
    """Two-node LangGraph: normalize the query, then recall memories."""

    def __init__(
        self, settings: Settings, store: MemoryStore, embedder: HttpEmbedder
    ) -> None:
        self._settings = settings
        self._store = store
        self._embedder = embedder

        builder = StateGraph(RecallState)
        builder.add_node("normalize", self._normalize)
        builder.add_node("recall", self._recall)
        builder.add_edge(START, "normalize")
        builder.add_edge("normalize", "recall")
        builder.add_edge("recall", END)
        self._compiled = builder.compile()

    def _normalize(self, state: RecallState) -> dict[str, Any]:
        top_k = int(state.get("top_k") or self._settings.top_k)
        return {
            "query": (state.get("query") or "").strip()[:8000],
            "top_k": max(1, min(top_k, 20)),
        }

    def _recall(self, state: RecallState) -> dict[str, Any]:
        query = state.get("query") or ""
        if not query:
            return {"memories": [], "source": "empty"}

        embedding = None
        if self._embedder.enabled:
            vectors = self._embedder.embed([query])
            embedding = vectors[0] if vectors else None

        source = "semantic" if embedding is not None else "lexical"
        start = perf_counter()
        memories = self._store.search(
            state["tenant_id"],
            state["user_id"],
            query,
            embedding,
            state.get("top_k") or self._settings.top_k,
        )
        RECALL_LATENCY.labels(source=source).observe(perf_counter() - start)
        RECALLS.labels(source=source).inc()
        return {"memories": memories, "source": source}

    def invoke(
        self,
        tenant_id: str,
        user_id: str,
        query: str,
        top_k: int | None = None,
    ) -> dict[str, Any]:
        state = self._compiled.invoke(
            {
                "tenant_id": tenant_id,
                "user_id": user_id,
                "query": query,
                "top_k": top_k,
            }
        )
        return {
            "memories": state.get("memories") or [],
            "source": state.get("source") or "empty",
        }


class ExtractState(TypedDict, total=False):
    tenant_id: str
    user_id: str
    messages: list[dict[str, str]]
    candidates: list[dict[str, str]]
    created: int
    merged: int
    memories: list[dict[str, Any]]
    source: str


class ExtractGraph:
    """LangGraph: normalize the turn, extract candidates, then persist them."""

    def __init__(
        self, settings: Settings, store: MemoryStore, embedder: HttpEmbedder
    ) -> None:
        self._settings = settings
        self._store = store
        self._embedder = embedder
        self._llm: Any | None = None
        self._tenant_llms: dict[str, Any] = {}
        if settings.llm_enabled:
            self._llm = self._build_llm("")

        builder = StateGraph(ExtractState)
        builder.add_node("normalize", self._normalize)
        builder.add_node("extract", self._extract)
        builder.add_node("persist", self._persist)
        builder.add_edge(START, "normalize")
        builder.add_edge("normalize", "extract")
        builder.add_edge("extract", "persist")
        builder.add_edge("persist", END)
        self._compiled = builder.compile()

    def _build_llm(self, tenant_id: str) -> Any:
        from langchain_openai import ChatOpenAI

        kwargs: dict[str, Any] = {}
        if tenant_id:
            kwargs["default_headers"] = {"x-tenant-id": tenant_id}
        model = ChatOpenAI(
            model=self._settings.llm_model,
            base_url=self._settings.llm_base_url,
            api_key=self._settings.llm_api_key,
            temperature=0,
            timeout=self._settings.llm_timeout_seconds,
            **kwargs,
        )
        return model.with_structured_output(ExtractedMemories)

    def _llm_for(self, tenant_id: str) -> Any:
        if not tenant_id or self._llm is None:
            return self._llm
        cached = self._tenant_llms.get(tenant_id)
        if cached is None:
            if len(self._tenant_llms) >= 32:
                self._tenant_llms.clear()
            cached = self._build_llm(tenant_id)
            self._tenant_llms[tenant_id] = cached
        return cached

    def _normalize(self, state: ExtractState) -> dict[str, Any]:
        messages = [
            {
                "role": str(message.get("role") or ""),
                "content": (message.get("content") or "")[: self._settings.max_message_chars],
            }
            for message in (state.get("messages") or [])[-self._settings.max_messages :]
            if message.get("role") in {"user", "assistant"}
        ]
        return {"messages": messages}

    def _extract(self, state: ExtractState) -> dict[str, Any]:
        messages = state.get("messages") or []
        if self._llm is not None and messages:
            try:
                candidates = self._extract_with_llm(
                    messages, state.get("tenant_id") or ""
                )
                if candidates:
                    EXTRACTIONS.labels(source="llm").inc()
                    return {"candidates": candidates, "source": "llm"}
            except Exception as exc:  # noqa: BLE001 - fall back to rules
                logger.warning("memory llm failed, using rules: %s", exc)

        candidates = extract_rules(
            messages,
            max_chars=self._settings.max_content_chars,
            min_chars=self._settings.min_content_chars,
        )
        EXTRACTIONS.labels(source="rules").inc()
        return {"candidates": candidates, "source": "rules"}

    def _extract_with_llm(
        self, messages: list[dict[str, str]], tenant_id: str = ""
    ) -> list[dict[str, str]]:
        from langchain_core.messages import HumanMessage, SystemMessage

        lines = []
        for message in messages:
            label = "用户" if message["role"] == "user" else "助手"
            lines.append(f"{label}：{message['content']}")
        result = self._llm_for(tenant_id).invoke(  # type: ignore[union-attr]
            [
                SystemMessage(content=EXTRACT_PROMPT),
                HumanMessage(content="\n".join(lines)),
            ]
        )
        if isinstance(result, dict):
            result = ExtractedMemories(**result)
        if not isinstance(result, ExtractedMemories):
            raise TypeError(f"unexpected structured output: {type(result)!r}")

        candidates: list[dict[str, str]] = []
        for item in result.memories:
            content = clean_content(item.content, self._settings.max_content_chars)
            if len(content) < self._settings.min_content_chars:
                continue
            kind = item.kind if item.kind in VALID_KINDS else "fact"
            candidates.append({"content": content, "kind": kind})
        return candidates

    def _persist(self, state: ExtractState) -> dict[str, Any]:
        candidates = state.get("candidates") or []
        if not candidates:
            return {"created": 0, "merged": 0, "memories": []}

        if self._embedder.enabled:
            vectors = self._embedder.embed([item["content"] for item in candidates])
            if vectors:
                for item, vector in zip(candidates, vectors):
                    item["embedding"] = vector

        created, merged = self._store.upsert(
            state["tenant_id"], state["user_id"], candidates
        )
        return {
            "created": created,
            "merged": merged,
            "memories": self._store.list(state["tenant_id"], state["user_id"], 20),
        }

    def invoke(
        self,
        tenant_id: str,
        user_id: str,
        messages: list[dict[str, str]],
    ) -> dict[str, Any]:
        state = self._compiled.invoke(
            {"tenant_id": tenant_id, "user_id": user_id, "messages": messages}
        )
        return {
            "created": int(state.get("created", 0)),
            "merged": int(state.get("merged", 0)),
            "memories": state.get("memories") or [],
        }
