// Package tasks is the task domain: delegated work that Mainloop tracks apart from the native
// sessions that run it. It owns the task, attempt and operation state machines, the REST and MCP
// entry points and the durable intent rows behind them.
//
// This slice records tasks, operations and reports and answers cancel. Creating the workspace and
// native session for an attempt, handoff, capacity and the reconcile loop are not here; an
// operation that needs them waits in its last confirmed state.
package tasks

import (
	"errors"
	"fmt"
	"slices"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

// ErrTransition is returned by every transition function that refuses a move.
var ErrTransition = errors.New("tasks: transition not allowed")

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrTransition, fmt.Sprintf(format, args...))
}

// TaskState is a task's status together with the reason that qualifies it. The reason is empty
// when the status has none.
type TaskState struct {
	Status dbstate.TaskStatus
	Reason dbstate.TaskReason
}

func (s TaskState) String() string {
	if s.Reason == "" {
		return string(s.Status)
	}
	return string(s.Status) + "/" + string(s.Reason)
}

// taskReasons lists the reasons each status accepts. A nil entry means the status takes none, and
// the empty reason is listed where it is also allowed.
var taskReasons = map[dbstate.TaskStatus][]dbstate.TaskReason{
	dbstate.TaskQueued:    {"", dbstate.ReasonProvisioningUnavailable, dbstate.ReasonCapacityQueued},
	dbstate.TaskRunning:   {""},
	dbstate.TaskWaiting:   {dbstate.ReasonAwaitingChild, dbstate.ReasonApproval, dbstate.ReasonCI, dbstate.ReasonPublication, dbstate.ReasonHandoff, dbstate.ReasonReconciliation},
	dbstate.TaskBlocked:   {dbstate.ReasonOwnerDecision, dbstate.ReasonReconciliation},
	dbstate.TaskCompleted: {""},
	dbstate.TaskFailed:    {""},
	dbstate.TaskCancelled: {""},
}

// ValidTaskState reports whether the status and reason go together. It is the same table as the
// tasks_status_reason constraint, and a test holds the two to each other.
func ValidTaskState(s TaskState) bool {
	return slices.Contains(taskReasons[s.Status], s.Reason)
}

// taskEdges are the status moves. A move to the same status is a change of reason and is allowed
// for every status that is not final.
var taskEdges = map[dbstate.TaskStatus][]dbstate.TaskStatus{
	dbstate.TaskQueued:  {dbstate.TaskRunning, dbstate.TaskBlocked, dbstate.TaskFailed, dbstate.TaskCancelled},
	dbstate.TaskRunning: {dbstate.TaskWaiting, dbstate.TaskBlocked, dbstate.TaskCompleted, dbstate.TaskFailed, dbstate.TaskCancelled},
	dbstate.TaskWaiting: {dbstate.TaskRunning, dbstate.TaskBlocked, dbstate.TaskCompleted, dbstate.TaskFailed, dbstate.TaskCancelled},
	dbstate.TaskBlocked: {dbstate.TaskRunning, dbstate.TaskWaiting, dbstate.TaskCompleted, dbstate.TaskFailed, dbstate.TaskCancelled},
}

// TaskFinal reports whether a task status can no longer change.
func TaskFinal(s dbstate.TaskStatus) bool {
	switch s {
	case dbstate.TaskCompleted, dbstate.TaskFailed, dbstate.TaskCancelled:
		return true
	}
	return false
}

// TransitionTask checks a task move. The target must be a valid status and reason pair, and the
// status must follow an edge or stay the same while the task is not final.
func TransitionTask(from, to TaskState) error {
	if !ValidTaskState(from) {
		return refuse("task state %s is not valid", from)
	}
	if !ValidTaskState(to) {
		return refuse("task state %s is not valid", to)
	}
	if TaskFinal(from.Status) {
		return refuse("task is %s", from.Status)
	}
	if from.Status == to.Status || slices.Contains(taskEdges[from.Status], to.Status) {
		return nil
	}
	return refuse("task cannot move from %s to %s", from.Status, to.Status)
}

// AttemptLive reports whether an attempt is being created or is running. Only an active attempt
// is let into MCP: a creating attempt's agent has not been started yet.
func AttemptLive(s dbstate.AttemptState) bool {
	return s == dbstate.AttemptCreating || s == dbstate.AttemptActive
}

// AttemptUnfenced reports whether an attempt could still be writing. The database allows one such
// attempt per task.
func AttemptUnfenced(s dbstate.AttemptState) bool {
	return AttemptLive(s) || s == dbstate.AttemptDraining
}

