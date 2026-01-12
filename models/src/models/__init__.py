"""Shared Pydantic models for mainloop."""

from models.agent import AgentResponse, AgentTask
from models.conversation import Conversation, Message
from models.workflow import (
    EventTypes,
    GitHubPR,
    GitHubRepo,
    MainThread,
    Notification,
    NotificationType,
    PlanningSession,
    PlanningSessionStatus,
    Project,
    QuestionOption,
    QueueItem,
    QueueItemPriority,
    QueueItemResponse,
    QueueItemType,
    TaskQuestion,
    TaskStatus,
    WorkerTask,
    WorkerTaskCreate,
    WorkflowEvent,
)

__all__ = [
    # Core
    "Conversation",
    "Message",
    "AgentTask",
    "AgentResponse",
    # The Loop (meta thread + tasks)
    "MainThread",
    "WorkerTask",
    "WorkerTaskCreate",
    "TaskStatus",
    "TaskQuestion",
    "QuestionOption",
    # Notifications (simple read/clear)
    "Notification",
    "NotificationType",
    # Queue (legacy, may deprecate)
    "QueueItem",
    "QueueItemResponse",
    "QueueItemType",
    "QueueItemPriority",
    # Workflow events
    "WorkflowEvent",
    "EventTypes",
    # GitHub
    "GitHubRepo",
    "GitHubPR",
    "Project",
    # Planning
    "PlanningSession",
    "PlanningSessionStatus",
]
