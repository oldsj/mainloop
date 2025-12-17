"""Shared Pydantic models for mainloop."""

from models.conversation import Conversation, Message
from models.agent import AgentTask, AgentResponse

__all__ = [
    "Conversation",
    "Message",
    "AgentTask",
    "AgentResponse",
]
