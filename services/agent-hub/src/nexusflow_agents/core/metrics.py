from __future__ import annotations

from prometheus_client import Counter, Histogram

HUB_CALLS = Counter(
    "agent_hub_calls_total",
    "Agent hub calls by capability, agent and status",
    ["capability", "agent", "status"],
)
HUB_LATENCY = Histogram(
    "agent_hub_call_seconds",
    "Agent hub call latency by capability and agent",
    ["capability", "agent"],
    buckets=(0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0),
)
HUB_DEGRADED = Counter(
    "agent_hub_degraded_total",
    "Calls that exhausted all local/remote agents",
    ["capability"],
)
HUB_BACKGROUND_DROPPED = Counter(
    "agent_hub_background_dropped_total",
    "Background tasks dropped because the pool was full",
    ["capability"],
)
