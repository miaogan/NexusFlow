from __future__ import annotations

from fastapi import APIRouter, Depends

from nexusflow_agents.agents.intent.deps import get_graph
from nexusflow_agents.agents.intent.graph import IntentGraph
from nexusflow_agents.agents.intent.schemas import IntentRequest, IntentResult

router = APIRouter(prefix="/v1", tags=["intent"])


@router.post("/intent", response_model=IntentResult)
def classify(
    req: IntentRequest,
    graph: IntentGraph = Depends(get_graph),
) -> IntentResult:
    history = [item.model_dump() for item in req.history]
    return IntentResult(**graph.invoke(req.message, history))
