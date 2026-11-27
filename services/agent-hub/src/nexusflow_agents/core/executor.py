from __future__ import annotations

from collections.abc import Callable, Mapping
from typing import Any, ClassVar, Protocol

import httpx

from nexusflow_agents.core.registry import (
    CAPABILITY_INTENT,
    CAPABILITY_MEMORY_EXTRACT,
    CAPABILITY_MEMORY_RECALL,
    CAPABILITY_SUMMARY,
    AgentSpec,
)

Handler = Callable[[dict[str, Any], dict[str, Any]], dict[str, Any]]


class Executor(Protocol):
    def call(
        self,
        spec: AgentSpec,
        capability: str,
        payload: dict[str, Any],
        ctx: dict[str, Any],
    ) -> dict[str, Any]: ...

    def probe(self, spec: AgentSpec) -> bool: ...


class LocalExecutor:
    """Invokes the in-process agent implementation for a capability."""

    def __init__(self, handlers: Mapping[str, Handler]) -> None:
        self._handlers = dict(handlers)

    def call(
        self,
        spec: AgentSpec,
        capability: str,
        payload: dict[str, Any],
        ctx: dict[str, Any],
    ) -> dict[str, Any]:
        handler = self._handlers.get(capability)
        if handler is None:
            raise RuntimeError(f"no local handler for {capability}")
        return handler(payload, {**ctx, "agent": spec.name})

    def probe(self, spec: AgentSpec) -> bool:
        return True


class HttpExecutor:
    """Calls a remote agent worker over the existing HTTP contract."""

    PATHS: ClassVar[dict[str, str]] = {
        CAPABILITY_INTENT: "/v1/intent",
        CAPABILITY_SUMMARY: "/v1/summarize",
        CAPABILITY_MEMORY_RECALL: "/v1/memories/search",
        CAPABILITY_MEMORY_EXTRACT: "/v1/memories/extract",
    }

    def __init__(self, health_timeout_seconds: float = 2.0) -> None:
        self._health_timeout = health_timeout_seconds

    @staticmethod
    def _headers(ctx: dict[str, Any]) -> dict[str, str]:
        headers: dict[str, str] = {}
        mapping = {
            "tenant_id": "x-tenant-id",
            "user_id": "x-user-id",
            "trace_id": "x-trace-id",
            "auth_header": "authorization",
        }
        for key, header in mapping.items():
            value = ctx.get(key)
            if value:
                headers[header] = str(value)
        return headers

    def call(
        self,
        spec: AgentSpec,
        capability: str,
        payload: dict[str, Any],
        ctx: dict[str, Any],
    ) -> dict[str, Any]:
        path = self.PATHS.get(capability)
        if path is None or not spec.endpoint:
            raise RuntimeError(f"no remote endpoint for {capability}")
        resp = httpx.post(
            f"{spec.endpoint.rstrip('/')}{path}",
            json=payload,
            headers=self._headers(ctx),
            timeout=max(0.1, spec.timeout_ms / 1000),
        )
        resp.raise_for_status()
        data = resp.json()
        if not isinstance(data, dict):
            raise TypeError(f"unexpected response for {capability}: {type(data)!r}")
        return data

    def probe(self, spec: AgentSpec) -> bool:
        if not spec.endpoint:
            return False
        try:
            resp = httpx.get(
                f"{spec.endpoint.rstrip('/')}/healthz",
                timeout=self._health_timeout,
            )
            return resp.status_code < 500
        except httpx.HTTPError:
            return False
