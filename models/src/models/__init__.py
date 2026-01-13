"""Shared Pydantic models for mainloop."""

from models.agent import AgentResponse, AgentTask
from models.conversation import Conversation, Message
from models.session import (
    QuestionOption,
    Session,
    SessionCreate,
    SessionNotification,
    SessionQuestion,
    SessionStatus,
)

# Backward compatibility aliases (deprecated - use Session instead)
# These will be removed once migration is complete
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
    # Session models (unified)
    "Session",
    "SessionCreate",
    "SessionNotification",
    "SessionStatus",
    "SessionQuestion",
    "QuestionOption",
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
    # Backward compatibility (deprecated)
    "WorkerTask",
    "WorkerTaskCreate",
    "TaskStatus",
    "TaskQuestion",
]
