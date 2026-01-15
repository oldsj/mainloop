"""Shared Pydantic models for mainloop."""

from models.agent import AgentResponse, AgentTask
from models.conversation import Conversation, Message
from models.session import (
    Session,
    SessionCreate,
    SessionNotification,
    SessionStatus,
)
from models.workflow import (
    EventTypes,
    GitHubPR,
    GitHubRepo,
    MainThread,
    Project,
    QueueItem,
    QueueItemPriority,
    QueueItemResponse,
    QueueItemType,
    WorkflowEvent,
)

__all__ = [
    # Existing
    "Conversation",
    "Message",
    "AgentTask",
    "AgentResponse",
    # Session models
    "Session",
    "SessionCreate",
    "SessionNotification",
    "SessionStatus",
    # Workflow models
    "MainThread",
    "QueueItem",
    "QueueItemResponse",
    "QueueItemType",
    "QueueItemPriority",
    "WorkflowEvent",
    "EventTypes",
    "GitHubRepo",
    "GitHubPR",
    "Project",
]
