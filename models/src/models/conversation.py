"""Conversation and message models."""

from datetime import datetime
from typing import Literal
from pydantic import BaseModel, Field


class Message(BaseModel):
    """A single message in a conversation."""

    id: str = Field(..., description="Unique message ID")
    conversation_id: str = Field(..., description="Parent conversation ID")
    role: Literal["user", "assistant"] = Field(..., description="Message role")
    content: str = Field(..., description="Message content")
    created_at: datetime = Field(default_factory=datetime.now, description="Creation timestamp")


class Conversation(BaseModel):
    """A conversation thread."""

    id: str = Field(..., description="Unique conversation ID")
    user_id: str = Field(..., description="User ID from Cloudflare Access")
    title: str | None = Field(None, description="Conversation title")
    summary: str | None = Field(None, description="Compacted summary of older messages")
    summarized_through_id: str | None = Field(None, description="ID of last message included in summary")
    message_count: int = Field(0, description="Total message count for compaction threshold")
    created_at: datetime = Field(default_factory=datetime.now, description="Creation timestamp")
    updated_at: datetime = Field(default_factory=datetime.now, description="Last update timestamp")
