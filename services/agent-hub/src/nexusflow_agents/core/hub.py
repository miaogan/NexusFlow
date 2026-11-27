from __future__ import annotations

import logging
import time
from dataclasses import dataclass, field
from typing import Any

from nexusflow_agents.core.executor import Executor
from nexusflow_agents.core.lifecycle import BackgroundPool
from nexusflow_agents.core.metrics import (
    HUB_BACKGROUND_DROPPED,
    HUB_CALLS,
    HUB_DEGRADED,
    HUB_LATENCY,
)
from nexusflow_agents.core.registry import AgentRegistry, AgentSpec

logger = logging.getLogger(__name__)


class AgentUnavailable(RuntimeError):
    """Raised only when a required capability has no healthy agent."""


@dataclass
class AgentResult:
    ok: bool
    data: dict[str, Any] = field(default_factory=dict)
    error: str = ""
    agent: str = ""
    latency_ms: float = 0.0
    degraded: bool = False
    attempts: int = 0


class Hub:
    """Star-topology scheduler: routes a capability to healthy spokes."""

    def __init__(
        self,
        registry: AgentRegistry,
        executors: dict[str, Executor],
        *,
        max_attempts: int = 2,
        fail_threshold: int = 3,
        background: BackgroundPool | None = None,
    ) -> None:
        self._registry = registry
        self._executors = executors
        self._max_attempts = max(1, max_attempts)
        self._fail_threshold = max(1, fail_threshold)
        self._background = background

    @property
    def registry(self) -> AgentRegistry:
        return self._registry

    def call(
        self,
        capability: str,
        payload: dict[str, Any],
        ctx: dict[str, Any] | None = None,
        *,
        required: bool = False,
    ) -> AgentResult:
        context = ctx or {}
        tenant_id = str(context.get("tenant_id") or "")
        specs = self._registry.resolve(capability, tenant_id)[: self._max_attempts]
        if not specs:
            HUB_DEGRADED.labels(capability=capability).inc()
            if required:
                raise AgentUnavailable(f"no healthy agent for {capability}")
            return AgentResult(
                ok=False,
                error=f"no healthy agent for {capability}",
                degraded=True,
            )

        last_error = ""
        attempts = 0
        for spec in specs:
            attempts += 1
            executor = self._executors.get(spec.kind)
            if executor is None:
                last_error = f"no executor for kind {spec.kind}"
                continue
            started = time.perf_counter()
            try:
                data = executor.call(spec, capability, payload, context)
            except Exception as exc:  # noqa: BLE001 - try the next agent
                elapsed = (time.perf_counter() - started) * 1000
                last_error = str(exc) or exc.__class__.__name__
                HUB_CALLS.labels(
                    capability=capability, agent=spec.name, status="error"
                ).inc()
                HUB_LATENCY.labels(capability=capability, agent=spec.name).observe(
                    elapsed / 1000
                )
                self._registry.mark_failure(
                    spec.name, self._fail_threshold, spec.tenant_id
                )
                logger.warning(
                    "agent %s failed for %s: %s", spec.name, capability, last_error
                )
                continue

            elapsed = (time.perf_counter() - started) * 1000
            HUB_CALLS.labels(
                capability=capability, agent=spec.name, status="ok"
            ).inc()
            HUB_LATENCY.labels(capability=capability, agent=spec.name).observe(
                elapsed / 1000
            )
            self._registry.mark_success(spec.name, spec.tenant_id)
            return AgentResult(
                ok=True,
                data=data,
                agent=spec.name,
                latency_ms=round(elapsed, 2),
                attempts=attempts,
            )

        HUB_DEGRADED.labels(capability=capability).inc()
        if required:
            raise AgentUnavailable(last_error or f"no healthy agent for {capability}")
        return AgentResult(
            ok=False,
            error=last_error or f"no healthy agent for {capability}",
            degraded=True,
            attempts=attempts,
        )

    def call_agent(
        self,
        name: str,
        capability: str,
        payload: dict[str, Any],
        ctx: dict[str, Any] | None = None,
        *,
        required: bool = False,
    ) -> AgentResult:
        """Invokes one named spoke instead of resolving by capability."""
        context = ctx or {}
        tenant_id = str(context.get("tenant_id") or "")
        spec = self._registry.get(name, tenant_id)
        if spec is None:
            message = f"unknown agent: {name}"
            if required:
                raise AgentUnavailable(message)
            return AgentResult(ok=False, error=message, degraded=True)
        if capability and capability not in spec.capabilities:
            message = f"agent {name} does not provide {capability}"
            if required:
                raise AgentUnavailable(message)
            return AgentResult(ok=False, error=message, degraded=True, agent=name)

        executor = self._executors.get(spec.kind)
        if executor is None:
            message = f"no executor for kind {spec.kind}"
            if required:
                raise AgentUnavailable(message)
            return AgentResult(ok=False, error=message, degraded=True, agent=name)

        target = capability or next(iter(sorted(spec.capabilities)), "")
        started = time.perf_counter()
        try:
            data = executor.call(spec, target, payload, context)
        except Exception as exc:
            elapsed = (time.perf_counter() - started) * 1000
            message = str(exc) or exc.__class__.__name__
            HUB_CALLS.labels(capability=target, agent=spec.name, status="error").inc()
            HUB_LATENCY.labels(capability=target, agent=spec.name).observe(elapsed / 1000)
            self._registry.mark_failure(
                spec.name, self._fail_threshold, spec.tenant_id
            )
            if required:
                raise AgentUnavailable(message) from exc
            return AgentResult(
                ok=False,
                error=message,
                agent=spec.name,
                latency_ms=round(elapsed, 2),
                degraded=True,
                attempts=1,
            )

        elapsed = (time.perf_counter() - started) * 1000
        HUB_CALLS.labels(capability=target, agent=spec.name, status="ok").inc()
        HUB_LATENCY.labels(capability=target, agent=spec.name).observe(elapsed / 1000)
        self._registry.mark_success(spec.name, spec.tenant_id)
        return AgentResult(
            ok=True,
            data=data,
            agent=spec.name,
            latency_ms=round(elapsed, 2),
            attempts=1,
        )

    def call_skill(
        self,
        skill: str,
        payload: dict[str, Any],
        ctx: dict[str, Any] | None = None,
        *,
        required: bool = False,
    ) -> AgentResult:
        """Routes a named skill to a tenant's custom SubAgent."""
        context = ctx or {}
        tenant_id = str(context.get("tenant_id") or "")
        specs = self._registry.resolve_skill(skill, tenant_id)[: self._max_attempts]
        if not specs:
            HUB_DEGRADED.labels(capability=f"skill.{skill}").inc()
            message = f"no healthy agent for skill {skill}"
            if required:
                raise AgentUnavailable(message)
            return AgentResult(ok=False, error=message, degraded=True)

        last_error = ""
        attempts = 0
        for spec in specs:
            attempts += 1
            executor = self._executors.get(spec.kind)
            if executor is None:
                last_error = f"no executor for kind {spec.kind}"
                continue
            target = (
                "agent.call"
                if "agent.call" in spec.capabilities
                else next(iter(sorted(spec.capabilities)), skill)
            )
            started = time.perf_counter()
            try:
                data = executor.call(spec, target, payload, context)
            except Exception as exc:  # noqa: BLE001 - try the next skill agent
                elapsed = (time.perf_counter() - started) * 1000
                last_error = str(exc) or exc.__class__.__name__
                HUB_CALLS.labels(
                    capability=f"skill.{skill}", agent=spec.name, status="error"
                ).inc()
                HUB_LATENCY.labels(
                    capability=f"skill.{skill}", agent=spec.name
                ).observe(elapsed / 1000)
                self._registry.mark_failure(
                    spec.name, self._fail_threshold, spec.tenant_id
                )
                logger.warning(
                    "agent %s failed for skill %s: %s", spec.name, skill, last_error
                )
                continue

            elapsed = (time.perf_counter() - started) * 1000
            HUB_CALLS.labels(
                capability=f"skill.{skill}", agent=spec.name, status="ok"
            ).inc()
            HUB_LATENCY.labels(capability=f"skill.{skill}", agent=spec.name).observe(
                elapsed / 1000
            )
            self._registry.mark_success(spec.name, spec.tenant_id)
            return AgentResult(
                ok=True,
                data=data,
                agent=spec.name,
                latency_ms=round(elapsed, 2),
                attempts=attempts,
            )

        HUB_DEGRADED.labels(capability=f"skill.{skill}").inc()
        if required:
            raise AgentUnavailable(last_error or f"no healthy agent for skill {skill}")
        return AgentResult(
            ok=False,
            error=last_error or f"no healthy agent for skill {skill}",
            degraded=True,
            attempts=attempts,
        )

    def submit_background(
        self,
        capability: str,
        payload: dict[str, Any],
        ctx: dict[str, Any] | None = None,
    ) -> bool:
        if self._background is None:
            HUB_BACKGROUND_DROPPED.labels(capability=capability).inc()
            return False
        accepted = self._background.submit(self.call, capability, payload, ctx)
        if not accepted:
            HUB_BACKGROUND_DROPPED.labels(capability=capability).inc()
        return accepted

    def shutdown(self) -> None:
        if self._background is not None:
            self._background.shutdown()

    def register(self, spec: AgentSpec) -> None:
        self._registry.register(spec)
