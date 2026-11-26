#!/usr/bin/env python3
"""Offline LLM-as-judge scoring for NexusFlow RAG answers.

For each golden query the script calls the gateway chat API to get an answer
plus its cited chunks, then asks an OpenAI-compatible model to score the answer
on faithfulness / relevancy / completeness. Only the standard library is used.

Usage:
    export RAG_LLM_BASE_URL=https://api.deepseek.com/v1
    export RAG_LLM_API_KEY=sk-...
    python3 eval/judge.py --base-url http://127.0.0.1:8080 --k 2

Exit codes: 0 ok, 1 metric below threshold, 2 configuration/runtime error.
"""
from __future__ import annotations

import argparse
import json
import os
import re
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from rag_eval import Case, load_cases, request_json, resolve_token

SCORE_KEYS = ("faithfulness", "relevancy", "completeness")
JSON_FENCE_RE = re.compile(r"^```(?:json)?\s*|\s*```$", re.MULTILINE)

SYSTEM_PROMPT = (
    "你是严谨的 RAG 答案质量评审员。根据用户问题、系统回答和检索到的引用片段，"
    "按三个维度打分（0 到 1 的小数）：\n"
    "- faithfulness：回答是否只依据引用片段，没有编造引用之外的事实；\n"
    "- relevancy：回答是否切中用户问题，没有答非所问；\n"
    "- completeness：回答是否覆盖问题所需的关键信息。\n"
    "只输出一个 JSON 对象，字段为 faithfulness、relevancy、completeness 和 reason，不要输出其他内容。"
)


def build_judge_messages(
    query: str, answer: str, contexts: list[str]
) -> list[dict[str, str]]:
    if contexts:
        material = "\n".join(
            f"[{idx}] {text}" for idx, text in enumerate(contexts, start=1)
        )
    else:
        material = "（无引用片段）"
    user = f"用户问题：{query}\n\n系统回答：{answer}\n\n引用片段：\n{material}"
    return [
        {"role": "system", "content": SYSTEM_PROMPT},
        {"role": "user", "content": user},
    ]


def parse_scores(text: str) -> dict[str, Any]:
    """Extract scores from a model reply, tolerating code fences and prose."""
    cleaned = JSON_FENCE_RE.sub("", text or "").strip()
    start, end = cleaned.find("{"), cleaned.rfind("}")
    if start == -1 or end == -1 or end <= start:
        raise ValueError("judge reply does not contain a JSON object")
    data = json.loads(cleaned[start : end + 1])
    if not isinstance(data, dict):
        raise ValueError("judge reply JSON is not an object")

    scores: dict[str, Any] = {}
    for key in SCORE_KEYS:
        value = data.get(key)
        if isinstance(value, bool) or not isinstance(value, (int, float)):
            raise ValueError(f"judge reply missing numeric '{key}'")
        scores[key] = min(1.0, max(0.0, float(value)))
    scores["reason"] = str(data.get("reason") or "")
    return scores


def judge_answer(
    cfg: argparse.Namespace,
    query: str,
    answer: str,
    contexts: list[str],
    timeout: float = 60.0,
) -> dict[str, Any]:
    payload = {
        "model": cfg.llm_model,
        "messages": build_judge_messages(query, answer, contexts),
        "temperature": 0,
        "response_format": {"type": "json_object"},
    }
    response = request_json(
        "POST",
        f"{cfg.llm_base_url.rstrip('/')}/chat/completions",
        payload,
        token=cfg.llm_api_key,
        timeout=timeout,
    )
    choices = response.get("choices") or []
    if not choices:
        raise RuntimeError("judge response has no choices")
    return parse_scores(str((choices[0].get("message") or {}).get("content") or ""))


def collect_case(
    args: argparse.Namespace, token: str, case: Case
) -> dict[str, Any]:
    payload: dict[str, Any] = {"message": case.query, "top_k": args.k}
    if case.kb_id:
        payload["kb_id"] = case.kb_id
    if case.filters:
        payload["filters"] = case.filters
    response = request_json(
        "POST", f"{args.base_url}/v1/chat", payload, token, timeout=args.timeout
    )
    citations = response.get("citations") or []
    contexts = [str(c.get("content") or "") for c in citations if c.get("content")]
    return {
        "query": case.query,
        "answer": str(response.get("answer") or ""),
        "contexts": contexts,
        "citations": len(citations),
        "intent": response.get("intent") or "",
    }


def aggregate(rows: list[dict[str, Any]]) -> dict[str, float]:
    scored = [row for row in rows if row.get("scores")]
    metrics: dict[str, float] = {}
    for key in SCORE_KEYS:
        if not scored:
            metrics[key] = 0.0
            continue
        metrics[key] = sum(row["scores"][key] for row in scored) / len(scored)
    metrics["judged"] = float(len(scored))
    metrics["errors"] = float(len(rows) - len(scored))
    return metrics


def check_thresholds(metrics: dict[str, float], thresholds: dict[str, float]) -> list[str]:
    failures: list[str] = []
    for name, minimum in thresholds.items():
        if name not in metrics:
            continue
        if metrics[name] + 1e-9 < float(minimum):
            failures.append(f"{name}={metrics[name]:.3f} < {float(minimum):.3f}")
    return failures


