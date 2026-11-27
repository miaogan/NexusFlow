from __future__ import annotations

import json
import re
from collections.abc import Callable, Mapping
from typing import Any, ClassVar, Protocol
from urllib.parse import urlsplit

import httpx

from nexusflow_agents.core.registry import (
    CAPABILITY_INTENT,
    CAPABILITY_MEMORY_EXTRACT,
    CAPABILITY_MEMORY_RECALL,
    CAPABILITY_SUMMARY,
    AgentSpec,
)

Handler = Callable[[dict[str, Any], dict[str, Any]], dict[str, Any]]

MAX_SANDBOX_CODE_BYTES = 64 << 10
MAX_SANDBOX_INPUT_BYTES = 64 << 10
MAX_SANDBOX_TIMEOUT_SECONDS = 60
MAX_SANDBOX_PACKAGES = 32
MAX_SANDBOX_ENV_VARS = 32
SANDBOX_RUNTIMES = {"python", "node"}
_ENV_NAME = re.compile(r"^[A-Za-z_][A-Za-z0-9_]{0,63}$")


def validate_sandbox_config(config: Mapping[str, Any] | None) -> dict[str, Any]:
    """Validates a sandbox SubAgent payload before persisting or executing it."""
    if not isinstance(config, Mapping):
        raise TypeError("sandbox config is required")
    code = config.get("code")
    if not isinstance(code, str) or not code.strip():
        raise ValueError("sandbox.code is required")
    if len(code.encode("utf-8")) > MAX_SANDBOX_CODE_BYTES:
        raise ValueError(
            f"sandbox.code exceeds {MAX_SANDBOX_CODE_BYTES} bytes"
        )
    runtime = str(config.get("runtime") or "python").strip().lower()
    if runtime not in SANDBOX_RUNTIMES:
        raise ValueError(f"sandbox.runtime must be one of {sorted(SANDBOX_RUNTIMES)}")
    try:
        timeout = int(config.get("timeout_seconds") or 10)
    except (TypeError, ValueError) as exc:
        raise ValueError("sandbox.timeout_seconds must be an integer") from exc
    if not 1 <= timeout <= MAX_SANDBOX_TIMEOUT_SECONDS:
        raise ValueError(
            f"sandbox.timeout_seconds must be 1..{MAX_SANDBOX_TIMEOUT_SECONDS}"
        )

    raw_packages = config.get("packages") or []
    if not isinstance(raw_packages, list):
        raise TypeError("sandbox.packages must be an array")
    packages = [str(item)[:128] for item in raw_packages][:MAX_SANDBOX_PACKAGES]

    raw_env = config.get("env_vars") or {}
    if not isinstance(raw_env, Mapping):
        raise TypeError("sandbox.env_vars must be an object")
    env_vars: dict[str, str] = {}
    for key, value in list(raw_env.items())[:MAX_SANDBOX_ENV_VARS]:
        name = str(key)
        if not _ENV_NAME.match(name):
            raise ValueError(f"invalid sandbox env var name: {name}")
        env_vars[name] = str(value)[:1024]

    return {
        "code": code,
        "runtime": runtime,
        "timeout_seconds": timeout,
        "packages": packages,
        "env_vars": env_vars,
    }


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
        if not spec.endpoint:
            raise RuntimeError(f"no remote endpoint for {capability}")
        path = self.PATHS.get(capability)
        # Custom SubAgents (agent.call / tool.call / ...) use their endpoint as
        # the full call URL; built-in workers keep their fixed sub-paths.
        url = spec.endpoint.rstrip("/") + path if path else spec.endpoint
        resp = httpx.post(
            url,
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
        parts = urlsplit(spec.endpoint)
        if not parts.scheme or not parts.netloc:
            return False
        try:
            resp = httpx.get(
                f"{parts.scheme}://{parts.netloc}/healthz",
                timeout=self._health_timeout,
            )
            return resp.status_code < 500
        except httpx.HTTPError:
            return False


class SandboxAgentExecutor:
    """Runs a sandbox SubAgent's code through the gateway sandbox endpoint.

    The caller's bearer token is forwarded so the gateway enforces
    ``sandbox:execute``; the hub never bypasses that permission.
    """

    def __init__(
        self,
        gateway_url: str,
        *,
        timeout_buffer_seconds: float = 10.0,
    ) -> None:
        self._gateway = gateway_url.rstrip("/")
        self._timeout_buffer = max(1.0, timeout_buffer_seconds)

    @staticmethod
    def _headers(ctx: dict[str, Any]) -> dict[str, str]:
        headers: dict[str, str] = {}
        for key, header in (
            ("auth_header", "authorization"),
            ("tenant_id", "x-tenant-id"),
            ("user_id", "x-user-id"),
            ("trace_id", "x-trace-id"),
            ("request_id", "x-request-id"),
        ):
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
        config = validate_sandbox_config(spec.sandbox)
        headers = self._headers(ctx)
        if "authorization" not in headers:
            raise RuntimeError(
                "sandbox agent requires an authenticated caller with sandbox:execute"
            )
        input_json = json.dumps(payload, ensure_ascii=False, default=str)
        if len(input_json.encode("utf-8")) > MAX_SANDBOX_INPUT_BYTES:
            raise RuntimeError(
                f"sandbox input exceeds {MAX_SANDBOX_INPUT_BYTES} bytes"
            )
        body: dict[str, Any] = {
            "code": config["code"],
            "runtime": config["runtime"],
            "timeout_seconds": config["timeout_seconds"],
            "input_json": input_json,
        }
        if config["packages"]:
            body["packages"] = config["packages"]
        if config["env_vars"]:
            body["env_vars"] = config["env_vars"]
        try:
            resp = httpx.post(
                f"{self._gateway}/v1/sandbox/execute",
                json=body,
                headers=headers,
                timeout=float(config["timeout_seconds"]) + self._timeout_buffer,
            )
            resp.raise_for_status()
            data = resp.json()
        except httpx.HTTPError as exc:
            raise RuntimeError(f"sandbox execution failed: {exc}") from exc
        if not isinstance(data, dict):
            raise TypeError("unexpected sandbox response")
        if not data.get("success"):
            reason = (
                data.get("error_msg") or data.get("stderr") or "sandbox run failed"
            )
            raise RuntimeError(str(reason))
        parsed: Any = {}
        output_json = data.get("output_json")
        if output_json:
            try:
                parsed = json.loads(str(output_json))
            except json.JSONDecodeError:
                parsed = {"raw": output_json}
        if not isinstance(parsed, dict):
            parsed = {"result": parsed}
        return {
            "agent": spec.name,
            "capability": capability,
            "output": parsed,
            "stdout": str(data.get("stdout") or ""),
            "stderr": str(data.get("stderr") or ""),
            "sandbox_id": str(data.get("sandbox_id") or ""),
        }

    def probe(self, spec: AgentSpec) -> bool:
        try:
            validate_sandbox_config(spec.sandbox)
        except (TypeError, ValueError):
            return False
        return True
