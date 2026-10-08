"""Durable task contracts. Caller inputs never include runtime authority."""

from datetime import datetime
from typing import Annotated, Literal

from pydantic import Field, model_validator

from models.native_agent import ContractModel
from models.provider import ProviderAgentRef, ProviderProfileId
from models.workspace import WorkspaceEnvironment

Identifier = Annotated[str, Field(strict=True, min_length=1, max_length=100)]
EvidenceRef = Annotated[str, Field(strict=True, min_length=1, max_length=2048)]
TaskStatus = Literal[
    "queued", "running", "waiting", "blocked", "completed", "failed", "cancelled"
]
AttemptState = Literal[
    "creating",
    "active",
    "draining",
    "fenced",
    "superseded",
    "failed",
    "cancelled",
    "completed",
]
TaskReason = Literal[
    "awaiting_child",
    "approval",
    "ci",
    "publication",
    "handoff",
    "reconciliation",
    "provisioning_unavailable",
    "handoff_unavailable",
    "cancel_unavailable",
]
OperationState = Literal[
    "requested",
    "draining",
    "checkpoint_required",
    "checkpoint_verified",
    "source_fencing",
    "source_fenced",
    "target_creating",
    "target_ready",
    "completed",
    "blocked",
    "uncertain",
]


class TaskCheckout(ContractModel):
    branch: Annotated[str, Field(strict=True, min_length=1, max_length=255)]
    # Empty selects the remote's default branch, matching the workspace contract.
    ref: Annotated[str, Field(strict=True, max_length=255)] = ""
    depth: Annotated[int, Field(strict=True, ge=0, le=1000)] = 1

    @model_validator(mode="after")
    def valid_refs(self):
        # Ref expressions are never accepted as branch names.
        for value in (self.branch, self.ref):
            if (
                value.startswith(("-", "/", "refs/"))
                or value.endswith(("/", "."))
                or value == "@"
                or any(c.isspace() or ord(c) < 32 or ord(c) == 127 for c in value)
                or any(c in value for c in "~^:?*[\\")
                or any(s in value for s in ("..", "@{", "//"))
                or any(
                    p.startswith(".") or p.endswith(".lock") for p in value.split("/")
                )
            ):
                raise ValueError("invalid checkout branch/ref")
        return self


class TaskCreate(ContractModel):
    request_id: Identifier
    title: Annotated[str, Field(strict=True, min_length=1, max_length=200)]
    brief: Annotated[str, Field(strict=True, min_length=1, max_length=16384)]
    mode: Literal["code", "coordination"]
    project_id: Identifier | None = None
    topic_id: Identifier | None = None
    provider_profile_id: ProviderProfileId | None = None
    checkout: TaskCheckout | None = None

    @model_validator(mode="after")
    def project_required(self):
        if self.mode == "code" and (self.project_id is None or self.checkout is None):
            raise ValueError("code tasks require project_id and checkout")
        if self.mode == "coordination" and self.checkout is not None:
            raise ValueError("coordination tasks have no repository authority")
        if len(self.brief.encode()) > 16384:
            raise ValueError("brief exceeds 16 KiB")
        return self


class TaskAction(ContractModel):
    request_id: Identifier
    expected_version: Annotated[int, Field(strict=True, ge=1)]
    expected_attempt_id: Identifier | None


class TaskReassign(TaskAction):
    target_profile_id: ProviderProfileId


class TaskReport(ContractModel):
    task_id: Identifier
    attempt_id: Identifier
    request_id: Identifier
    summary: Annotated[str, Field(strict=True, min_length=1, max_length=4000)]
    outcome: Literal["progress", "completed", "failed", "blocked"]
    evidence_refs: Annotated[tuple[EvidenceRef, ...], Field(max_length=64)] = ()


class TaskLookup(ContractModel):
    task_id: Identifier


class TaskList(ContractModel):
    project_id: Identifier | None = None
    parent_task_id: Identifier | None = None


class TaskScopedAction(TaskAction):
    task_id: Identifier


class TaskScopedReassign(TaskReassign):
    task_id: Identifier


class Task(ContractModel):
    id: Identifier
    owner_id: Identifier
    project_id: Identifier | None = None
    topic_id: Identifier | None = None
    parent_task_id: Identifier | None = None
    root_task_id: Identifier
    creator_binding_id: Identifier | None = None
    title: str
    brief: str
    mode: Literal["code", "coordination"]
    assigned_profile_id: ProviderProfileId
    selection_source: Literal[
        "explicit", "project_default", "installation_default", "inherited"
    ]
    provider_constraint: ProviderProfileId | None = None
    accepted_environment: WorkspaceEnvironment | None = None
    status: TaskStatus = "queued"
    reason: TaskReason | None = None
    current_attempt_id: Identifier | None = None
    version: Annotated[int, Field(strict=True, ge=1)] = 1
    checkout: TaskCheckout | None = None
    created_at: datetime
    updated_at: datetime

    @model_validator(mode="after")
    def hierarchy(self):
        if self.parent_task_id is None and self.root_task_id != self.id:
            raise ValueError("a root task references itself")
        if self.parent_task_id == self.id:
            raise ValueError("a task cannot parent itself")
        if self.mode == "code" and (self.project_id is None or self.checkout is None):
            raise ValueError("code task requires project and checkout")
        if self.mode == "coordination" and self.checkout is not None:
            raise ValueError("coordination task cannot carry checkout")
        return self


