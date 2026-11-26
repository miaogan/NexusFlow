from __future__ import annotations

from typing import Any, Protocol

import httpx


class CorrectedAnswerProvider(Protocol):
    """Looks up human corrections recorded against similar past questions."""

    def lookup(
        self,
        *,
        query: str,
        tenant_id: str,
        auth_header: str | None = None,
        limit: int = 3,
    ) -> list[dict[str, Any]]: ...


class NullCorrectedAnswerProvider:
    """Used when the gateway URL is not configured or lookup is disabled."""

    def lookup(self, **_: Any) -> list[dict[str, Any]]:
        return []


class HttpCorrectedAnswerProvider:
    """Reads corrections through the gateway feedback API.

    ``GET /v1/corrected-answers`` scores historical ``corrected_answer`` records
    against the current query and returns the best matches for the tenant. Any
    failure yields an empty list: corrections are an optimisation, never a
    reason to fail a chat turn.
    """

    def __init__(
        self, base_url: str, timeout_seconds: float = 5.0, default_limit: int = 3
    ) -> None:
        self._base_url = base_url.rstrip("/")
        self._timeout = timeout_seconds
        self._default_limit = default_limit

    def lookup(
        self,
        *,
        query: str,
        tenant_id: str,
        auth_header: str | None = None,
        limit: int | None = None,
    ) -> list[dict[str, Any]]:
        if not self._base_url or not query.strip():
            return []
        headers = {"x-tenant-id": tenant_id}
        if auth_header:
            headers["Authorization"] = auth_header
        want = limit or self._default_limit
        try:
            resp = httpx.get(
                f"{self._base_url}/v1/corrected-answers",
                params={"query": query, "limit": want},
                headers=headers,
                timeout=self._timeout,
            )
        except httpx.HTTPError:
            return []
        if resp.is_error:
            return []
        try:
            data = resp.json()
        except ValueError:
            return []

        out: list[dict[str, Any]] = []
        for item in data.get("items") or []:
            answer = str(item.get("corrected_answer") or "").strip()
            if not answer:
                continue
            out.append(
                {
                    "query": str(item.get("query") or "").strip(),
                    "answer": answer,
                    "score": float(item.get("match_score") or 0.0),
                }
            )
            if len(out) >= want:
                break
        return out
