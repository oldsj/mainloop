"""Durable workflow models for absurd-based orchestration."""

import uuid
from datetime import datetime
from enum import Enum
from typing import Any, Literal

from pydantic import BaseModel, Field


def _uuid() -> str:
    return str(uuid.uuid4())


class QueueItemType(str, Enum):
    """Types of items in the human queue."""

    QUESTION = "question"
    APPROVAL = "approval"
    REVIEW = "review"
    ERROR = "error"
    NOTIFICATION = "notification"
    # Plan-first workflow types
    PLAN_READY = "plan_ready"  # Legacy: Plan ready as GitHub issue
    PLAN_REVIEW = (
        "plan_review"  # Interactive plan review in inbox (with options + text input)
    )
    CODE_READY = "code_ready"  # Code is ready for review
    FEEDBACK_ADDRESSED = "feedback_addressed"  # Worker addressed feedback
    ROUTING_SUGGESTION = "routing_suggestion"  # Suggesting to route to existing task


class QueueItemPriority(str, Enum):
    """Priority levels for queue items."""

    URGENT = "urgent"
    HIGH = "high"
    NORMAL = "normal"
    LOW = "low"


class MainThread(BaseModel):
    """Represents a user's main conversation thread (durable workflow)."""

    id: str = Field(default_factory=_uuid, description="Unique thread ID")
    user_id: str = Field(..., description="User ID from Cloudflare Access")
    workflow_run_id: str | None = Field(None, description="Absurd workflow run ID")
    status: Literal["active", "paused", "error"] = Field(
        default="active", description="Thread status"
    )
    created_at: datetime = Field(
        default_factory=datetime.utcnow, description="Creation timestamp"
    )
    last_activity_at: datetime = Field(
        default_factory=datetime.utcnow, description="Last activity timestamp"
    )
    active_tasks: list[str] = Field(
        default_factory=list, description="IDs of active worker tasks"
    )
    context: dict[str, Any] = Field(
        default_factory=dict, description="Accumulated context/memory"
    )


class QueueItem(BaseModel):
    """An item in the human review queue."""

    id: str = Field(default_factory=_uuid, description="Unique item ID")
    main_thread_id: str = Field(..., description="Parent main thread ID")
    task_id: str | None = Field(None, description="Related worker task ID")
    user_id: str = Field(..., description="User ID")

    # Item details
    item_type: QueueItemType = Field(..., description="Type of queue item")
    priority: QueueItemPriority = Field(
        default=QueueItemPriority.NORMAL, description="Priority level"
    )
    title: str = Field(..., description="Short title/summary")
    content: str = Field(..., description="Full content/question")

    # Context for responding
    context: dict[str, Any] = Field(default_factory=dict, description="Extra context")
    options: list[str] | None = Field(
        None, description="Predefined response options if any"
    )

    # State
    status: Literal["pending", "responded", "expired", "cancelled"] = Field(
        default="pending", description="Item status"
    )
    response: str | None = Field(None, description="Human response")
    responded_at: datetime | None = Field(None, description="Response timestamp")
    read_at: datetime | None = Field(
        None, description="When item was read/acknowledged"
    )

    # Timestamps
    created_at: datetime = Field(
        default_factory=datetime.utcnow, description="Creation timestamp"
    )
    expires_at: datetime | None = Field(None, description="When this item expires")


class QueueItemResponse(BaseModel):
    """Human response to a queue item."""

    response: str = Field(..., description="Response text")
    metadata: dict[str, Any] = Field(
        default_factory=dict, description="Optional metadata"
    )


class WorkflowEvent(BaseModel):
    """Event for communication between workflows."""

    id: str = Field(default_factory=_uuid, description="Unique event ID")
    event_type: str = Field(..., description="Event type for routing")
    source_workflow_id: str = Field(..., description="Workflow that emitted this event")
    target_workflow_id: str | None = Field(
        None, description="Target workflow if specific"
    )
    payload: dict[str, Any] = Field(default_factory=dict, description="Event payload")
    created_at: datetime = Field(
        default_factory=datetime.utcnow, description="Creation timestamp"
    )


class EventTypes:
    """Standard event types for workflow communication."""

    # User -> Main Thread
    USER_MESSAGE = "user.message"
    USER_QUEUE_RESPONSE = "user.queue_response"

    # Main Thread -> Worker
    WORKER_SPAWN = "worker.spawn"
    WORKER_CANCEL = "worker.cancel"

    # Worker -> Main Thread
    WORKER_STARTED = "worker.started"
    WORKER_PROGRESS = "worker.progress"
    WORKER_QUESTION = "worker.question"
    WORKER_COMPLETED = "worker.completed"
    WORKER_FAILED = "worker.failed"

    # Main Thread -> Human Queue
    QUEUE_ITEM_ADDED = "queue.item_added"
    QUEUE_ITEM_RESPONSE = "queue.item_response"


class GitHubRepo(BaseModel):
    """GitHub repository reference."""

    owner: str = Field(..., description="Repository owner")
    name: str = Field(..., description="Repository name")
    default_branch: str = Field(default="main", description="Default branch")

    @property
    def full_name(self) -> str:
        return f"{self.owner}/{self.name}"

    @property
    def clone_url(self) -> str:
        return f"https://github.com/{self.owner}/{self.name}.git"


class GitHubPR(BaseModel):
    """GitHub pull request reference."""

    number: int = Field(..., description="PR number")
    url: str = Field(..., description="PR URL")
    title: str = Field(..., description="PR title")
    state: Literal["open", "closed", "merged"] = Field(..., description="PR state")
    head_branch: str = Field(..., description="Head branch name")
    base_branch: str = Field(..., description="Base branch name")


class Project(BaseModel):
    """A GitHub repository project tracked by the user."""

    id: str = Field(default_factory=_uuid, description="Unique project ID")
    user_id: str = Field(..., description="User who owns this project")

    # GitHub identifiers
    owner: str = Field(..., description="GitHub owner/org")
    name: str = Field(..., description="Repository name")
    full_name: str = Field(..., description="owner/name")

    # GitHub metadata (cached)
    description: str | None = Field(None, description="Repo description")
    default_branch: str = Field(default="main", description="Default branch")
    avatar_url: str | None = Field(None, description="Owner avatar URL")
    html_url: str = Field(..., description="GitHub repo URL")

    # Tracking
    created_at: datetime = Field(
        default_factory=datetime.utcnow, description="Creation timestamp"
    )
    last_used_at: datetime = Field(
        default_factory=datetime.utcnow, description="Last used timestamp"
    )
    metadata_updated_at: datetime | None = Field(
        None, description="When GitHub data was refreshed"
    )

    # Stats (cached, refreshed periodically)
    open_pr_count: int = Field(default=0, description="Cached open PR count")
    open_issue_count: int = Field(default=0, description="Cached open issue count")
