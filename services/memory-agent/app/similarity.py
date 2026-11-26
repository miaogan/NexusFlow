from __future__ import annotations

import math
import re

_WORD = re.compile(r"[a-zA-Z0-9]+")


def tokenize(text: str) -> set[str]:
    """ASCII words plus CJK character bigrams: dependency-free and predictable."""
    lowered = (text or "").lower()
    tokens = set(_WORD.findall(lowered))
    cjk = [ch for ch in lowered if "\u4e00" <= ch <= "\u9fff"]
    tokens.update(a + b for a, b in zip(cjk, cjk[1:]))
    return tokens


def lexical_score(a: str, b: str) -> float:
    """Cosine over binary token sets, in [0, 1]."""
    ta, tb = tokenize(a), tokenize(b)
    if not ta or not tb:
        return 0.0
    overlap = len(ta & tb)
    return overlap / math.sqrt(len(ta) * len(tb))


def cosine(a: list[float] | None, b: list[float] | None) -> float:
    if not a or not b or len(a) != len(b):
        return 0.0
    dot = sum(x * y for x, y in zip(a, b))
    na = math.sqrt(sum(x * x for x in a))
    nb = math.sqrt(sum(y * y for y in b))
    if na == 0 or nb == 0:
        return 0.0
    return dot / (na * nb)


def vector_literal(values: list[float]) -> str:
    """Formats a Python list for a pgvector ``%s::vector`` cast."""
    return "[" + ",".join(f"{v:.6f}" for v in values) + "]"
