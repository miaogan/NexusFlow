from __future__ import annotations

from collections.abc import Sequence

HEADER = "早期对话摘要："
ROLE_LABELS = {"user": "用户", "assistant": "助手"}

# Per-message caps keep one verbose turn from crowding out the rest.
USER_SNIPPET_CHARS = 120
ASSISTANT_SNIPPET_CHARS = 200


def _collapse(text: str) -> str:
    return " ".join((text or "").split())


def _clean_previous(previous: str) -> str:
    """Strips our own header/label so re-folding does not nest summaries."""
    text = (previous or "").strip()
    if text.startswith(HEADER):
        text = text[len(HEADER) :].strip()
    if text.startswith("此前摘要："):
        text = text[len("此前摘要：") :].strip()
    return _collapse(text)


def _snippet(role: str, content: str) -> str:
    text = _collapse(content)
    limit = USER_SNIPPET_CHARS if role == "user" else ASSISTANT_SNIPPET_CHARS
    if len(text) > limit:
        text = text[:limit].rstrip() + "…"
    return text


def _render(previous_summary: str, entries: Sequence[str]) -> str:
    lines: list[str] = []
    if previous_summary:
        lines.append(f"此前摘要：{previous_summary}")
    lines.extend(entries)
    if not lines:
        return ""
    return f"{HEADER}\n" + "\n".join(lines)


def extractive_summary(
    previous_summary: str,
    messages: Sequence[dict[str, str]],
    budget_chars: int,
) -> str:
    """Folds the previous summary and older messages into a compact digest.

    Deterministic so it can run without an LLM key. When the budget is tight
    the oldest message lines are dropped first; the previous summary is
    truncated (never discarded outright) if it alone overflows.
    """
    budget = max(100, int(budget_chars))
    previous = _clean_previous(previous_summary)
    entries: list[str] = []
    for message in messages:
        role = str(message.get("role") or "user")
        snippet = _snippet(role, message.get("content") or "")
        if snippet:
            entries.append(f"{ROLE_LABELS.get(role, '消息')}：{snippet}")

    # Drop the oldest lines until we fit, always keeping the newest one.
    while len(entries) > 1 and len(_render(previous, entries)) > budget:
        entries.pop(0)

    text = _render(previous, entries)
    if len(text) <= budget:
        return text

    if previous:
        room = budget - len(HEADER) - 1
        for entry in entries:
            room -= len(entry) + 1
        previous = previous[: max(20, room)].rstrip() + "…"
        text = _render(previous, entries)
        if len(text) <= budget:
            return text

    # Last resort: hard truncation preserving the leading context.
    return text[:budget].rstrip()
