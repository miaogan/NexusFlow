"""MCP stdio transport.

Reads newline-delimited JSON-RPC from stdin and writes responses to stdout.
Diagnostics go to stderr so the protocol stream stays clean.

Usage:
    AGENT_HUB_MCP_ENABLED=true python -m nexusflow_agents.mcp.stdio
"""

from __future__ import annotations

import json
import logging
import sys
from typing import BinaryIO, TextIO

from nexusflow_agents.core.config import get_hub_settings
from nexusflow_agents.mcp.server import error_payload, handle_payload

logger = logging.getLogger(__name__)


def process_stream(reader: BinaryIO, writer: TextIO, max_bytes: int) -> int:
    """Processes newline-delimited JSON-RPC until EOF. Returns an exit code."""
    for raw in reader:
        line = raw.strip()
        if not line:
            continue
        if len(line) > max_bytes:
            response = error_payload(None, -32600, "request line too large")
        else:
            try:
                payload = json.loads(line)
            except json.JSONDecodeError as exc:
                response = error_payload(None, -32700, f"parse error: {exc}")
            else:
                response = handle_payload(payload)
        if response is None:
            continue
        writer.write(json.dumps(response, ensure_ascii=False, default=str) + "\n")
        writer.flush()
    return 0


def main() -> int:
    logging.basicConfig(stream=sys.stderr, level=logging.INFO)
    settings = get_hub_settings()
    if not settings.mcp_enabled:
        print(
            "MCP is disabled; set AGENT_HUB_MCP_ENABLED=true to run stdio.",
            file=sys.stderr,
        )
        return 2
    logger.info("MCP stdio transport started")
    return process_stream(sys.stdin.buffer, sys.stdout, settings.mcp_max_bytes)


if __name__ == "__main__":
    raise SystemExit(main())