class TaskAttempt(ContractModel):
    id: Identifier
    task_id: Identifier
    number: Annotated[int, Field(strict=True, ge=1)]
    profile_id: ProviderProfileId
    native_provider: Literal["claude", "codex"]
    configuration_revision: str
    agent_ref: ProviderAgentRef
    role: Literal["supervisor", "child"]
    depth: Literal[1, 2]
    writer_generation: Annotated[int, Field(strict=True, ge=1)] | None = None
    session_id: Identifier | None = None
    binding_id: Identifier | None = None
    workspace_id: Identifier | None = None
    state: AttemptState = "creating"
    brief_delivery_id: Identifier | None = None
    predecessor_id: Identifier | None = None
    successor_id: Identifier | None = None
    evidence_refs: Annotated[tuple[EvidenceRef, ...], Field(max_length=64)] = ()
    result_ref: EvidenceRef | None = None
    environment: WorkspaceEnvironment | None = None
    initial_ref: str | None = None
    checkpoint_ref: EvidenceRef | None = None
    manifest_ref: EvidenceRef | None = None
    superseded_at: datetime | None = None
    archived_at: datetime | None = None
    native_deleted_at: datetime | None = None
    retention_hold: str | None = None
    created_at: datetime
    updated_at: datetime

    @model_validator(mode="after")
    def role_depth(self):
        if (self.role, self.depth) not in (("supervisor", 1), ("child", 2)):
            raise ValueError("invalid delegated role/depth")
        return self


class TaskOperation(ContractModel):
    id: Identifier
    owner_id: Identifier
    principal_key: str
    request_id: Identifier
    request_digest: str
    request_payload: dict = Field(default_factory=dict)
    kind: Literal["create", "retry", "reassign", "cancel"]
    task_id: Identifier | None = None
    attempt_id: Identifier | None = None
    state: OperationState = "requested"
    last_confirmed_step: OperationState = "requested"
    source_attempt_id: Identifier | None = None
    target_attempt_id: Identifier | None = None
    checkpoint_ref: EvidenceRef | None = None
    manifest_ref: EvidenceRef | None = None
    reason: TaskReason | None = None
    created_at: datetime
    updated_at: datetime


class TaskEligibility(ContractModel):
    available: bool = False
    reason: TaskReason | None = None

    @model_validator(mode="after")
    def unavailable_reason(self):
        if not self.available and self.reason is None:
            raise ValueError("unavailable actions require a reason")
        return self


class TaskProjection(ContractModel):
    # These facts are observations, never execution authority or approval receipts.
    agent_activity: str | None = None
    delivery_state: str | None = None
    workspace_health: str | None = None
    environment_version_id: str | None = None
    repository: str | None = None
    branch: str | None = None
    pr_url: str | None = None
    pr_number: int | None = None
    pr_head_sha: str | None = None
    pr_state: Literal["open", "closed", "merged", "unknown"] = "unknown"
    ci_state: Literal["pending", "success", "failure", "unknown"] = "unknown"
    ci_head_sha: str | None = None
    merge_state: str | None = None
    merge_proposal_id: str | None = None
    publication_state: str | None = None
    pending_approval_ids: tuple[str, ...] = ()
    observed_at: datetime | None = None


class TaskArtifact(ContractModel):
    id: Identifier
    operation_id: Identifier
    kind: Literal[
        "checkpoint",
        "handoff_manifest",
        "unverified_provider_summary",
        "retention_receipt",
    ]
    sha256: str
    payload: dict


class TaskView(ContractModel):
    task: Task
    attempts: tuple[TaskAttempt, ...] = ()
    operations: tuple[TaskOperation, ...] = ()
    artifacts: tuple[TaskArtifact, ...] = ()
    reports: tuple[TaskReport, ...] = ()
    projection: TaskProjection = Field(default_factory=TaskProjection)
    actions: dict[Literal["retry", "reassign", "cancel"], TaskEligibility]


class TaskUpdated(ContractModel):
    type: Literal["task:updated"] = "task:updated"
    event_id: Identifier
    task_id: Identifier
    version: int
    attempt_id: Identifier | None
    root_task_id: Identifier
    parent_task_id: Identifier | None
    occurred_at: datetime


class ProjectProviderUpdate(ContractModel):
    profile_id: ProviderProfileId | None
    expected_version: Annotated[int, Field(strict=True, ge=0)]


class ProjectProviderPreference(ContractModel):
    project_id: Identifier
    profile_id: ProviderProfileId | None = None
    version: int = 0
