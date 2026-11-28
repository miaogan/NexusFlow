"""PII detection and masking for the LLM boundary.

Prompts, retrieved context and model replies are shielded with deterministic
tokens such as ``{{EMAIL_1}}``. The default policy keeps tokens in the reply
(masked output); ``RAG_PII_RESTORE_OUTPUT=true`` restores the original values
for deployments where the answer must remain complete.

Masking happens at the prompt boundary, not in storage: uploaded documents and
conversation history keep their original text, while everything sent to a model
(chunks, history, summary, memories, corrections) is masked.
"""

from __future__ import annotations

import re
from collections import Counter
from dataclasses import dataclass, field
from typing import Any

# Patterns run in order; earlier matches are replaced by tokens so a later
# pattern cannot partially match the same digits (e.g. an ID inside a card).
_PATTERNS: list[tuple[str, re.Pattern[str]]] = [
    (
        "email",
        re.compile(r"(?<![A-Za-z0-9._%+-])[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}"),
    ),
    (
        "id_card",
        re.compile(r"(?<![0-9A-Za-z])\d{17}[\dXx](?![0-9A-Za-z])"),
    ),
    (
        "bank_card",
        re.compile(r"(?<!\d)(?:\d[ -]?){16,19}(?!\d)"),
    ),
    (
        "phone",
        re.compile(r"(?<!\d)1[3-9]\d{9}(?!\d)"),
    ),
    (
        "ipv4",
        re.compile(
            r"(?<![\d.])(?:(?:25[0-5]|2[0-4]\d|1?\d?\d)\.){3}"
            r"(?:25[0-5]|2[0-4]\d|1?\d?\d)(?![\d.])"
        ),
    ),
]


@dataclass
class MaskSession:
    """Per-request masking state so tokens stay stable within one answer."""

    enabled: bool = False
    restore_output: bool = False
    _mapping: dict[str, str] = field(default_factory=dict)
    _by_value: dict[tuple[str, str], str] = field(default_factory=dict)
    _counters: Counter[str] = field(default_factory=Counter)
    _total: int = 0

    def mask(self, text: str | None) -> str:
        if not self.enabled or not text:
            return text or ""
        masked = text
        for kind, pattern in _PATTERNS:
            masked = pattern.sub(
                lambda match, kind=kind: self._token(kind, match.group(0)), masked
            )
        return masked

    def mask_chunks(self, chunks: list[dict[str, Any]]) -> list[dict[str, Any]]:
        if not self.enabled:
            return chunks
        out: list[dict[str, Any]] = []
        for chunk in chunks:
            copied = dict(chunk)
            copied["content"] = self.mask(chunk.get("content") or "")
            out.append(copied)
        return out

    def restore(self, text: str) -> str:
        if not self._mapping or not text:
            return text
        restored = text
        for token, original in self._mapping.items():
            restored = restored.replace(token, original)
        return restored

    def output(self, answer: str) -> str:
        """Applies the outbound policy: restore tokens or re-mask the reply."""
        if not self.enabled or not answer:
            return answer
        if self.restore_output:
            return self.restore(answer)
        # Catch PII the model generated on its own, not just echoed tokens.
        return self.mask(answer)

    def counts(self) -> dict[str, int]:
        return dict(self._counters)

    def total(self) -> int:
        return self._total

    def _token(self, kind: str, original: str) -> str:
        key = (kind, original)
        if key in self._by_value:
            return self._by_value[key]
        self._counters[kind] += 1
        self._total += 1
        token = f"{{{{{kind.upper()}_{self._counters[kind]}}}}}"
        self._mapping[token] = original
        self._by_value[key] = token
        return token


class PIIShield:
    def __init__(self, enabled: bool = False, restore_output: bool = False) -> None:
        self.enabled = enabled
        self.restore_output = restore_output

    def session(self) -> MaskSession:
        return MaskSession(enabled=self.enabled, restore_output=self.restore_output)

    def detect(self, text: str) -> dict[str, int]:
        """Counts PII occurrences without producing tokens (for diagnostics)."""
        counts: Counter[str] = Counter()
        for kind, pattern in _PATTERNS:
            found = len(pattern.findall(text or ""))
            if found:
                counts[kind] = found
        return dict(counts)


def get_pii_shield(settings: Any) -> PIIShield:
    return PIIShield(
        enabled=bool(getattr(settings, "pii_enabled", False)),
        restore_output=bool(getattr(settings, "pii_restore_output", False)),
    )
