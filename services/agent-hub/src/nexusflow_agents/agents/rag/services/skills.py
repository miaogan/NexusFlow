from __future__ import annotations

import json
from typing import Any

from nexusflow_agents.core.hub import Hub
from nexusflow_agents.core.registry import AgentRegistry
from nexusflow_agents.core.runtime import get_hub, get_registry


class SkillUnavailable(RuntimeError):
    """Raised when no healthy custom SubAgent serves the requested skill."""


class SkillRouter:
    """Resolves tenant-visible skills and invokes their SubAgents."""

    def __init__(
        self,
        hub: Hub | None = None,
        registry: AgentRegistry | None = None,
    ) -> None:
        self._hub = hub or get_hub()
        self._registry = registry or get_registry()

    def describe(self, tenant_id: str) -> list[dict[str, Any]]:
        return self._registry.available_skills(tenant_id)

    def available(self, tenant_id: str) -> list[str]:
        return [str(item["skill"]) for item in self.describe(tenant_id)]

    def invoke(
        self,
        skill: str,
        message: str,
        history: list[dict[str, str]],
        *,
        tenant_id: str,
        user_id: str,
        auth_header: str | None = None,
    ) -> dict[str, Any]:
        result = self._hub.call_skill(
            skill,
            {"message": message, "history": history},
            {
                "tenant_id": tenant_id,
                "user_id": user_id,
                "auth_header": auth_header or "",
            },
        )
        if not result.ok:
            raise SkillUnavailable(result.error or f"skill {skill} unavailable")
        return result.data

    @staticmethod
    def answer(data: dict[str, Any]) -> str:
        """Normalizes a SubAgent response into answer text."""
        for key in ("answer", "output", "text", "result"):
            value = data.get(key)
            if isinstance(value, str) and value.strip():
                return value.strip()
            if isinstance(value, (dict, list)) and value:
                return json.dumps(value, ensure_ascii=False)
        return json.dumps(data, ensure_ascii=False)
