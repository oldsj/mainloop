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
	ReasonHandoffUnavailable      TaskReason = "handoff_unavailable"
	ReasonCancelUnavailable       TaskReason = "cancel_unavailable"
	// ReasonSessionLost, ReasonStalled and ReasonRuntimeLimit are the liveness sweep's bounds on a
	// running task: its attempt's kagent session is gone or terminal, it has shown no activity for
	// TASK_STALL_TIMEOUT, or it has run longer than TASK_MAX_RUNTIME. See internal/tasks/liveness.go.
	ReasonSessionLost  TaskReason = "session_lost"
	ReasonStalled      TaskReason = "stalled"
	ReasonRuntimeLimit TaskReason = "runtime_limit"

	// ReasonIdle is a project's standing supervisor at rest: waiting for its next message after a
	// completed report. Only a standing task takes it (docs/specs/project-supervisor.md).
	ReasonIdle TaskReason = "idle"
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

// LoopState is task_loops.state. Armed and paused (by the owner: not evaluated for its trigger)
// are live; the rest are final: done (a one-time timer fired), exhausted (max firings made, or
// stopped for no progress), expired, stopped (by its recipient or the owner) and ended (its
// recipient's task ended, or its subject is gone).
type LoopState string

// Loop states.
const (
	LoopArmed     LoopState = "armed"
	LoopPaused    LoopState = "paused"
	LoopDone      LoopState = "done"
	LoopExhausted LoopState = "exhausted"
	LoopExpired   LoopState = "expired"
	LoopStopped   LoopState = "stopped"
	LoopEnded     LoopState = "ended"
)

// LoopTrigger is task_loops.trigger: what makes a loop fire.
type LoopTrigger string

// Loop triggers.
const (
	LoopTriggerTask        LoopTrigger = "task"
	LoopTriggerTime        LoopTrigger = "time"
	LoopTriggerPullRequest LoopTrigger = "pull_request"
	LoopTriggerQuiet       LoopTrigger = "quiet"
)

// LoopStopNoProgress is task_loops.stop_reason and task_loop_firings.stops_loop: a loop armed with
// expect_change stopped because three firings in a row found its subject unchanged.
const LoopStopNoProgress = "no_progress"

// PRCondition is an element of task_loops.pr_conditions: what a pull_request trigger waits for.
type PRCondition string

// Pull request conditions. CI counts only when its result is for the pull request's head.
const (
	PRCISuccess PRCondition = "ci_success"
	PRCIFailure PRCondition = "ci_failure"
	PRMerged    PRCondition = "merged"
	PRClosed    PRCondition = "closed"
)

// PRConditions lists the pull request conditions, in the order the contract lists them.
var PRConditions = []PRCondition{PRCISuccess, PRCIFailure, PRMerged, PRClosed}

// Capacity tiers of a task's last admission (tasks.capacity_tier; docs/specs/tasks.md#capacity).
const (
	CapacityTierBase     = "base"
	CapacityTierOverflow = "overflow"
)

// Capacity refusal reasons counted in task_capacity_refusals; the same strings as the task error
// reasons child_capacity and global_capacity.
const (
	CapacityRefusalChild  = "child_capacity"
	CapacityRefusalGlobal = "global_capacity"
)
