"""Durable workflow models for absurd-based orchestration."""

from datetime import datetime
from enum import Enum
from typing import Any, Literal
from pydantic import BaseModel, Field
import uuid


def _uuid() -> str:
    return str(uuid.uuid4())


class TaskStatus(str, Enum):
    """Status of a worker task."""

    PENDING = "pending"
    PLANNING = "planning"  # Creating implementation plan
    WAITING_PLAN_REVIEW = "waiting_plan_review"  # Plan needs approval
    IMPLEMENTING = "implementing"  # Writing code per approved plan
    WAITING_HUMAN = "waiting_human"  # Code review
    COMPLETED = "completed"
    FAILED = "failed"
    CANCELLED = "cancelled"


class QueueItemType(str, Enum):
    """Types of items in the human queue."""

    QUESTION = "question"
    APPROVAL = "approval"
    REVIEW = "review"
    ERROR = "error"
    NOTIFICATION = "notification"
    # New types for plan-first workflow
    PLAN_READY = "plan_ready"  # Plan is ready for review
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


class WorkerTask(BaseModel):
    """A task assigned to a worker agent."""

    id: str = Field(default_factory=_uuid, description="Unique task ID")
    main_thread_id: str = Field(..., description="Parent main thread ID")
    user_id: str = Field(..., description="User ID")

    # Task definition
    task_type: str = Field(
        ..., description="Type of task: feature, bugfix, review, etc."
    )
    description: str = Field(..., description="Human-readable task description")
    prompt: str = Field(..., description="Full prompt for Claude")
    model: str | None = Field(None, description="Claude model to use (haiku, sonnet, opus)")

    # Repository context
    repo_url: str | None = Field(None, description="GitHub repository URL")
    branch_name: str | None = Field(None, description="Branch to create/work on")
    base_branch: str = Field(default="main", description="Base branch")

    # Execution state
    status: TaskStatus = Field(default=TaskStatus.PENDING, description="Task status")
    workflow_run_id: str | None = Field(
        None, description="Absurd workflow run ID for this task"
    )
    worker_pod_name: str | None = Field(
        None, description="K8s pod running this task"
    )

    # Timestamps
    created_at: datetime = Field(
        default_factory=datetime.utcnow, description="Creation timestamp"
    )
    started_at: datetime | None = Field(None, description="Start timestamp")
    completed_at: datetime | None = Field(None, description="Completion timestamp")

    # Results
    result: dict[str, Any] | None = Field(None, description="Task result data")
    error: str | None = Field(None, description="Error message if failed")

    # GitHub integration
    pr_url: str | None = Field(None, description="Created PR URL")
    pr_number: int | None = Field(None, description="PR number")
    commit_sha: str | None = Field(None, description="Final commit SHA")

    # Conversation linking (for routing)
    conversation_id: str | None = Field(None, description="Originating conversation ID")
    message_id: str | None = Field(None, description="Originating message ID")
    keywords: list[str] = Field(default_factory=list, description="Keywords for task routing")
    skip_plan: bool = Field(default=False, description="Skip plan phase if user said 'just do it'")


class WorkerTaskCreate(BaseModel):
    """Request to create a new worker task."""

    task_type: str = Field(..., description="Type of task")
    description: str = Field(..., description="Task description")
    repo_url: str | None = Field(None, description="Repository URL")
    base_branch: str = Field(default="main", description="Base branch")
    model: str | None = Field(None, description="Claude model (haiku, sonnet, opus)")
    context: dict[str, Any] = Field(
        default_factory=dict, description="Additional context"
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
    read_at: datetime | None = Field(None, description="When item was read/acknowledged")

    # Timestamps
    created_at: datetime = Field(
        default_factory=datetime.utcnow, description="Creation timestamp"
    )
    expires_at: datetime | None = Field(
        None, description="When this item expires"
    )


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
    source_workflow_id: str = Field(
        ..., description="Workflow that emitted this event"
    )
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
