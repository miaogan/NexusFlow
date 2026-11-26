from __future__ import annotations

from typing import Any

import httpx


class RetrievalUnavailable(RuntimeError):
    """Raised when the gateway retrieval call fails."""


class GatewayRetriever:
    """Retrieves chunks through the Go gateway's unified /v1/search endpoint.

    Reusing the gateway keeps a single recall/rerank pipeline (and trace id)
    instead of duplicating retrieval inside the Python service.
    """

    def __init__(self, base_url: str, timeout_seconds: float = 15.0) -> None:
        self._base_url = base_url.rstrip("/")
        self._timeout = timeout_seconds

    def search(
        self,
        *,
        query: str,
        tenant_id: str,
        top_k: int,
        kb_id: str | None = None,
        filters: dict[str, str] | None = None,
        auth_header: str | None = None,
    ) -> tuple[list[dict[str, Any]], str | None]:
        headers = {"x-tenant-id": tenant_id}
        if auth_header:
            headers["Authorization"] = auth_header
        recall = max(top_k * 4, 20)
        payload: dict[str, Any] = {
            "query": query,
            "routes": [
                {"type": "vector", "top_k": recall},
                {"type": "bm25", "top_k": recall},
            ],
            "final_top_n": top_k,
        }
        if kb_id:
            payload["kb_id"] = kb_id
        if filters:
            payload["filters"] = filters

        try:
            resp = httpx.post(
                f"{self._base_url}/v1/search",
                json=payload,
                headers=headers,
                timeout=self._timeout,
            )
        except httpx.HTTPError as exc:
            raise RetrievalUnavailable(f"retrieval failed: {exc}") from exc

        # The query engine treats "all recall routes empty" as an error; for
        # chat that simply means nothing matched, so surface it as no context
        # and let the generator explain instead of failing the whole turn.
        if resp.status_code == 502 and "no recall route produced results" in resp.text:
            return [], None
        if resp.is_error:
            raise RetrievalUnavailable(
                f"retrieval failed: HTTP {resp.status_code}: {resp.text[:200]}"
            )

        data = resp.json()
        return list(data.get("chunks") or []), data.get("trace_id")
