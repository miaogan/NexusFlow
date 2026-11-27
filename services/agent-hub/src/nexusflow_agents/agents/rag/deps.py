from __future__ import annotations

from functools import lru_cache

from nexusflow_agents.agents.rag.core.config import get_settings
from nexusflow_agents.agents.rag.graph import ChatGraph
from nexusflow_agents.agents.rag.services.chat import ChatService
from nexusflow_agents.agents.rag.services.corrected import HttpCorrectedAnswerProvider
from nexusflow_agents.agents.rag.services.hub_agents import (
    HubIntentClassifier,
    HubMemory,
    HubSummarizer,
)
from nexusflow_agents.agents.rag.services.llm import AnswerGenerator
from nexusflow_agents.agents.rag.services.retrieval import GatewayRetriever
from nexusflow_agents.agents.rag.services.store import (
    ConversationStore,
    InMemoryConversationStore,
    PostgresConversationStore,
)


@lru_cache
def get_store() -> ConversationStore:
    settings = get_settings()
    if settings.database_url:
        return PostgresConversationStore(settings.database_url)
    return InMemoryConversationStore()


@lru_cache
def get_chat_graph() -> ChatGraph:
    settings = get_settings()
    retriever = GatewayRetriever(settings.gateway_url, settings.search_timeout_seconds)
    generator = AnswerGenerator(
        base_url=settings.llm_base_url,
        api_key=settings.llm_api_key,
        model=settings.llm_model,
        timeout_seconds=settings.llm_timeout_seconds,
        max_context_chars=settings.max_context_chars,
    )
    return ChatGraph(retriever, generator)


@lru_cache
def get_intent_classifier() -> HubIntentClassifier:
    return HubIntentClassifier()


@lru_cache
def get_summarizer() -> HubSummarizer:
    return HubSummarizer(budget_chars=get_settings().summary_budget_chars)


@lru_cache
def get_memory_client() -> HubMemory:
    return HubMemory(top_k=get_settings().memory_top_k)


@lru_cache
def get_corrected_provider() -> HttpCorrectedAnswerProvider:
    settings = get_settings()
    return HttpCorrectedAnswerProvider(
        settings.gateway_url,
        settings.corrected_timeout_seconds,
        settings.corrected_top_k,
    )


@lru_cache
def get_service() -> ChatService:
    return ChatService(
        get_store(),
        get_chat_graph(),
        get_settings(),
        get_intent_classifier(),
        get_summarizer(),
        get_memory_client(),
        get_corrected_provider(),
    )
