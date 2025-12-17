"""Claude agent task and response models."""

from datetime import datetime
from typing import Any
from pydantic import BaseModel, Field


class AgentTask(BaseModel):
    """A task for the Claude agent."""

    id: str = Field(..., description="Unique task ID")
    conversation_id: str = Field(..., description="Parent conversation ID")
    prompt: str = Field(..., description="Task prompt for Claude")
    context: dict[str, Any] | None = Field(None, description="Additional context")
    created_at: datetime = Field(default_factory=datetime.now, description="Creation timestamp")


class AgentResponse(BaseModel):
    """Response from the Claude agent."""

    task_id: str = Field(..., description="Parent task ID")
    content: str = Field(..., description="Response content")
    tool_uses: list[dict[str, Any]] | None = Field(None, description="Tool uses during execution")
    created_at: datetime = Field(default_factory=datetime.now, description="Creation timestamp")