def llm_config(args: argparse.Namespace) -> argparse.Namespace | None:
    base_url = args.llm_base_url or os.environ.get("RAG_LLM_BASE_URL", "")
    api_key = args.llm_api_key or os.environ.get("RAG_LLM_API_KEY", "")
    if not base_url or not api_key:
        return None
    return argparse.Namespace(
        llm_base_url=base_url,
        llm_api_key=api_key,
        llm_model=args.llm_model or os.environ.get("RAG_LLM_MODEL", "deepseek-chat"),
    )


def print_report(
    args: argparse.Namespace,
    cfg: argparse.Namespace,
    rows: list[dict[str, Any]],
    metrics: dict[str, float],
    thresholds: dict[str, float],
) -> None:
    print(
        f"RAG judge  base={args.base_url}  model={cfg.llm_model}  "
        f"cases={len(rows)}  judged={int(metrics['judged'])}  "
        f"errors={int(metrics['errors'])}"
    )
    for key in SCORE_KEYS:
        limit = thresholds.get(key)
        if limit is None:
            print(f"  {key:<14} {metrics[key]:8.3f}")
        else:
            status = "PASS" if metrics[key] + 1e-9 >= float(limit) else "FAIL"
            print(f"  {key:<14} {metrics[key]:8.3f}  (>= {float(limit):.3f})  {status}")

    print("\nper-case:")
    print(f"  {'query':<34} {'faith':>6} {'relev':>6} {'compl':>6}  note")
    for row in rows:
        query = row["query"]
        if len(query) > 32:
            query = query[:31] + "…"
        if row.get("scores"):
            scores = row["scores"]
            note = ""
            print(
                f"  {query:<34} {scores['faithfulness']:6.2f} "
                f"{scores['relevancy']:6.2f} {scores['completeness']:6.2f}  {note}"
            )
        else:
            print(f"  {query:<34} {'-':>6} {'-':>6} {'-':>6}  {row.get('error', 'error')}")


def main() -> int:
    parser = argparse.ArgumentParser(description="LLM-as-judge for NexusFlow RAG answers")
    parser.add_argument("--base-url", default="http://127.0.0.1:8080")
    parser.add_argument("--dataset", default="eval/golden.jsonl")
    parser.add_argument("--thresholds", default="eval/judge_thresholds.json")
    parser.add_argument("--report", default="eval/judge_report.json")
    parser.add_argument("--k", type=int, default=2, help="top-k contexts for each answer")
    parser.add_argument("--limit", type=int, default=0, help="judge at most N cases (0 = all)")
    parser.add_argument("--tenant", default=os.environ.get("NEXUSFLOW_TENANT", "default"))
    parser.add_argument("--username", default=os.environ.get("NEXUSFLOW_USERNAME", "admin"))
    parser.add_argument(
        "--password", default=os.environ.get("NEXUSFLOW_PASSWORD", "admin123")
    )
    parser.add_argument("--token", default=None, help="bearer token (or NEXUSFLOW_TOKEN)")
    parser.add_argument("--timeout", type=float, default=60.0)
    parser.add_argument("--llm-base-url", default=None, help="or RAG_LLM_BASE_URL")
    parser.add_argument("--llm-api-key", default=None, help="or RAG_LLM_API_KEY")
    parser.add_argument("--llm-model", default=None, help="or RAG_LLM_MODEL")
    args = parser.parse_args()

    cfg = llm_config(args)
    if cfg is None:
        print(
            "judge requires an LLM: set RAG_LLM_BASE_URL and RAG_LLM_API_KEY "
            "(or pass --llm-base-url / --llm-api-key)",
            file=sys.stderr,
        )
        return 2

    try:
        cases = load_cases(Path(args.dataset))
        if args.limit > 0:
            cases = cases[: args.limit]
        thresholds: dict[str, float] = {}
        threshold_path = Path(args.thresholds)
        if threshold_path.exists():
            thresholds = json.loads(threshold_path.read_text(encoding="utf-8"))
        token = resolve_token(args)
    except Exception as exc:  # noqa: BLE001 - CLI should print a clean failure
        print(f"judge setup failed: {exc}", file=sys.stderr)
        return 2

    rows: list[dict[str, Any]] = []
    for case in cases:
        row: dict[str, Any] = {"query": case.query}
        try:
            collected = collect_case(args, token, case)
            row.update(collected)
            row["scores"] = judge_answer(
                cfg,
                collected["query"],
                collected["answer"],
                collected["contexts"],
                timeout=args.timeout,
            )
        except Exception as exc:  # noqa: BLE001 - keep judging the remaining cases
            row["error"] = str(exc)
        rows.append(row)

    if not any(row.get("scores") for row in rows):
        print("judge failed: no case produced a score", file=sys.stderr)
        return 2

    metrics = aggregate(rows)
    print_report(args, cfg, rows, metrics, thresholds)
    failures = check_thresholds(metrics, thresholds)

    report = {
        "generated_at": datetime.now(timezone.utc).isoformat(),
        "config": {
            "base_url": args.base_url,
            "k": args.k,
            "dataset": args.dataset,
            "llm_base_url": cfg.llm_base_url,
            "llm_model": cfg.llm_model,
        },
        "metrics": metrics,
        "thresholds": thresholds,
        "failures": failures,
        "cases": [
            {
                "query": row["query"],
                "answer": row.get("answer", ""),
                "citations": row.get("citations", 0),
                "intent": row.get("intent", ""),
                "scores": row.get("scores"),
                "reason": (row.get("scores") or {}).get("reason", ""),
                "error": row.get("error", ""),
            }
            for row in rows
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
