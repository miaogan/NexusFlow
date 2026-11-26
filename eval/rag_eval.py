#!/usr/bin/env python3
"""Offline RAG evaluation for NexusFlow.

Runs a golden query set against the gateway search API (and optionally the chat
API) and reports retrieval quality plus citation sanity. Only the standard
library is used so the script can run in CI without extra installs.

Usage:
    python3 eval/rag_eval.py --base-url http://127.0.0.1:8080 --k 2
    python3 eval/rag_eval.py --with-chat --k 2

Exit code is non-zero when a metric falls below eval/thresholds.json.
"""
from __future__ import annotations

import argparse
import json
import math
import os
import re
import sys
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any
from urllib import error, request

MARKER_RE = re.compile(r"\[(\d+)\]")


@dataclass
class Case:
    query: str
    relevant: set[str]
    kb_id: str | None = None
    filters: dict[str, str] = field(default_factory=dict)


@dataclass
class RetrievalResult:
    query: str
    relevant: list[str]
    retrieved: list[str]
    latency_ms: float
    recall: float
    mrr: float
    ndcg: float


@dataclass
class ChatResult:
    query: str
    citations: int
    valid_citations: int
    relevant_citations: int
    has_inline_marker: bool


def load_cases(path: Path) -> list[Case]:
    cases: list[Case] = []
    for lineno, raw_line in enumerate(path.read_text(encoding="utf-8").splitlines(), start=1):
        line = raw_line.strip()
        if not line or line.startswith("#"):
            continue
        raw = json.loads(line)
        relevant = raw.get("relevant_ids") or raw.get("relevant") or []
        if not raw.get("query") or not relevant:
            raise ValueError(f"{path}:{lineno}: query and relevant_ids are required")
        cases.append(
            Case(
                query=str(raw["query"]),
                relevant={str(item) for item in relevant},
                kb_id=raw.get("kb_id"),
                filters={str(k): str(v) for k, v in (raw.get("filters") or {}).items()},
            )
        )
    if not cases:
        raise ValueError(f"{path}: no cases found")
    return cases


