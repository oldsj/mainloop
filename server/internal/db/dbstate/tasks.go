package dbstate

// TaskMode is tasks.mode.
type TaskMode string

// Task modes.
const (
	TaskModeCode         TaskMode = "code"
	TaskModeCoordination TaskMode = "coordination"
)

// TaskSelection is tasks.selection_source: where the provider profile came from.
type TaskSelection string

// Task selection sources.
const (
	SelectionExplicit            TaskSelection = "explicit"
	SelectionProjectDefault      TaskSelection = "project_default"
	SelectionInstallationDefault TaskSelection = "installation_default"
	SelectionInherited           TaskSelection = "inherited"
)

// TaskStatus is tasks.status.
type TaskStatus string

// Task statuses.
const (
	TaskQueued    TaskStatus = "queued"
	TaskRunning   TaskStatus = "running"
	TaskWaiting   TaskStatus = "waiting"
	TaskBlocked   TaskStatus = "blocked"
	TaskCompleted TaskStatus = "completed"
	TaskFailed    TaskStatus = "failed"
	TaskCancelled TaskStatus = "cancelled"
)

// TaskReason is tasks.reason and task_operations.reason: why a task is waiting or blocked, or why
// an operation cannot continue.
type TaskReason string

// Task reasons.
const (
	ReasonOwnerDecision           TaskReason = "owner_decision"
	ReasonAwaitingChild           TaskReason = "awaiting_child"
	ReasonApproval                TaskReason = "approval"
	ReasonCI                      TaskReason = "ci"
	ReasonPublication             TaskReason = "publication"
	ReasonHandoff                 TaskReason = "handoff"
	ReasonReconciliation          TaskReason = "reconciliation"
	ReasonProvisioningUnavailable TaskReason = "provisioning_unavailable"
	ReasonCapacityQueued          TaskReason = "capacity_queued"
	ReasonHandoffUnavailable      TaskReason = "handoff_unavailable"
	ReasonCancelUnavailable       TaskReason = "cancel_unavailable"
)

// AttemptState is task_attempts.state.
type AttemptState string

// Attempt states.
const (
	AttemptCreating   AttemptState = "creating"
	AttemptActive     AttemptState = "active"
	AttemptDraining   AttemptState = "draining"
	AttemptFenced     AttemptState = "fenced"
	AttemptSuperseded AttemptState = "superseded"
	AttemptFailed     AttemptState = "failed"
	AttemptCancelled  AttemptState = "cancelled"
	AttemptCompleted  AttemptState = "completed"
)

// AttemptRole is task_attempts.role.
type AttemptRole string

// Attempt roles.
const (
	AttemptSupervisor AttemptRole = "supervisor"
	AttemptChild      AttemptRole = "child"
)

// NativeProvider is task_attempts.native_provider.
type NativeProvider string

// Native providers.
const (
	ProviderClaude NativeProvider = "claude"
	ProviderCodex  NativeProvider = "codex"
)

// TaskOpKind is task_operations.kind.
type TaskOpKind string

// Task operation kinds.
const (
	TaskOpCreate   TaskOpKind = "create"
	TaskOpRetry    TaskOpKind = "retry"
	TaskOpReassign TaskOpKind = "reassign"
	TaskOpCancel   TaskOpKind = "cancel"
)

// TaskOpState is task_operations.state and task_operations.last_confirmed_step.
type TaskOpState string

// Task operation states.
const (
	TaskOpRequested          TaskOpState = "requested"
	TaskOpDraining           TaskOpState = "draining"
	TaskOpCheckpointRequired TaskOpState = "checkpoint_required"
	TaskOpCheckpointVerified TaskOpState = "checkpoint_verified"
	TaskOpSourceFencing      TaskOpState = "source_fencing"
	TaskOpSourceFenced       TaskOpState = "source_fenced"
	TaskOpTargetCreating     TaskOpState = "target_creating"
	TaskOpTargetReady        TaskOpState = "target_ready"
	TaskOpCompleted          TaskOpState = "completed"
	TaskOpBlocked            TaskOpState = "blocked"
	TaskOpUncertain          TaskOpState = "uncertain"
)

// TaskArtifactKind is task_artifacts.kind.
type TaskArtifactKind string

// Task artifact kinds.
const (
	ArtifactCheckpoint       TaskArtifactKind = "checkpoint"
	ArtifactHandoffManifest  TaskArtifactKind = "handoff_manifest"
	ArtifactProviderSummary  TaskArtifactKind = "unverified_provider_summary"
	ArtifactRetentionReceipt TaskArtifactKind = "retention_receipt"
)

// ReportOutcome is task_reports.outcome.
type ReportOutcome string

// Report outcomes.
const (
	OutcomeProgress  ReportOutcome = "progress"
	OutcomeCompleted ReportOutcome = "completed"
	OutcomeFailed    ReportOutcome = "failed"
	OutcomeBlocked   ReportOutcome = "blocked"
)

// TaskDeliveryState is task_event_deliveries.state.
type TaskDeliveryState string

// Task event delivery states.
const (
	TaskDeliveryPending   TaskDeliveryState = "pending"
	TaskDeliveryQueued    TaskDeliveryState = "queued"
	TaskDeliveryDelivered TaskDeliveryState = "delivered"
	TaskDeliveryUncertain TaskDeliveryState = "uncertain"
	TaskDeliveryCancelled TaskDeliveryState = "cancelled"
)
