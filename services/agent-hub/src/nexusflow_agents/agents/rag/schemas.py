from pydantic import BaseModel, Field


class ChatRequest(BaseModel):
    message: str = Field(min_length=1, max_length=8000)
    conversation_id: str | None = None
    top_k: int | None = Field(default=None, ge=1, le=50)
    kb_id: str | None = None
    filters: dict[str, str] | None = None


class Citation(BaseModel):
    chunk_id: str
    content: str
    score: float = 0.0
    meta: dict[str, str] = Field(default_factory=dict)


class ChatResponse(BaseModel):
    conversation_id: str
    message_id: str
    answer: str
    citations: list[Citation] = Field(default_factory=list)
    trace_id: str | None = None
    created_at: str


class ConversationSummary(BaseModel):
    id: str
    title: str
    message_count: int
    created_at: str
    updated_at: str


class ConversationListResponse(BaseModel):
    conversations: list[ConversationSummary] = Field(default_factory=list)
    total: int
    limit: int
    offset: int


class MessageOut(BaseModel):
    id: str
    role: str
    content: str
    citations: list[Citation] = Field(default_factory=list)
    created_at: str


class ConversationDetail(BaseModel):
    conversation: ConversationSummary
    messages: list[MessageOut] = Field(default_factory=list)
