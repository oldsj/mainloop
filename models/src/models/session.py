"""Session models - unified model for all background work (with or without code)."""

import uuid
from datetime import datetime
from enum import Enum
from typing import Any, Literal

from pydantic import BaseModel, Field


def _uuid() -> str:
    return str(uuid.uuid4())


class SessionStatus(str, Enum):
    """Status of a session."""

    PENDING = "pending"
    ACTIVE = "active"
    COMPLETED = "completed"
    FAILED = "failed"
    CANCELLED = "cancelled"

    WAITING_ON_USER = "waiting_on_user"

    # Code work states
    IMPLEMENTING = "implementing"
    UNDER_REVIEW = "under_review"


class Session(BaseModel):
    """A background task that runs independently from the main thread.

    Sessions can be:
    - Simple Claude conversations (no repo_url)
    - Code work with GitHub integration (with repo_url)

    Both types have their own conversation and can notify the user when input is needed.
    """

    id: str = Field(default_factory=_uuid, description="Unique session ID")
    user_id: str = Field(..., description="User ID from Cloudflare Access")
    main_thread_id: str = Field(..., description="Parent main thread ID")

    # Session definition
    title: str = Field(..., description="Short title for the session")
    description: str = Field(..., description="What the session is for")
    prompt: str = Field(..., description="Initial prompt that started the session")

    # Conversation - each session has its own chat
    conversation_id: str = Field(..., description="Session's conversation ID")
    # Session tree (native children): set from the native binding when listing.
    parent_session_id: str | None = Field(
        None, description="Parent session (delegation)"
    )
    topic: str | None = Field(None, description="Topic this session works for")

    # Execution state
    status: SessionStatus = Field(
        default=SessionStatus.PENDING, description="Session status"
    )
    worker_pod_name: str | None = Field(
        None, description="K8s pod running this session"
    )

    # Timestamps
    created_at: datetime = Field(
        default_factory=datetime.utcnow, description="Creation timestamp"
    )
    started_at: datetime | None = Field(None, description="Start timestamp")
    completed_at: datetime | None = Field(None, description="Completion timestamp")
    archived_at: datetime | None = Field(
        None,
        description="When the session was cleared from the list; kept for audit, never deleted",
    )

    # Results
    summary: str | None = Field(
        None, description="Summary posted to main thread on completion"
    )
    error: str | None = Field(None, description="Error message if failed")

    # === Code work fields (optional - only used when repo_url is set) ===

    # Repository context
    repo_url: str | None = Field(None, description="GitHub repository URL")
    project_id: str | None = Field(None, description="Associated project ID")
    branch_name: str | None = Field(None, description="Branch to create/work on")
    base_branch: str = Field(default="main", description="Base branch")

    # Model selection
    model: str | None = Field(
        None, description="Claude model to use (haiku, sonnet, opus)"
    )

    # GitHub integration - Plan phase (issue)
    issue_url: str | None = Field(None, description="Plan issue URL")
    issue_number: int | None = Field(None, description="Plan issue number")
    issue_etag: str | None = Field(None, description="ETag for conditional polling")
    issue_last_modified: datetime | None = Field(
        None, description="Last-Modified for polling"
    )

    # GitHub integration - Implementation phase (PR)
    pr_url: str | None = Field(None, description="Implementation PR URL")
    pr_number: int | None = Field(None, description="PR number")
    pr_etag: str | None = Field(None, description="ETag for conditional PR polling")
    pr_last_modified: datetime | None = Field(
        None, description="Last-Modified for PR polling"
    )
    commit_sha: str | None = Field(None, description="Final commit SHA")

    # Inline thread anchoring
    anchor_message_id: str | None = Field(
        None, description="Main thread message ID that triggered this session"
    )
    color: str | None = Field(
        None, description="Assigned color for inline display (hex or name)"
    )

    # Additional result data
    result: dict[str, Any] | None = Field(None, description="Session result data")

    @property
    def has_repo(self) -> bool:
        """Returns True if this session involves code work."""
        return self.repo_url is not None

    @property
    def needs_attention(self) -> bool:
        """Returns True if this session is waiting on user input."""
        return self.status == SessionStatus.WAITING_ON_USER


class SessionCreate(BaseModel):
    """Request to create a new session."""

    title: str = Field(..., description="Short title for the session")
    description: str = Field(..., description="What the session is for")
    prompt: str = Field(..., description="Initial prompt for the session")

    # Optional: for code work
    repo_url: str | None = Field(
        None, description="GitHub repository URL for code work"
    )

    # Optional: for inline threading
    anchor_message_id: str | None = Field(
        None, description="Main thread message ID to anchor this session to"
    )

    # Optional: run a real native agent under Herdr in the workspace pod
    agent_kind: Literal["claude", "codex"] | None = Field(
        None, description="Native agent kind; omit for the existing session worker"
    )


class SessionNotification(BaseModel):
    """Ephemeral notification about a session needing attention."""

    id: str = Field(default_factory=_uuid, description="Unique notification ID")
    session_id: str = Field(..., description="Session that needs attention")
    user_id: str = Field(..., description="User to notify")
    title: str = Field(..., description="Notification title")
    preview: str = Field(..., description="Preview text")
    read: bool = Field(default=False, description="Whether notification was read")
    created_at: datetime = Field(
        default_factory=datetime.utcnow, description="Creation timestamp"
    )


class NativeDeliveryInfo(BaseModel):
    """Delivery ledger row for one user message sent to a native agent."""

    message_id: str
    state: str = Field(
        ...,
        description="queued|recorded|sending|delivered|completed|uncertain|failed",
    )
    evidence_ref: str | None = None
    detail: str | None = None
    source: str = "user"  # user | report | writeout | brief


class NativeSessionInfo(BaseModel):
    """Identity strip for a session bound to a native agent under Herdr."""

    session_id: str
    kind: Literal["claude", "codex"]
    role: str = "agent"  # agent | main | child
    parent_session_id: str | None = None
    topic: str | None = None
    agent_name: str
    native_session_id: str | None = None
    model: str | None = None
    approval_policy: str
    herdr_pane_id: str | None = None
    herdr_terminal_id: str | None = None
    herdr_workspace_id: str | None = None
    workspace_pod: str | None = None
    workspace_pod_uid: str | None = None
    workspace_ready: bool = False
    agent_live: bool | None = None
    generation: int = 1
    lineage_seq: int = 1
    context_tokens: int | None = None
    baseline_tokens: int | None = None
    turns_in_lineage: int = 0
    continuations: int = 0
    rotating: bool = False
    journal_cursor: int = 0
    journal_ref: str | None = None
    turn_in_flight: bool = False
    deliveries: list[NativeDeliveryInfo] = Field(default_factory=list)
    note: str | None = None
