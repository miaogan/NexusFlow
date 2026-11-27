from __future__ import annotations

import itertools
import json
import logging
from typing import Any

import httpx

from nexusflow_agents.core.config import HubSettings
from nexusflow_agents.core.tooling import ToolError, ToolService, ToolSpec

logger = logging.getLogger(__name__)

PROTOCOL_VERSION = "2024-11-05"


class MCPError(RuntimeError):
    pass


def parse_servers(raw: str) -> list[tuple[str, str]]:
    """Parses ``name=url,name2=url2`` into unique (name, url) pairs."""
    servers: list[tuple[str, str]] = []
    seen: set[str] = set()
    for item in (raw or "").replace(";", ",").split(","):
        item = item.strip()
        if not item:
            continue
        if "=" in item:
            name, url = item.split("=", 1)
        else:
            name, url = "", item
        name = name.strip()
        url = url.strip()
        if not url.startswith(("http://", "https://")):
            logger.warning("skipping MCP server with invalid url: %s", url)
            continue
        if not name:
            name = url.split("://", 1)[-1].split("/", 1)[0].replace(":", "_")
        if name in seen:
            continue
        seen.add(name)
        servers.append((name, url))
    return servers


class MCPClient:
    """Minimal HTTP JSON-RPC MCP client with time/size bounds."""

    def __init__(
        self,
        url: str,
        *,
        timeout_seconds: float = 10.0,
        max_bytes: int = 1 << 20,
    ) -> None:
        self.url = url
        self.timeout = timeout_seconds
        self.max_bytes = max_bytes
        self._ids = itertools.count(1)

    def _rpc(self, method: str, params: dict[str, Any] | None = None) -> dict[str, Any]:
        body = {
            "jsonrpc": "2.0",
            "id": next(self._ids),
            "method": method,
        }
        if params is not None:
            body["params"] = params
        try:
            resp = httpx.post(self.url, json=body, timeout=self.timeout)
            resp.raise_for_status()
        except httpx.HTTPError as exc:
            raise MCPError(f"mcp {method} failed: {exc}") from exc
        if len(resp.content) > self.max_bytes:
            raise MCPError("mcp response exceeded size limit")
        try:
            data = resp.json()
        except json.JSONDecodeError as exc:
            raise MCPError(f"mcp {method} returned invalid json: {exc}") from exc
        if not isinstance(data, dict):
            raise MCPError("mcp response must be an object")
        if data.get("error"):
            error = data["error"]
            raise MCPError(str(error.get("message") if isinstance(error, dict) else error))
        result = data.get("result")
        return result if isinstance(result, dict) else {}

    def initialize(self) -> dict[str, Any]:
        result = self._rpc(
            "initialize",
            {
                "protocolVersion": PROTOCOL_VERSION,
                "capabilities": {},
                "clientInfo": {"name": "nexusflow-agent-hub", "version": "1.0.0"},
            },
        )
        try:
            self._rpc("notifications/initialized")
        except MCPError:
            pass
        return result

    def list_tools(self) -> list[dict[str, Any]]:
        result = self._rpc("tools/list")
        tools = result.get("tools")
        return tools if isinstance(tools, list) else []

    def call_tool(self, name: str, arguments: dict[str, Any]) -> dict[str, Any]:
        result = self._rpc("tools/call", {"name": name, "arguments": arguments})
        if result.get("isError"):
            content = result.get("content") or []
            text = " ".join(
                str(item.get("text") or "")
                for item in content
                if isinstance(item, dict)
            )
            raise ToolError(text or f"mcp tool {name} failed")
        return _decode_content(result)


def _decode_content(result: dict[str, Any]) -> dict[str, Any]:
    content = result.get("content")
    if not isinstance(content, list):
        return result
    texts: list[str] = []
    for item in content:
        if isinstance(item, dict) and item.get("type") == "text":
            texts.append(str(item.get("text") or ""))
    if len(texts) == 1:
        try:
            decoded = json.loads(texts[0])
            if isinstance(decoded, dict):
                return decoded
            return {"data": decoded}
        except json.JSONDecodeError:
            pass
    return {"content": texts, "raw": result}


def register_mcp_tools(service: ToolService, settings: HubSettings) -> list[str]:
    """Connects to configured MCP servers and registers their tools.

    Failures are logged and skipped so an unreachable third party never blocks
    hub startup.
    """
    registered: list[str] = []
    for server_name, url in parse_servers(settings.mcp_servers):
        client = MCPClient(
            url,
            timeout_seconds=settings.mcp_timeout_seconds,
            max_bytes=settings.mcp_max_bytes,
        )
        try:
            client.initialize()
            tools = client.list_tools()
        except Exception as exc:  # noqa: BLE001 - external servers are optional
            logger.warning("mcp server %s unavailable: %s", server_name, exc)
            continue
        for tool in tools:
            remote = str(tool.get("name") or "")
            if not remote:
                continue
            name = f"mcp.{server_name}.{remote}"
            schema = tool.get("inputSchema") or tool.get("input_schema") or {}
            spec = ToolSpec(
                name=name,
                description=str(tool.get("description") or f"MCP tool {remote}"),
                input_schema=schema if isinstance(schema, dict) else {},
                risk="external",
                timeout_ms=max(100, int(settings.mcp_timeout_seconds * 1000)),
                requires_approval=True,
            )
            service.register(spec, _handler(client, remote))
            registered.append(name)
    return registered


def _handler(client: MCPClient, remote: str):
    def call(payload: dict[str, Any], ctx: dict[str, Any]) -> dict[str, Any]:
        return client.call_tool(remote, payload)

    return call