def request_json(
    method: str,
    url: str,
    payload: dict[str, Any] | None = None,
    token: str | None = None,
    timeout: float = 30.0,
) -> dict[str, Any]:
    data = json.dumps(payload).encode("utf-8") if payload is not None else None
    req = request.Request(url, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", f"Bearer {token}")
    try:
        with request.urlopen(req, timeout=timeout) as resp:
            return json.loads(resp.read().decode("utf-8"))
    except error.HTTPError as exc:
        body = exc.read().decode("utf-8", "replace")
        raise RuntimeError(f"{method} {url} -> HTTP {exc.code}: {body}") from exc
    except error.URLError as exc:
        raise RuntimeError(f"{method} {url} failed: {exc.reason}") from exc


def resolve_token(args: argparse.Namespace) -> str:
    token = args.token or os.environ.get("NEXUSFLOW_TOKEN")
    if token:
        return token
    login = request_json(
        "POST",
        f"{args.base_url}/v1/auth/login",
        {
            "tenant_id": args.tenant,
            "username": args.username,
            "password": args.password,
        },
        timeout=args.timeout,
    )
    token = login.get("access_token")
    if not token:
        raise RuntimeError("login succeeded without an access_token")
    return token


def chunk_keys(chunk: dict[str, Any]) -> set[str]:
    keys = {str(chunk.get("chunk_id") or "")}
    meta = chunk.get("meta") or {}
    for name in ("doc_id", "document_id", "file_name"):
        value = meta.get(name)
        if value:
            keys.add(str(value))
    keys.discard("")
    return keys


def is_relevant(chunk: dict[str, Any], relevant: set[str]) -> bool:
    return bool(chunk_keys(chunk) & relevant)


def matched_ids(chunks: list[dict[str, Any]], relevant: set[str]) -> set[str]:
    found: set[str] = set()
    for chunk in chunks:
        found |= chunk_keys(chunk) & relevant
    return found


def recall_at_k(chunks: list[dict[str, Any]], relevant: set[str]) -> float:
    if not relevant:
        return 0.0
    return len(matched_ids(chunks, relevant)) / len(relevant)


def mrr(chunks: list[dict[str, Any]], relevant: set[str]) -> float:
    for rank, chunk in enumerate(chunks, start=1):
        if is_relevant(chunk, relevant):
            return 1.0 / rank
    return 0.0


def ndcg_at_k(chunks: list[dict[str, Any]], relevant: set[str]) -> float:
    gains = [1.0 if is_relevant(chunk, relevant) else 0.0 for chunk in chunks]
    dcg = sum(gain / math.log2(idx + 2) for idx, gain in enumerate(gains))
    ideal = [1.0] * min(len(relevant), len(chunks))
    idcg = sum(1.0 / math.log2(idx + 2) for idx in range(len(ideal)))
    return dcg / idcg if idcg else 0.0


def percentile(values: list[float], pct: float) -> float:
    if not values:
        return 0.0
    ordered = sorted(values)
    idx = max(0, min(len(ordered) - 1, math.ceil(pct / 100.0 * len(ordered)) - 1))
    return ordered[idx]


def evaluate_retrieval(
    args: argparse.Namespace, token: str, cases: list[Case]
) -> list[RetrievalResult]:
    results: list[RetrievalResult] = []
    for case in cases:
        payload: dict[str, Any] = {"query": case.query, "final_top_n": args.k}
        if case.kb_id:
            payload["kb_id"] = case.kb_id
        if case.filters:
            payload["filters"] = case.filters

        started = time.perf_counter()
        response = request_json(
            "POST", f"{args.base_url}/v1/search", payload, token, timeout=args.timeout
        )
        latency = (time.perf_counter() - started) * 1000.0
        chunks = (response.get("chunks") or [])[: args.k]

        results.append(
            RetrievalResult(
                query=case.query,
                relevant=sorted(case.relevant),
                retrieved=[str(c.get("chunk_id") or "") for c in chunks],
                latency_ms=latency,
                recall=recall_at_k(chunks, case.relevant),
                mrr=mrr(chunks, case.relevant),
                ndcg=ndcg_at_k(chunks, case.relevant),
            )
        )
    return results


def evaluate_chat(
    args: argparse.Namespace, token: str, cases: list[Case]
) -> list[ChatResult]:
    results: list[ChatResult] = []
    for case in cases:
        payload: dict[str, Any] = {"message": case.query, "top_k": args.k}
        if case.kb_id:
            payload["kb_id"] = case.kb_id
        if case.filters:
            payload["filters"] = case.filters

        response = request_json(
            "POST", f"{args.base_url}/v1/chat", payload, token, timeout=args.timeout
        )
        citations = response.get("citations") or []
        answer = str(response.get("answer") or "")
        results.append(
            ChatResult(
                query=case.query,
                citations=len(citations),
                valid_citations=sum(1 for c in citations if c.get("chunk_id")),
                relevant_citations=sum(
                    1
                    for c in citations
                    if bool(chunk_keys(c) & case.relevant)
                ),
                has_inline_marker=bool(MARKER_RE.search(answer)),
            )
        )
    return results


def summarize(
    retrieval: list[RetrievalResult], chat: list[ChatResult]
) -> dict[str, float]:
    latencies = [item.latency_ms for item in retrieval]
    metrics = {
        "recall_at_k": sum(item.recall for item in retrieval) / len(retrieval),
        "mrr": sum(item.mrr for item in retrieval) / len(retrieval),
        "ndcg_at_k": sum(item.ndcg for item in retrieval) / len(retrieval),
        "latency_p50_ms": percentile(latencies, 50),
        "latency_p95_ms": percentile(latencies, 95),
    }

    total_citations = sum(item.citations for item in chat)
    cited_answers = [item for item in chat if item.citations > 0]
    if chat:
        metrics["citation_validity"] = (
            sum(item.valid_citations for item in chat) / total_citations
            if total_citations
            else 0.0
        )
        metrics["citation_hit_rate"] = (
            sum(1 for item in cited_answers if item.relevant_citations > 0)
            / len(cited_answers)
            if cited_answers
            else 0.0
        )
        metrics["inline_marker_rate"] = (
            sum(1 for item in cited_answers if item.has_inline_marker)
            / len(cited_answers)
            if cited_answers
            else 0.0
        )
        metrics["no_citation_rate"] = (
            1.0 - len(cited_answers) / len(chat) if chat else 0.0
        )
    return metrics


def check_thresholds(metrics: dict[str, float], thresholds: dict[str, float]) -> list[str]:
    failures: list[str] = []
    for name, minimum in thresholds.items():
        if name not in metrics:
            continue
        if metrics[name] + 1e-9 < float(minimum):
            failures.append(f"{name}={metrics[name]:.3f} < {float(minimum):.3f}")
    return failures


def print_report(
    args: argparse.Namespace,
    retrieval: list[RetrievalResult],
    chat: list[ChatResult],
    metrics: dict[str, float],
    thresholds: dict[str, float],
) -> None:
    print(f"RAG evaluation  base={args.base_url}  k={args.k}  cases={len(retrieval)}")
    for name, value in metrics.items():
        limit = thresholds.get(name)
        if limit is None:
            print(f"  {name:<22} {value:8.3f}")
        else:
            status = "PASS" if value + 1e-9 >= float(limit) else "FAIL"
            print(f"  {name:<22} {value:8.3f}  (>= {float(limit):.3f})  {status}")

    print("\nper-case retrieval:")
    print(f"  {'query':<34} {'recall':>6} {'mrr':>6} {'ndcg':>6}  retrieved")
    for item in retrieval:
        query = item.query if len(item.query) <= 32 else item.query[:31] + "…"
        print(
            f"  {query:<34} {item.recall:6.2f} {item.mrr:6.2f} {item.ndcg:6.2f}  "
            + ",".join(item.retrieved)
        )

    if chat:
        print("\nper-case chat citations:")
        for item in chat:
            print(
                f"  {item.query[:34]:<34} citations={item.citations} "
                f"valid={item.valid_citations} relevant={item.relevant_citations} "
                f"marker={item.has_inline_marker}"
            )


def main() -> int:
    parser = argparse.ArgumentParser(description="NexusFlow RAG offline evaluation")
    parser.add_argument("--base-url", default="http://127.0.0.1:8080")
    parser.add_argument("--dataset", default="eval/golden.jsonl")
    parser.add_argument("--thresholds", default="eval/thresholds.json")
    parser.add_argument("--report", default="eval/report.json")
    parser.add_argument("--k", type=int, default=2, help="top-k for retrieval metrics")
    parser.add_argument("--tenant", default=os.environ.get("NEXUSFLOW_TENANT", "default"))
    parser.add_argument("--username", default=os.environ.get("NEXUSFLOW_USERNAME", "admin"))
    parser.add_argument(
        "--password", default=os.environ.get("NEXUSFLOW_PASSWORD", "admin123")
    )
    parser.add_argument("--token", default=None, help="bearer token (or NEXUSFLOW_TOKEN)")
    parser.add_argument("--timeout", type=float, default=30.0)
    parser.add_argument(
        "--with-chat",
        action="store_true",
        help="also call /v1/chat and report citation sanity metrics",
    )
    args = parser.parse_args()

    try:
        cases = load_cases(Path(args.dataset))
        thresholds: dict[str, float] = {}
        threshold_path = Path(args.thresholds)
        if threshold_path.exists():
            thresholds = json.loads(threshold_path.read_text(encoding="utf-8"))
        token = resolve_token(args)
        retrieval = evaluate_retrieval(args, token, cases)
        chat = evaluate_chat(args, token, cases) if args.with_chat else []
    except Exception as exc:  # noqa: BLE001 - CLI should print a clean failure
        print(f"evaluation failed: {exc}", file=sys.stderr)
        return 2

    metrics = summarize(retrieval, chat)
    print_report(args, retrieval, chat, metrics, thresholds)
    failures = check_thresholds(metrics, thresholds)

    report = {
        "config": {
            "base_url": args.base_url,
            "k": args.k,
            "dataset": args.dataset,
            "with_chat": args.with_chat,
        },
        "metrics": metrics,
        "thresholds": thresholds,
        "failures": failures,
        "cases": [
            {
                "query": item.query,
                "relevant": item.relevant,
                "retrieved": item.retrieved,
                "latency_ms": round(item.latency_ms, 2),
                "recall": item.recall,
                "mrr": item.mrr,
                "ndcg": item.ndcg,
            }
            for item in retrieval
        ],
        "chat": [
            {
                "query": item.query,
                "citations": item.citations,
                "valid_citations": item.valid_citations,
                "relevant_citations": item.relevant_citations,
                "inline_marker": item.has_inline_marker,
            }
            for item in chat
        ],
    }
    Path(args.report).parent.mkdir(parents=True, exist_ok=True)
    Path(args.report).write_text(
        json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8"
    )

    if failures:
        print("\nthreshold failures: " + "; ".join(failures), file=sys.stderr)
        return 1
    print(f"\nreport written to {args.report}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
