"""Shared Pydantic models for mainloop."""

from models.conversation import Conversation, Message
from models.agent import AgentTask, AgentResponse
from models.workflow import (
    MainThread,
    WorkerTask,
    WorkerTaskCreate,
    TaskStatus,
    QueueItem,
    QueueItemResponse,
    QueueItemType,
    QueueItemPriority,
    WorkflowEvent,
    EventTypes,
    GitHubRepo,
    GitHubPR,
)

__all__ = [
    # Existing
    "Conversation",
    "Message",
    "AgentTask",
    "AgentResponse",
    # Workflow models
    "MainThread",
    "WorkerTask",
    "WorkerTaskCreate",
    "TaskStatus",
    "QueueItem",
    "QueueItemResponse",
    "QueueItemType",
    "QueueItemPriority",
    "WorkflowEvent",
    "EventTypes",
    "GitHubRepo",
    "GitHubPR",
]
