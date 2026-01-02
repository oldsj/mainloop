"""API-specific response models."""

from pydantic import BaseModel, Field

from models import Conversation, Message


class ConversationResponse(BaseModel):
    """Response model for conversation with messages."""

    conversation: Conversation
    messages: list[Message] = Field(default_factory=list)


class ConversationListResponse(BaseModel):
    """Response model for list of conversations."""

    conversations: list[Conversation]
    total: int


class ChatRequest(BaseModel):
    """Request model for sending a message."""

    message: str = Field(..., description="User message")
    conversation_id: str | None = Field(None, description="Existing conversation ID")


class ChatResponse(BaseModel):
    """Response model for chat interaction."""

    conversation_id: str
    message: Message
