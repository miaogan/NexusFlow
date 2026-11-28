from pydantic import BaseModel, Field


class ChatMessage(BaseModel):
    role: str = Field(pattern="^(system|user|assistant)$")
    content: str = Field(max_length=8000)


class ChatRequest(BaseModel):
    message: str = Field(min_length=1, max_length=8000)
    conversation_id: str | None = None
    top_k: int | None = Field(default=None, ge=1, le=50)
    kb_id: str | None = None
    filters: dict[str, str] | None = None
    # Approved high-risk tool call to replay after /v1/approvals/{id}/decide.
    approval_id: str | None = Field(default=None, max_length=64)
    # Stateless callers (OpenAI-compatible endpoint) send their own history and
    # opt out of conversation persistence.
    history: list[ChatMessage] | None = Field(default=None, max_length=50)
    stateless: bool = False
    # OpenAI-compatible generation controls. system_prompt is appended to the
    # built-in assistant prompt; sampling values are forwarded to the model.
    system_prompt: str | None = Field(default=None, max_length=8000)
    temperature: float | None = Field(default=None, ge=0, le=2)
    max_tokens: int | None = Field(default=None, ge=1, le=32768)


class Citation(BaseModel):
    chunk_id: str
    content: str
    score: float = 0.0
    meta: dict[str, str] = Field(default_factory=dict)


class ChatUsage(BaseModel):
    prompt_tokens: int = 0
    completion_tokens: int = 0
    total_tokens: int = 0


class ChatResponse(BaseModel):
    conversation_id: str
    message_id: str
    answer: str
    citations: list[Citation] = Field(default_factory=list)
    trace_id: str | None = None
    created_at: str
    usage: ChatUsage | None = None


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
