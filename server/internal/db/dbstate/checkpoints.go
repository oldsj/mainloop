package dbstate

// CheckpointState is session_checkpoints.state: pending until kagent confirms the checkpoint or
// definitely refuses it, then done either way.
type CheckpointState string

// Checkpoint states.
const (
	CheckpointPending CheckpointState = "pending"
	CheckpointDone    CheckpointState = "done"
)

// ForkState is checkpoint_forks.state: requested until kagent's ForkSession is confirmed
// (accepted), then ready once the forked runtime is observed READY. failed is a definite refusal.
type ForkState string

// Fork states.
const (
	ForkRequested ForkState = "requested"
	ForkAccepted  ForkState = "accepted"
	ForkReady     ForkState = "ready"
	ForkFailed    ForkState = "failed"
)

// FailureReason is session_checkpoints.failure_reason and checkpoint_forks.failure_reason: a
// definite kagent refusal, mapped to the API's status (see server/internal/sessions/checkpoints.go).
type FailureReason string

// Failure reasons.
const (
	FailureInvalidArgument    FailureReason = "invalid_argument"
	FailureNotFound           FailureReason = "not_found"
	FailureFailedPrecondition FailureReason = "failed_precondition"
)
