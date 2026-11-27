from __future__ import annotations

import json
import threading
import time
from collections.abc import Callable
from dataclasses import dataclass, field
from typing import Any, Literal

ToolRisk = Literal["read", "write", "execute", "external"]
ToolHandler = Callable[[dict[str, Any], dict[str, Any]], dict[str, Any]]
MAX_TOOL_OUTPUT_BYTES = 256 << 10


@dataclass(frozen=True)
class ToolSpec:
    name: str
    description: str
    input_schema: dict[str, Any] = field(default_factory=dict)
    output_schema: dict[str, Any] = field(default_factory=dict)
    risk: ToolRisk = "read"
    timeout_ms: int = 5000
    requires_approval: bool = False


class ToolError(RuntimeError):
    pass


class ApprovalRequired(ToolError):
    """Raised when a high-risk tool may only run after human approval."""

    def __init__(self, spec: ToolSpec) -> None:
        super().__init__(f"tool requires approval: {spec.name}")
        self.tool = spec.name
        self.risk = spec.risk


class ToolRegistry:
    def __init__(self) -> None:
        self._lock = threading.RLock()
        self._tools: dict[str, ToolSpec] = {}

    def register(self, spec: ToolSpec) -> None:
        with self._lock:
            self._tools[spec.name] = spec

    def get(self, name: str) -> ToolSpec | None:
        with self._lock:
            return self._tools.get(name)

    def list(self) -> list[ToolSpec]:
        with self._lock:
            return sorted(self._tools.values(), key=lambda spec: spec.name)

    def snapshot(self) -> list[dict[str, Any]]:
        return [
            {
                "name": spec.name,
                "description": spec.description,
                "input_schema": spec.input_schema,
                "output_schema": spec.output_schema,
                "risk": spec.risk,
                "timeout_ms": spec.timeout_ms,
                "requires_approval": spec.requires_approval,
            }
            for spec in self.list()
        ]


def validate_payload(schema: dict[str, Any], payload: dict[str, Any]) -> None:
    """Small JSON-Schema subset: object/required/properties/type/enum."""
    if not schema:
        return
    expected = schema.get("type")
    if expected and expected != "object":
        raise ToolError(f"tool input must be an object, got {type(payload).__name__}")
    properties = schema.get("properties") or {}
    for key in schema.get("required") or []:
        if key not in payload:
            raise ToolError(f"missing required field: {key}")
    if schema.get("additionalProperties") is False:
        unknown = set(payload) - set(properties)
        if unknown:
            raise ToolError(f"unexpected fields: {', '.join(sorted(unknown))}")
    for key, value in payload.items():
        rule = properties.get(key) or {}
        if not rule:
            continue
        kind = rule.get("type")
        if kind == "string" and not isinstance(value, str):
            raise ToolError(f"{key} must be a string")
        if kind == "integer" and not isinstance(value, int):
            raise ToolError(f"{key} must be an integer")
        if kind == "number" and not isinstance(value, (int, float)):
            raise ToolError(f"{key} must be a number")
        if kind == "boolean" and not isinstance(value, bool):
            raise ToolError(f"{key} must be a boolean")
        if kind == "array" and not isinstance(value, list):
            raise ToolError(f"{key} must be an array")
        if kind == "object" and not isinstance(value, dict):
            raise ToolError(f"{key} must be an object")
        if "enum" in rule and value not in rule["enum"]:
            raise ToolError(f"{key} must be one of {rule['enum']}")


class ToolService:
    """Executes registered tools with schema, deadline and approval guards."""

    def __init__(
        self,
        registry: ToolRegistry,
        handlers: dict[str, ToolHandler] | None = None,
        *,
        max_output_bytes: int = MAX_TOOL_OUTPUT_BYTES,
    ) -> None:
        self._registry = registry
        self._handlers = dict(handlers or {})
        self._max_output_bytes = max_output_bytes

    @property
    def registry(self) -> ToolRegistry:
        return self._registry

    def register(self, spec: ToolSpec, handler: ToolHandler) -> None:
        self._registry.register(spec)
        self._handlers[spec.name] = handler

    def specs(self) -> list[dict[str, Any]]:
        return self._registry.snapshot()

    def llm_tools(self, names: frozenset[str] | None = None) -> list[dict[str, Any]]:
        """OpenAI-compatible function definitions for bind_tools."""
        tools: list[dict[str, Any]] = []
        for spec in self._registry.list():
            if names and spec.name not in names:
                continue
            tools.append(
                {
                    "type": "function",
                    "function": {
                        "name": spec.name,
                        "description": spec.description,
                        "parameters": spec.input_schema
                        or {"type": "object", "properties": {}},
                    },
                }
            )
        return tools

    def call(
        self,
        name: str,
        payload: dict[str, Any],
        ctx: dict[str, Any] | None = None,
        *,
        approved: bool = False,
    ) -> dict[str, Any]:
        spec = self._registry.get(name)
        if spec is None:
            raise ToolError(f"unknown tool: {name}")
        handler = self._handlers.get(name)
        if handler is None:
            raise ToolError(f"tool has no handler: {name}")
        context = ctx or {}
        audit_base = {
            "tenant_id": str(context.get("tenant_id") or ""),
            "user_id": str(context.get("user_id") or ""),
            "trace_id": str(context.get("trace_id") or ""),
        }
        from nexusflow_agents.core.audit import get_audit

        if spec.requires_approval and not approved:
            get_audit().record(
                "tool.call",
                resource=name,
                risk=spec.risk,
                decision="pending",
                status="approval_required",
                **audit_base,
            )
            raise ApprovalRequired(spec)
        deadline_ms = int(context.get("deadline_ms") or 0)
        if deadline_ms and time.time() * 1000 >= deadline_ms:
            raise ToolError(f"tool deadline exceeded before calling {name}")
        try:
            validate_payload(spec.input_schema, payload)
            result = handler(payload, context)
            if not isinstance(result, dict):
                raise ToolError(f"tool {name} returned a non-object result")
        except ToolError as exc:
            get_audit().record(
                "tool.call",
                resource=name,
                risk=spec.risk,
                decision="denied",
                status="error",
                detail={"error": str(exc)[:200]},
                **audit_base,
            )
            raise
        except Exception as exc:
            get_audit().record(
                "tool.call",
                resource=name,
                risk=spec.risk,
                decision="denied",
                status="error",
                detail={"error": str(exc)[:200]},
                **audit_base,
            )
            raise ToolError(f"tool {name} failed: {exc}") from exc
        truncated = self._truncate(result)
        get_audit().record(
            "tool.call",
            resource=name,
            risk=spec.risk,
            decision="allowed",
            status="ok",
            detail={
                "keys": sorted(payload),
                "truncated": bool(truncated.get("truncated")),
            },
            **audit_base,
        )
        return truncated

    def _truncate(self, result: dict[str, Any]) -> dict[str, Any]:
        if self._max_output_bytes <= 0:
            return result
        encoded = json.dumps(result, ensure_ascii=False, default=str)
        if len(encoded.encode("utf-8")) <= self._max_output_bytes:
            return result
        preview = encoded[: self._max_output_bytes]
        return {
            "truncated": True,
            "reason": f"tool output exceeded {self._max_output_bytes} bytes",
            "preview": preview,
        }
