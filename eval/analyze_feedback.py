#!/usr/bin/env python3
"""Offline feedback triage for NexusFlow.

Pulls low-rated answers from the gateway feedback API, attributes each failure
to a coarse stage (retrieval vs generation vs citation formatting) and exports
the samples as a regression set that can be replayed after retrieval/prompt
changes. Only the standard library is used so it runs in CI without installs.

Usage:
    python3 eval/analyze_feedback.py --base-url http://127.0.0.1:8080
    python3 eval/analyze_feedback.py --limit 50 --out /tmp/regressions.jsonl

The script is read-only: it never mutates feedback or conversations.
"""
from __future__ import annotations

import argparse
import json
import os
import re
import sys
from collections import Counter
from datetime import datetime, timezone
from pathlib import Path
from typing import Any
from urllib import error, request

MARKER_RE = re.compile(r"\[(\d+)\]")

# Phrases the generator uses when it cannot ground an answer in the corpus.
# A negative rating on such an answer most likely means retrieval missed.
NO_ANSWER_PHRASES = (
    "未在知识库",
    "资料不足",
    "没有找到",
    "无法回答",
    "没有相关内容",
)

GENERATION_WRONG = "generation_wrong"
RETRIEVAL_MISS = "retrieval_miss"
CITATION_MISSING = "citation_missing"
CITATION_OUT_OF_RANGE = "citation_out_of_range"
UNCLASSIFIED = "unclassified"

ATTRIBUTIONS = (
    GENERATION_WRONG,
    RETRIEVAL_MISS,
    CITATION_MISSING,
    CITATION_OUT_OF_RANGE,
    UNCLASSIFIED,
)


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


def fetch_feedback(args: argparse.Namespace, token: str) -> list[dict[str, Any]]:
    rating = args.rating
    rating_param = str(int(rating)) if float(rating).is_integer() else str(rating)
    url = (
        f"{args.base_url}/v1/feedback"
        f"?rating={rating_param}&limit={args.limit}&offset=0"
    )
    body = request_json("GET", url, token=token, timeout=args.timeout)
    items = body.get("feedback") or []
    if not isinstance(items, list):
        raise RuntimeError("unexpected feedback payload: 'feedback' is not a list")
    return [item for item in items if isinstance(item, dict)]


def entry_intent(entry: dict[str, Any]) -> str:
    meta = entry.get("meta") or {}
    intent = str(meta.get("intent") or "").strip()
    return intent or "unknown"


def entry_citations(entry: dict[str, Any]) -> list[str]:
    raw = entry.get("cited_chunk_ids") or []
    return [str(item) for item in raw if str(item)]


def marker_indexes(answer: str) -> list[int]:
    return [int(match) for match in MARKER_RE.findall(answer)]


def has_no_answer_phrase(answer: str) -> bool:
    return any(phrase in answer for phrase in NO_ANSWER_PHRASES)


def classify_feedback(entry: dict[str, Any]) -> str:
    """Attribute a negative rating to the most likely failing stage.

    Priority is deliberate: an explicit correction means the pipeline produced
    a wrong answer (even if retrieval was fine), and a grounded-but-empty
    answer points at retrieval before we blame citation formatting.
    """
    if str(entry.get("corrected_answer") or "").strip():
        return GENERATION_WRONG

    answer = str(entry.get("answer") or "")
    citations = entry_citations(entry)
    if not citations or has_no_answer_phrase(answer):
        return RETRIEVAL_MISS

    markers = marker_indexes(answer)
    if not markers:
        return CITATION_MISSING
    if max(markers) > len(citations):
        return CITATION_OUT_OF_RANGE
    return UNCLASSIFIED


def build_regression(entry: dict[str, Any], attribution: str) -> dict[str, Any]:
    return {
        "query": str(entry.get("query") or ""),
        "answer": str(entry.get("answer") or ""),
        "cited_chunk_ids": entry_citations(entry),
        "attribution": attribution,
        "rating": entry.get("rating"),
        "intent": entry_intent(entry),
        "message_id": entry.get("message_id") or "",
        "conversation_id": entry.get("conversation_id") or "",
        "trace_id": entry.get("trace_id") or "",
        "comment": entry.get("comment") or "",
        "corrected_answer": entry.get("corrected_answer") or "",
        "created_at": entry.get("created_at") or "",
    }


