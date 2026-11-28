from __future__ import annotations

from fastapi import APIRouter, Depends, Request

from nexusflow_agents.agents.intent.deps import get_graph
from nexusflow_agents.agents.intent.graph import IntentGraph
from nexusflow_agents.agents.intent.schemas import IntentRequest, IntentResult

router = APIRouter(prefix="/v1", tags=["intent"])


@router.post("/intent", response_model=IntentResult)
def classify(
    req: IntentRequest,
    request: Request,
    graph: IntentGraph = Depends(get_graph),
) -> IntentResult:
    history = [item.model_dump() for item in req.history]
    tenant_id = request.headers.get("x-tenant-id") or ""
    return IntentResult(
        **graph.invoke(req.message, history, req.skills, tenant_id=tenant_id)
    )
