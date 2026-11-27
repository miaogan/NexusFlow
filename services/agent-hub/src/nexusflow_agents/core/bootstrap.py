from __future__ import annotations

import logging
from dataclasses import dataclass

from nexusflow_agents.agents.rag.core.config import settings as rag_settings
from nexusflow_agents.core.agent_store import AgentStore, build_agent_store
from nexusflow_agents.core.config import HubSettings, get_hub_settings
from nexusflow_agents.core.executor import HttpExecutor, LocalExecutor
from nexusflow_agents.core.hub import Hub
from nexusflow_agents.core.lifecycle import AgentLifecycle, BackgroundPool
from nexusflow_agents.core.local_agents import LOCAL_HANDLERS
from nexusflow_agents.core.registry import (
    CAPABILITY_INTENT,
    CAPABILITY_MEMORY_EXTRACT,
    CAPABILITY_MEMORY_RECALL,
    CAPABILITY_SUMMARY,
    AgentRegistry,
    AgentSpec,
)
from nexusflow_agents.core.tooling import ToolRegistry, ToolService

logger = logging.getLogger(__name__)

INTENT_CAPABILITIES = frozenset({CAPABILITY_INTENT})
SUMMARY_CAPABILITIES = frozenset({CAPABILITY_SUMMARY})
MEMORY_CAPABILITIES = frozenset({CAPABILITY_MEMORY_RECALL, CAPABILITY_MEMORY_EXTRACT})


def _local_spec(name: str, capabilities: frozenset[str], priority: int) -> AgentSpec:
    return AgentSpec(
        name=f"local.{name}",
        capabilities=capabilities,
        kind="local",
        priority=priority,
    )


def _http_spec(
    name: str,
    capabilities: frozenset[str],
    endpoint: str,
    timeout_seconds: float,
    priority: int,
) -> AgentSpec:
    return AgentSpec(
        name=f"remote.{name}",
        capabilities=capabilities,
        kind="http",
        endpoint=endpoint,
        timeout_ms=max(100, int(timeout_seconds * 1000)),
        priority=priority,
    )


def build_registry(settings: HubSettings | None = None) -> AgentRegistry:
    settings = settings or get_hub_settings()
    registry = AgentRegistry()
    role = settings.role
    mode = settings.mode
    prefer_local = settings.prefer == "local"
    local_priority, remote_priority = (10, 20) if prefer_local else (20, 10)

    if role in {"intent", "summary", "memory"}:
        # Split worker: only its own capability is hosted locally.
        if role == "intent":
            registry.register(_local_spec("intent", INTENT_CAPABILITIES, 10))
        elif role == "summary":
            registry.register(_local_spec("summary", SUMMARY_CAPABILITIES, 10))
        else:
            registry.register(_local_spec("memory", MEMORY_CAPABILITIES, 10))
        return registry

    if mode in {"local", "hybrid"}:
        registry.register(_local_spec("intent", INTENT_CAPABILITIES, local_priority))
        registry.register(_local_spec("summary", SUMMARY_CAPABILITIES, local_priority))
        registry.register(_local_spec("memory", MEMORY_CAPABILITIES, local_priority))

    if mode in {"remote", "hybrid"}:
        if rag_settings.intent_url:
            registry.register(
                _http_spec(
                    "intent",
                    INTENT_CAPABILITIES,
                    rag_settings.intent_url,
                    rag_settings.intent_timeout_seconds,
                    remote_priority,
                )
            )
        if rag_settings.summary_url:
            registry.register(
                _http_spec(
                    "summary",
                    SUMMARY_CAPABILITIES,
                    rag_settings.summary_url,
                    rag_settings.summary_timeout_seconds,
                    remote_priority,
                )
            )
        if rag_settings.memory_url:
            registry.register(
                _http_spec(
                    "memory",
                    MEMORY_CAPABILITIES,
                    rag_settings.memory_url,
                    rag_settings.memory_timeout_seconds,
                    remote_priority,
                )
            )
    return registry


def build_tool_service(settings: HubSettings | None = None) -> ToolService:
    """Registers tools according to the capability switches.

    Tools are only imported when a switch is on so a default deployment has no
    extra HTTP client or workflow dependency at startup.
    """
    settings = settings or get_hub_settings()
    service = ToolService(ToolRegistry())
    if settings.tools_enabled or settings.reasoning_enabled or settings.mcp_enabled:
        from nexusflow_agents.agents.rag.tools.gateway import register_gateway_tools

        register_gateway_tools(service)
    if settings.workflow_enabled:
        from nexusflow_agents.agents.rag.tools.workflow import register_workflow_tools

        register_workflow_tools(service)
    if settings.mcp_enabled and settings.mcp_servers:
        from nexusflow_agents.mcp.client import register_mcp_tools

        register_mcp_tools(service, settings)
    return service


@dataclass
class Runtime:
    registry: AgentRegistry
    hub: Hub
    lifecycle: AgentLifecycle
    http_executor: HttpExecutor
    tools: ToolService
    agent_store: AgentStore


def _load_custom_agents(registry: AgentRegistry, store: AgentStore) -> None:
    try:
        specs = store.list()
    except Exception as exc:  # noqa: BLE001 - never block hub startup
        logger.warning("load custom agents failed: %s", exc)
        return
    for spec in specs:
        registry.register(spec)
    if specs:
        logger.info("loaded %d custom agents from registry", len(specs))


def build_runtime(settings: HubSettings | None = None) -> Runtime:
    settings = settings or get_hub_settings()
    registry = build_registry(settings)
    agent_store = build_agent_store(
        settings.registry_driver, settings.registry_database_url
    )
    _load_custom_agents(registry, agent_store)
    local_executor = LocalExecutor(LOCAL_HANDLERS)
    http_executor = HttpExecutor(settings.health_timeout_seconds)
    background = BackgroundPool(
        settings.background_workers,
        max_pending=max(16, settings.background_workers * 8),
    )
    hub = Hub(
        registry,
        {"local": local_executor, "http": http_executor},
        max_attempts=settings.max_attempts,
        fail_threshold=settings.fail_threshold,
        background=background,
    )
    lifecycle = AgentLifecycle(
        registry,
        http_executor,
        interval_seconds=settings.health_interval_seconds,
        fail_threshold=settings.fail_threshold,
    )
    return Runtime(
        registry=registry,
        hub=hub,
        lifecycle=lifecycle,
        http_executor=http_executor,
        tools=build_tool_service(settings),
        agent_store=agent_store,
    )
