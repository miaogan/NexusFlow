from __future__ import annotations

import os

# Tests must stay offline and deterministic even when a developer has a local
# services/agent-hub/.env with real LLM credentials.
for _key in (
    "RAG_LLM_BASE_URL",
    "RAG_LLM_API_KEY",
    "INTENT_LLM_BASE_URL",
    "INTENT_LLM_API_KEY",
    "SUMMARY_LLM_BASE_URL",
    "SUMMARY_LLM_API_KEY",
    "MEMORY_LLM_BASE_URL",
    "MEMORY_LLM_API_KEY",
    "MEMORY_EMBEDDING_BASE_URL",
    "MEMORY_EMBEDDING_API_KEY",
    "MEMORY_DATABASE_URL",
    "RAG_DATABASE_URL",
):
    os.environ[_key] = ""

os.environ["AGENT_HUB_ROLE"] = "all"
os.environ["AGENT_HUB_MODE"] = "local"
os.environ["AGENT_HUB_DYNAMIC_REGISTRATION"] = "false"
os.environ["MEMORY_DRIVER"] = "memory"