def summarize(
    entries: list[dict[str, Any]], args: argparse.Namespace
) -> dict[str, Any]:
    by_attribution: Counter[str] = Counter()
    by_intent: dict[str, Counter[str]] = {}
    items: list[dict[str, Any]] = []
    regressions: list[dict[str, Any]] = []

    for entry in entries:
        attribution = classify_feedback(entry)
        intent = entry_intent(entry)
        by_attribution[attribution] += 1
        by_intent.setdefault(intent, Counter())[attribution] += 1

        items.append(
            {
                "subject_id": entry.get("subject_id") or "",
                "message_id": entry.get("message_id") or "",
                "conversation_id": entry.get("conversation_id") or "",
                "trace_id": entry.get("trace_id") or "",
                "intent": intent,
                "attribution": attribution,
                "rating": entry.get("rating"),
                "query": entry.get("query") or "",
                "comment": entry.get("comment") or "",
                "citations": len(entry_citations(entry)),
                "has_corrected_answer": bool(
                    str(entry.get("corrected_answer") or "").strip()
                ),
                "created_at": entry.get("created_at") or "",
            }
        )
        regressions.append(build_regression(entry, attribution))

    return {
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "config": {
            "base_url": args.base_url,
            "rating": args.rating,
            "limit": args.limit,
        },
        "total": len(entries),
        "by_attribution": {name: by_attribution.get(name, 0) for name in ATTRIBUTIONS},
        "by_intent": {
            intent: {name: counts.get(name, 0) for name in ATTRIBUTIONS}
            for intent, counts in sorted(by_intent.items())
        },
        "items": items,
        "regressions": regressions,
    }


def write_outputs(report: dict[str, Any], report_path: Path, out_path: Path) -> None:
    report_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.parent.mkdir(parents=True, exist_ok=True)

    summary = {key: value for key, value in report.items() if key != "regressions"}
    report_path.write_text(
        json.dumps(summary, ensure_ascii=False, indent=2), encoding="utf-8"
    )
    lines = [
        json.dumps(item, ensure_ascii=False)
        for item in report.get("regressions") or []
    ]
    out_path.write_text("\n".join(lines) + ("\n" if lines else ""), encoding="utf-8")


def print_report(report: dict[str, Any], report_path: Path, out_path: Path) -> None:
    print(
        f"feedback triage  base={report['config']['base_url']} "
        f"rating={report['config']['rating']}  samples={report['total']}"
    )
    if not report["total"]:
        print("  no negative feedback found; wrote empty outputs")
        print(f"  report: {report_path}")
        print(f"  regressions: {out_path}")
        return

    print("\nby attribution:")
    for name in ATTRIBUTIONS:
        count = report["by_attribution"].get(name, 0)
        share = count / report["total"] * 100.0
        print(f"  {name:<24} {count:5d}  {share:5.1f}%")

    print("\nby intent:")
    for intent, counts in report["by_intent"].items():
        total = sum(counts.values())
        parts = ", ".join(
            f"{name}={counts[name]}" for name in ATTRIBUTIONS if counts.get(name)
        )
        print(f"  {intent:<16} {total:5d}  {parts}")

    print(f"\nreport written to {report_path}")
    print(f"regressions written to {out_path}")


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Attribute negative NexusFlow feedback and export a regression set"
    )
    parser.add_argument("--base-url", default="http://127.0.0.1:8080")
    parser.add_argument("--tenant", default=os.environ.get("NEXUSFLOW_TENANT", "default"))
    parser.add_argument("--username", default=os.environ.get("NEXUSFLOW_USERNAME", "admin"))
    parser.add_argument(
        "--password", default=os.environ.get("NEXUSFLOW_PASSWORD", "admin123")
    )
    parser.add_argument("--token", default=None, help="bearer token (or NEXUSFLOW_TOKEN)")
    parser.add_argument(
        "--limit", type=int, default=200, help="max feedback rows to analyze (<=200)"
    )
    parser.add_argument(
        "--rating", type=float, default=-1.0, help="rating to triage (default: -1)"
    )
    parser.add_argument("--report", default="eval/feedback_report.json")
    parser.add_argument("--out", default="eval/regressions.jsonl")
    parser.add_argument("--timeout", type=float, default=30.0)
    args = parser.parse_args()

    try:
        token = resolve_token(args)
        entries = fetch_feedback(args, token)
    except Exception as exc:  # noqa: BLE001 - CLI should print a clean failure
        print(f"feedback analysis failed: {exc}", file=sys.stderr)
        return 2

    report = summarize(entries, args)
    report_path = Path(args.report)
    out_path = Path(args.out)
    write_outputs(report, report_path, out_path)
    print_report(report, report_path, out_path)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
