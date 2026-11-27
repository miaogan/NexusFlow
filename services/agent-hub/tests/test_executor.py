from __future__ import annotations

import json
from typing import Any

import httpx
import pytest

from nexusflow_agents.core import executor as executor_module
from nexusflow_agents.core.executor import (
    HttpExecutor,
    SandboxAgentExecutor,
    validate_sandbox_config,
)
from nexusflow_agents.core.registry import AgentSpec


class FakeResponse:
    def __init__(self, payload: Any, status_code: int = 200) -> None:
        self._payload = payload
        self.status_code = status_code

    def raise_for_status(self) -> None:
        if self.status_code >= 400:
            request = httpx.Request("POST", "http://gateway/v1/sandbox/execute")
            raise httpx.HTTPStatusError(
                "boom", request=request, response=httpx.Response(self.status_code)
            )

    def json(self) -> Any:
        return self._payload


def _sandbox_spec(**overrides: Any) -> AgentSpec:
    sandbox = {
        "code": "print('hi')",
        "runtime": "python",
        "timeout_seconds": 5,
        "packages": ["numpy"],
    }
    sandbox.update(overrides.pop("sandbox", {}))
    return AgentSpec(
        name="sandbox.echo",
        capabilities=frozenset({"agent.call"}),
        kind="sandbox",
        risk="execute",
        sandbox=sandbox,
        tenant_id="t1",
    )


def test_validate_sandbox_config_accepts_valid_payload() -> None:
    config = validate_sandbox_config(
        {
            "code": "print(1)",
            "runtime": "node",
            "timeout_seconds": 5,
            "packages": ["left-pad"],
            "env_vars": {"MODE": "test"},
        }
    )

    assert config["runtime"] == "node"
    assert config["timeout_seconds"] == 5
    assert config["packages"] == ["left-pad"]
    assert config["env_vars"] == {"MODE": "test"}


@pytest.mark.parametrize(
    "payload",
    [
        None,
        {},
        {"code": ""},
        {"code": "x", "runtime": "ruby"},
        {"code": "x", "timeout_seconds": 99},
        {"code": "x", "packages": "numpy"},
        {"code": "x", "env_vars": {"BAD-NAME": "1"}},
    ],
)
def test_validate_sandbox_config_rejects_invalid_payload(payload: Any) -> None:
    with pytest.raises((TypeError, ValueError)):
        validate_sandbox_config(payload)


def test_sandbox_executor_sends_code_and_input(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    captured: dict[str, Any] = {}

    def fake_post(url: str, *, json: Any, headers: Any, timeout: Any) -> FakeResponse:
        captured.update({"url": url, "json": json, "headers": headers, "timeout": timeout})
        return FakeResponse(
            {
                "success": True,
                "output_json": json_module_dumps({"echo": "ok"}),
                "stdout": "hi\n",
                "stderr": "",
                "sandbox_id": "sbx-1",
            }
        )

    monkeypatch.setattr(executor_module.httpx, "post", fake_post)
    executor = SandboxAgentExecutor("http://gateway:8080")

    result = executor.call(
        _sandbox_spec(),
        "agent.call",
        {"text": "hello"},
        {
            "auth_header": "Bearer token",
            "tenant_id": "t1",
            "user_id": "u1",
            "trace_id": "tr-1",
        },
    )

    assert captured["url"] == "http://gateway:8080/v1/sandbox/execute"
    assert captured["json"]["code"] == "print('hi')"
    assert captured["json"]["runtime"] == "python"
    assert json.loads(captured["json"]["input_json"]) == {"text": "hello"}
    assert captured["headers"]["authorization"] == "Bearer token"
    assert captured["headers"]["x-tenant-id"] == "t1"
    assert result["output"] == {"echo": "ok"}
    assert result["sandbox_id"] == "sbx-1"


def test_sandbox_executor_requires_authorization(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    executor = SandboxAgentExecutor("http://gateway:8080")

    with pytest.raises(RuntimeError, match="authenticated caller"):
        executor.call(_sandbox_spec(), "agent.call", {}, {})


def test_sandbox_executor_surfaces_failure(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr(
        executor_module.httpx,
        "post",
        lambda *args, **kwargs: FakeResponse(
            {"success": False, "error_msg": "exit 1", "stderr": "traceback"}
        ),
    )
    executor = SandboxAgentExecutor("http://gateway:8080")

    with pytest.raises(RuntimeError, match="exit 1"):
        executor.call(
            _sandbox_spec(),
            "agent.call",
            {},
            {"auth_header": "Bearer token"},
        )


def test_http_executor_uses_custom_endpoint_for_agent_call(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    captured: dict[str, Any] = {}

    def fake_post(url: str, **kwargs: Any) -> FakeResponse:
        captured.update({"url": url, **kwargs})
        return FakeResponse({"ok": True})

    monkeypatch.setattr(executor_module.httpx, "post", fake_post)
    spec = AgentSpec(
        name="custom.agent",
        capabilities=frozenset({"agent.call"}),
        kind="http",
        endpoint="http://custom:9000/v1/call",
        tenant_id="t1",
    )

    result = HttpExecutor().call(spec, "agent.call", {"text": "x"}, {})

    assert result == {"ok": True}
    assert captured["url"] == "http://custom:9000/v1/call"


def json_module_dumps(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False)
