from __future__ import annotations

from datetime import UTC, datetime

import pytest

from nexusflow_agents.agents.rag.services.approvals import (
    ApprovalConflict,
    ApprovalNotFound,
    ApprovalService,
)
from nexusflow_agents.core.audit import (
    AuditLog,
    _month_start,
    _next_month,
    _partition_month,
    _partition_name,
    get_audit,
)
from nexusflow_agents.core.tooling import ToolRegistry, ToolService, ToolSpec


def test_approval_lifecycle_and_single_decision() -> None:
    service = ApprovalService(ttl_seconds=60)
    record = service.create(
        "sandbox.execute", {"code": "print(1)"}, "execute",
        {"tenant_id": "t1", "user_id": "u1", "trace_id": "tr"},
    )

    assert [item["id"] for item in service.list(status="pending")] == [record.id]
    assert service.is_approved(record.id, "sandbox.execute") is False

    approved = service.decide(
        record.id, "approve", decided_by="admin", tenant_id="t1"
    )
    assert approved.status == "approved"
    assert service.is_approved(record.id, "sandbox.execute") is True
    assert service.is_approved(record.id, "workflow.run") is False

    with pytest.raises(ApprovalConflict):
        service.decide(record.id, "reject", decided_by="admin")


def test_approval_tenant_scope_and_expiry() -> None:
    service = ApprovalService(ttl_seconds=60)
    record = service.create("tool", {}, "write", {"tenant_id": "t1"})

    with pytest.raises(ApprovalNotFound):
        service.decide(record.id, "approve", decided_by="x", tenant_id="t2")

    short = ApprovalService(ttl_seconds=60)
    expired = short.create("tool", {}, "write", {}, ttl_seconds=-1)
    assert short.get(expired.id) is None


def test_tool_calls_write_audit_events() -> None:
    audit = get_audit()
    audit.clear()
    tools = ToolService(ToolRegistry())
    tools.register(
        ToolSpec(name="demo.echo", description="echo"),
        lambda payload, ctx: {"ok": True},
    )

    tools.call("demo.echo", {}, {"tenant_id": "t9", "trace_id": "tr-9"})

    events = audit.list(tenant_id="t9")
    assert events[0]["action"] == "tool.call"
    assert events[0]["decision"] == "allowed"
    assert events[0]["resource"] == "demo.echo"
    assert events[0]["trace_id"] == "tr-9"


def test_audit_list_supports_time_range_filters() -> None:
    audit = AuditLog()
    event = audit.record("pii.mask", tenant_id="t1")
    assert audit.list(tenant_id="t1", since=event.ts - 1, until=event.ts + 1)
    assert audit.list(tenant_id="t1", since=event.ts + 1) == []
    assert audit.list(tenant_id="t1", until=event.ts - 1) == []


def test_audit_epoch_accepts_unix_and_rfc3339() -> None:
    from nexusflow_agents.api.audit import _epoch

    assert _epoch("") == 0.0
    assert _epoch("100") == 100.0
    assert _epoch("2026-01-01T00:00:00Z") == 1767225600.0


def test_audit_partition_month_boundaries() -> None:
    december = datetime(2026, 12, 31, 23, 59, 59, tzinfo=UTC)

    assert _month_start(december) == datetime(2026, 12, 1, tzinfo=UTC)
    assert _next_month(december) == datetime(2027, 1, 1, tzinfo=UTC)
    assert _partition_name(december) == "audit_logs_p202612"


def test_audit_partition_name_roundtrip_and_rejects_noise() -> None:
    assert _partition_month("audit_logs_p202601") == datetime(2026, 1, 1, tzinfo=UTC)
    assert _partition_month("audit_logs_p202613") is None
    assert _partition_month("audit_logs_old") is None
    assert _partition_month("other_p202601") is None
