from __future__ import annotations

import os
import uuid

import pytest

from nexusflow_agents.agents.rag.services.approvals import (
    ApprovalConflict,
    PostgresApprovalService,
)
from nexusflow_agents.core.audit import PostgresAuditLog

DSN = os.environ.get("AGENT_HUB_TEST_DATABASE_URL", "")

pytestmark = pytest.mark.skipif(
    not DSN,
    reason="set AGENT_HUB_TEST_DATABASE_URL to run Postgres state integration tests",
)


def test_postgres_approvals_are_shared_between_replicas() -> None:
    first = PostgresApprovalService(DSN)
    second = PostgresApprovalService(DSN)  # simulates another pod
    tenant = "t-test-" + uuid.uuid4().hex[:8]
    record = first.create(
        "sandbox.execute",
        {"code": "print(1)"},
        "execute",
        {"tenant_id": tenant, "user_id": "u1"},
        ttl_seconds=60,
    )
    try:
        assert second.get(record.id) is not None
        assert [item["id"] for item in second.list(status="pending", tenant_id=tenant)] == [record.id]

        decided = second.decide(record.id, "approve", decided_by="reviewer", tenant_id=tenant)
        assert decided.status == "approved"
        assert first.is_approved(record.id, "sandbox.execute") is True

        with pytest.raises(ApprovalConflict):
            first.decide(record.id, "reject", decided_by="reviewer", tenant_id=tenant)
    finally:
        with first._connect() as conn:
            conn.execute("DELETE FROM agent_approvals WHERE id = %s", (record.id,))


def test_postgres_audit_trail_is_queryable() -> None:
    audit = PostgresAuditLog(DSN)
    tenant = "t-audit-" + uuid.uuid4().hex[:8]
    event = audit.record(
        "tool.call",
        tenant_id=tenant,
        user_id="u1",
        trace_id="tr-1",
        resource="demo.echo",
        risk="write",
        decision="allowed",
        detail={"ok": True},
    )
    try:
        events = audit.list(tenant_id=tenant, action="tool.call")
        assert events and events[0]["id"] == event.id
        assert events[0]["resource"] == "demo.echo"
        assert events[0]["decision"] == "allowed"
        assert events[0]["detail"] == {"ok": True}
    finally:
        with audit._connect() as conn:
            conn.execute("DELETE FROM audit_logs WHERE id = %s", (event.id,))
