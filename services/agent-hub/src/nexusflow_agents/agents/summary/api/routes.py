from __future__ import annotations

from fastapi import APIRouter, Depends, Request

from nexusflow_agents.agents.summary.deps import get_graph
from nexusflow_agents.agents.summary.graph import SummaryGraph
from nexusflow_agents.agents.summary.schemas import SummaryRequest, SummaryResult

router = APIRouter(prefix="/v1", tags=["summary"])


@router.post("/summarize", response_model=SummaryResult)
def summarize(
    req: SummaryRequest,
    request: Request,
    graph: SummaryGraph = Depends(get_graph),
) -> SummaryResult:
    messages = [item.model_dump() for item in req.messages]
    tenant_id = request.headers.get("x-tenant-id") or ""
    return SummaryResult(
        **graph.invoke(
            previous_summary=req.previous_summary,
            messages=messages,
            budget_chars=req.budget_chars,
            tenant_id=tenant_id,
        )
    )
