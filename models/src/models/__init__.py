"""Shared Pydantic models for mainloop."""

from models.agent import AgentResponse, AgentTask
from models.conversation import Conversation, Message
from models.workflow import (
    EventTypes,
    GitHubPR,
    GitHubRepo,
    MainThread,
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
    "TaskQuestion",
    "QuestionOption",
    "QueueItem",
    "QueueItemResponse",
    "QueueItemType",
    "QueueItemPriority",
    "WorkflowEvent",
    "EventTypes",
    "GitHubRepo",
    "GitHubPR",
    "Project",
    # Planning models
    "PlanningSession",
    "PlanningSessionStatus",
]
