from __future__ import annotations

from collections.abc import Sequence

# Deterministic extraction used when no LLM is configured. Prefixes map to the
# memory kind; the whole user utterance (cleaned) becomes the memory content so
# recall keeps enough context.
PATTERNS: tuple[tuple[str, tuple[str, ...]], ...] = (
    ("preference", ("我喜欢", "我偏好", "我更倾向", "我习惯", "我通常", "我不喜欢")),
    ("profile", ("我叫", "我的名字", "我是", "我的职业", "我来自", "我住在")),
    ("fact", ("请记住", "记住", "帮我记一下", "我的", "我在做", "我们团队")),
)


def clean_content(text: str, max_chars: int = 300) -> str:
    return " ".join((text or "").split())[:max_chars].strip()


def extract_rules(
    messages: Sequence[dict[str, str]],
    *,
    max_chars: int = 300,
    min_chars: int = 4,
) -> list[dict[str, str]]:
    """Pulls user-stated facts/preferences from a turn.

    Only the user's own utterances are considered; questions and very short
    fragments are ignored to avoid polluting memory.
    """
    found: list[dict[str, str]] = []
    seen: set[str] = set()
    for message in messages:
        if message.get("role") != "user":
            continue
        content = clean_content(message.get("content") or "", max_chars)
        if len(content) < min_chars or content.endswith(("?", "？")):
            continue
        key = content.lower()
        if key in seen:
            continue
        for kind, prefixes in PATTERNS:
            if any(prefix in content for prefix in prefixes):
                seen.add(key)
                found.append({"content": content, "kind": kind})
                break
    return found