// AttemptFinal reports whether an attempt state can no longer change.
func AttemptFinal(s dbstate.AttemptState) bool {
	switch s {
	case dbstate.AttemptSuperseded, dbstate.AttemptFailed, dbstate.AttemptCancelled, dbstate.AttemptCompleted:
		return true
	}
	return false
}

// attemptEdges are the moves an attempt can make. Settlement is the only way out of the unfenced
// states: an attempt is fenced first, so a successor is never admitted while the old runtime may
// still write.
var attemptEdges = map[dbstate.AttemptState][]dbstate.AttemptState{
	dbstate.AttemptCreating: {dbstate.AttemptActive, dbstate.AttemptDraining},
	dbstate.AttemptActive:   {dbstate.AttemptDraining},
	dbstate.AttemptDraining: {dbstate.AttemptFenced},
	dbstate.AttemptFenced: {
		dbstate.AttemptSuperseded, dbstate.AttemptFailed, dbstate.AttemptCancelled, dbstate.AttemptCompleted,
	},
}

// TransitionAttempt checks an attempt move. There is no move out of a final state and none to the
// same state.
func TransitionAttempt(from, to dbstate.AttemptState) error {
	if slices.Contains(attemptEdges[from], to) {
		return nil
	}
	if AttemptFinal(from) {
		return refuse("attempt is %s", from)
	}
	return refuse("attempt cannot move from %s to %s", from, to)
}

// OpFinal reports whether an operation state can no longer change.
func OpFinal(s dbstate.TaskOpState) bool {
	return s == dbstate.TaskOpCompleted || s == dbstate.TaskOpBlocked
}

// opEdges are the forward moves of each operation kind. Any state that is not final can also move
// to blocked or uncertain, and an uncertain operation resumes from its last confirmed step; both
// are handled in [TransitionOperation].
var opEdges = map[dbstate.TaskOpKind]map[dbstate.TaskOpState][]dbstate.TaskOpState{
	dbstate.TaskOpCreate: {
		dbstate.TaskOpRequested:      {dbstate.TaskOpTargetCreating},
		dbstate.TaskOpTargetCreating: {dbstate.TaskOpTargetReady},
		dbstate.TaskOpTargetReady:    {dbstate.TaskOpCompleted},
	},
	// A cancel with no attempt completes from requested. With an attempt it drains, then fences.
	dbstate.TaskOpCancel: {
		dbstate.TaskOpRequested:     {dbstate.TaskOpDraining, dbstate.TaskOpCompleted},
		dbstate.TaskOpDraining:      {dbstate.TaskOpSourceFencing, dbstate.TaskOpCompleted},
		dbstate.TaskOpSourceFencing: {dbstate.TaskOpSourceFenced},
		dbstate.TaskOpSourceFenced:  {dbstate.TaskOpCompleted},
	},
	dbstate.TaskOpRetry:    handoffEdges,
	dbstate.TaskOpReassign: handoffEdges,
}

var handoffEdges = map[dbstate.TaskOpState][]dbstate.TaskOpState{
	dbstate.TaskOpRequested:          {dbstate.TaskOpDraining},
	dbstate.TaskOpDraining:           {dbstate.TaskOpCheckpointRequired},
	dbstate.TaskOpCheckpointRequired: {dbstate.TaskOpCheckpointVerified},
	dbstate.TaskOpCheckpointVerified: {dbstate.TaskOpSourceFencing},
	dbstate.TaskOpSourceFencing:      {dbstate.TaskOpSourceFenced},
	dbstate.TaskOpSourceFenced:       {dbstate.TaskOpTargetCreating},
	dbstate.TaskOpTargetCreating:     {dbstate.TaskOpTargetReady},
	dbstate.TaskOpTargetReady:        {dbstate.TaskOpCompleted},
}

// TransitionOperation checks an operation move. last is the last step the operation confirmed:
// an uncertain operation resumes from there, never from a step it did not reach.
func TransitionOperation(kind dbstate.TaskOpKind, from, last, to dbstate.TaskOpState) error {
	edges, ok := opEdges[kind]
	if !ok {
		return refuse("unknown operation kind %q", kind)
	}
	if OpFinal(from) {
		return refuse("operation is %s", from)
	}
	switch to {
	case dbstate.TaskOpBlocked:
		return nil
	case dbstate.TaskOpUncertain:
		if from == dbstate.TaskOpUncertain {
			return refuse("operation is already uncertain")
		}
		return nil
	}
	if from == dbstate.TaskOpUncertain {
		if slices.Contains(edges[last], to) {
			return nil
		}
		return refuse("%s operation cannot resume from %s to %s", kind, last, to)
	}
	if slices.Contains(edges[from], to) {
		return nil
	}
	return refuse("%s operation cannot move from %s to %s", kind, from, to)
}
