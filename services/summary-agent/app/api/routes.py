from __future__ import annotations

from fastapi import APIRouter, Depends

from app.deps import get_graph
from app.graph import SummaryGraph
from app.schemas import SummaryRequest, SummaryResult

router = APIRouter(prefix="/v1", tags=["summary"])


@router.post("/summarize", response_model=SummaryResult)
def summarize(
    req: SummaryRequest,
    graph: SummaryGraph = Depends(get_graph),
) -> SummaryResult:
    messages = [item.model_dump() for item in req.messages]
    return SummaryResult(
        **graph.invoke(
            previous_summary=req.previous_summary,
            messages=messages,
            budget_chars=req.budget_chars,
        )
    )
