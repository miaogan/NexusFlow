from __future__ import annotations

import logging
import threading
from collections.abc import Callable
from concurrent.futures import ThreadPoolExecutor
from typing import Any

from nexusflow_agents.core.executor import HttpExecutor
from nexusflow_agents.core.registry import AgentRegistry

logger = logging.getLogger(__name__)


class BackgroundPool:
    """Bounded background pool for best-effort work such as memory extraction."""

    def __init__(self, max_workers: int = 8, max_pending: int = 64) -> None:
        self._executor = ThreadPoolExecutor(
            max_workers=max(1, max_workers),
            thread_name_prefix="agent-hub-bg",
        )
        self._slots = threading.BoundedSemaphore(max(1, max_pending))
        self._closed = False

    def submit(self, fn: Callable[..., Any], *args: Any, **kwargs: Any) -> bool:
        if self._closed or not self._slots.acquire(blocking=False):
            return False

        def run() -> None:
            try:
                fn(*args, **kwargs)
            except Exception as exc:  # noqa: BLE001 - background work is best-effort
                logger.warning("background agent task failed: %s", exc)
            finally:
                self._slots.release()

        self._executor.submit(run)
        return True

    def shutdown(self, wait: bool = False) -> None:
        self._closed = True
        self._executor.shutdown(wait=wait, cancel_futures=True)


class AgentLifecycle:
    """Periodically prunes expired registrations and probes HTTP agents."""

    def __init__(
        self,
        registry: AgentRegistry,
        executor: HttpExecutor,
        *,
        interval_seconds: float = 10.0,
        fail_threshold: int = 3,
    ) -> None:
        self._registry = registry
        self._executor = executor
        self._interval = max(1.0, interval_seconds)
        self._fail_threshold = max(1, fail_threshold)
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None

    def start(self) -> None:
        if self._thread is not None and self._thread.is_alive():
            return
        self._stop.clear()
        self._thread = threading.Thread(
            target=self._run, name="agent-hub-health", daemon=True
        )
        self._thread.start()

    def stop(self) -> None:
        self._stop.set()
        if self._thread is not None:
            self._thread.join(timeout=2.0)
            self._thread = None

    def _run(self) -> None:
        while not self._stop.wait(self._interval):
            self.tick()

    def tick(self) -> None:
        self._registry.prune_expired()
        for spec in self._registry.by_kind("http"):
            if self._executor.probe(spec):
                self._registry.mark_success(spec.name)
            else:
                self._registry.mark_failure(spec.name, self._fail_threshold)
