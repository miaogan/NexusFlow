from __future__ import annotations

from functools import lru_cache

from app.core.config import get_settings
from app.graph import ChatGraph
from app.services.chat import ChatService
from app.services.intent import HttpIntentClassifier
from app.services.llm import AnswerGenerator
from app.services.retrieval import GatewayRetriever
from app.services.store import (
    ConversationStore,
    InMemoryConversationStore,
    PostgresConversationStore,
)
from app.services.summary import HttpSummarizer


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
def get_intent_classifier() -> HttpIntentClassifier:
    settings = get_settings()
    return HttpIntentClassifier(settings.intent_url, settings.intent_timeout_seconds)


@lru_cache
def get_summarizer() -> HttpSummarizer:
    settings = get_settings()
    return HttpSummarizer(
        settings.summary_url,
        settings.summary_timeout_seconds,
        settings.summary_budget_chars,
    )


@lru_cache
def get_service() -> ChatService:
    return ChatService(
        get_store(),
        get_chat_graph(),
        get_settings(),
        get_intent_classifier(),
        get_summarizer(),
    )
