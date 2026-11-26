import json
import os
import time

from app.schemas import FeedbackEvent


class FeedbackStore:
    """Append-only feedback sink. Swap for Postgres/Kafka in production; the
    on-disk JSONL keeps the closed loop demoable without extra services."""

    def __init__(self, path: str) -> None:
        self.path = path

    def append(self, event: FeedbackEvent) -> None:
        record = event.model_dump()
        if not record.get("ts"):
            record["ts"] = int(time.time())
        parent = os.path.dirname(self.path)
        if parent:
            os.makedirs(parent, exist_ok=True)
        with open(self.path, "a", encoding="utf-8") as fh:
            fh.write(json.dumps(record, ensure_ascii=False) + "\n")
