package tasks

import (
	"errors"
	"testing"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

var (
	allStatuses = []dbstate.TaskStatus{
		dbstate.TaskQueued, dbstate.TaskRunning, dbstate.TaskWaiting, dbstate.TaskBlocked,
		dbstate.TaskCompleted, dbstate.TaskFailed, dbstate.TaskCancelled,
	}
	allReasons = []dbstate.TaskReason{
		"", dbstate.ReasonOwnerDecision, dbstate.ReasonAwaitingChild, dbstate.ReasonApproval, dbstate.ReasonCI,
		dbstate.ReasonPublication, dbstate.ReasonHandoff, dbstate.ReasonReconciliation,
		dbstate.ReasonProvisioningUnavailable, dbstate.ReasonHandoffUnavailable, dbstate.ReasonCancelUnavailable,
		dbstate.ReasonSessionLost, dbstate.ReasonStalled, dbstate.ReasonRuntimeLimit,
	}
	allAttemptStates = []dbstate.AttemptState{
		dbstate.AttemptCreating, dbstate.AttemptActive, dbstate.AttemptDraining, dbstate.AttemptFenced,
		dbstate.AttemptSuperseded, dbstate.AttemptFailed, dbstate.AttemptCancelled, dbstate.AttemptCompleted,
	}
	allOpStates = []dbstate.TaskOpState{
		dbstate.TaskOpRequested, dbstate.TaskOpDraining, dbstate.TaskOpCheckpointRequired,
		dbstate.TaskOpCheckpointVerified, dbstate.TaskOpSourceFencing, dbstate.TaskOpSourceFenced,
		dbstate.TaskOpTargetCreating, dbstate.TaskOpTargetReady, dbstate.TaskOpCompleted,
		dbstate.TaskOpBlocked, dbstate.TaskOpUncertain,
	}
)

func TestValidTaskStateTable(t *testing.T) {
	t.Parallel()
	valid := map[TaskState]bool{
		{dbstate.TaskQueued, ""}:                                    true,
		{dbstate.TaskQueued, dbstate.ReasonProvisioningUnavailable}: true,
		{dbstate.TaskRunning, ""}:                                   true,
		{dbstate.TaskWaiting, dbstate.ReasonAwaitingChild}:          true,
		{dbstate.TaskWaiting, dbstate.ReasonApproval}:               true,
		{dbstate.TaskWaiting, dbstate.ReasonCI}:                     true,
		{dbstate.TaskWaiting, dbstate.ReasonPublication}:            true,
		{dbstate.TaskWaiting, dbstate.ReasonHandoff}:                true,
		{dbstate.TaskWaiting, dbstate.ReasonReconciliation}:         true,
		{dbstate.TaskBlocked, dbstate.ReasonOwnerDecision}:          true,
		{dbstate.TaskBlocked, dbstate.ReasonReconciliation}:         true,
		{dbstate.TaskBlocked, dbstate.ReasonSessionLost}:            true,
		{dbstate.TaskBlocked, dbstate.ReasonStalled}:                true,
		{dbstate.TaskBlocked, dbstate.ReasonRuntimeLimit}:           true,
		{dbstate.TaskCompleted, ""}:                                 true,
		{dbstate.TaskFailed, ""}:                                    true,
		{dbstate.TaskCancelled, ""}:                                 true,
	}
	for _, st := range allStatuses {
		for _, r := range allReasons {
			s := TaskState{st, r}
			if got := ValidTaskState(s); got != valid[s] {
				t.Errorf("ValidTaskState(%s) = %v, want %v", s, got, valid[s])
			}
		}
	}
}

func TestTransitionTask(t *testing.T) {
	t.Parallel()
	allowed := map[dbstate.TaskStatus][]dbstate.TaskStatus{
		dbstate.TaskQueued:  {dbstate.TaskQueued, dbstate.TaskRunning, dbstate.TaskBlocked, dbstate.TaskFailed, dbstate.TaskCancelled},
		dbstate.TaskRunning: {dbstate.TaskRunning, dbstate.TaskWaiting, dbstate.TaskBlocked, dbstate.TaskCompleted, dbstate.TaskFailed, dbstate.TaskCancelled},
		dbstate.TaskWaiting: {dbstate.TaskWaiting, dbstate.TaskRunning, dbstate.TaskBlocked, dbstate.TaskCompleted, dbstate.TaskFailed, dbstate.TaskCancelled},
		dbstate.TaskBlocked: {dbstate.TaskBlocked, dbstate.TaskRunning, dbstate.TaskWaiting, dbstate.TaskCompleted, dbstate.TaskFailed, dbstate.TaskCancelled},
	}
	// One valid reason per status to exercise every (from, to) status pair.
	rep := map[dbstate.TaskStatus]dbstate.TaskReason{
		dbstate.TaskWaiting: dbstate.ReasonCI, dbstate.TaskBlocked: dbstate.ReasonReconciliation,
	}
	for _, from := range allStatuses {
		for _, to := range allStatuses {
			want := false
			for _, a := range allowed[from] {
				want = want || a == to
			}
			err := TransitionTask(TaskState{from, rep[from]}, TaskState{to, rep[to]})
			if (err == nil) != want {
				t.Errorf("%s -> %s: err = %v, want allowed = %v", from, to, err, want)
			}
			if err != nil && !errors.Is(err, ErrTransition) {
				t.Errorf("%s -> %s: error %v does not wrap ErrTransition", from, to, err)
			}
		}
	}
}

func TestTransitionTaskRefusesInvalidPairs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ from, to TaskState }{
		{TaskState{dbstate.TaskRunning, ""}, TaskState{dbstate.TaskWaiting, ""}},
		{TaskState{dbstate.TaskRunning, ""}, TaskState{dbstate.TaskBlocked, dbstate.ReasonCI}},
		{TaskState{dbstate.TaskRunning, ""}, TaskState{dbstate.TaskRunning, dbstate.ReasonCI}},
		{TaskState{dbstate.TaskRunning, dbstate.ReasonCI}, TaskState{dbstate.TaskRunning, ""}},
		{TaskState{dbstate.TaskRunning, ""}, TaskState{dbstate.TaskCancelled, dbstate.ReasonCancelUnavailable}},
	} {
		if err := TransitionTask(tc.from, tc.to); !errors.Is(err, ErrTransition) {
			t.Errorf("%s -> %s: err = %v, want ErrTransition", tc.from, tc.to, err)
		}
	}
}

func TestTransitionTaskReasonChangesWithinAStatus(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ from, to TaskState }{
		{TaskState{dbstate.TaskQueued, ""}, TaskState{dbstate.TaskQueued, dbstate.ReasonProvisioningUnavailable}},
		{TaskState{dbstate.TaskQueued, dbstate.ReasonProvisioningUnavailable}, TaskState{dbstate.TaskQueued, ""}},
		{TaskState{dbstate.TaskWaiting, dbstate.ReasonCI}, TaskState{dbstate.TaskWaiting, dbstate.ReasonPublication}},
		{TaskState{dbstate.TaskBlocked, dbstate.ReasonOwnerDecision}, TaskState{dbstate.TaskBlocked, dbstate.ReasonReconciliation}},
		{TaskState{dbstate.TaskBlocked, dbstate.ReasonOwnerDecision}, TaskState{dbstate.TaskRunning, ""}},
	} {
		if err := TransitionTask(tc.from, tc.to); err != nil {
			t.Errorf("%s -> %s: %v", tc.from, tc.to, err)
		}
	}
}

func TestTransitionAttempt(t *testing.T) {
	t.Parallel()
	allowed := map[dbstate.AttemptState][]dbstate.AttemptState{
		dbstate.AttemptCreating: {dbstate.AttemptActive, dbstate.AttemptDraining},
		dbstate.AttemptActive:   {dbstate.AttemptDraining},
		dbstate.AttemptDraining: {dbstate.AttemptFenced},
		dbstate.AttemptFenced: {
			dbstate.AttemptSuperseded, dbstate.AttemptFailed, dbstate.AttemptCancelled, dbstate.AttemptCompleted,
		},
	}
	for _, from := range allAttemptStates {
		for _, to := range allAttemptStates {
			want := false
			for _, a := range allowed[from] {
				want = want || a == to
			}
			err := TransitionAttempt(from, to)
			if (err == nil) != want {
				t.Errorf("%s -> %s: err = %v, want allowed = %v", from, to, err, want)
			}
			if err != nil && !errors.Is(err, ErrTransition) {
				t.Errorf("%s -> %s: error %v does not wrap ErrTransition", from, to, err)
			}
		}
	}
}

func TestAttemptClassification(t *testing.T) {
	t.Parallel()
	live := map[dbstate.AttemptState]bool{dbstate.AttemptCreating: true, dbstate.AttemptActive: true}
	unfenced := map[dbstate.AttemptState]bool{dbstate.AttemptCreating: true, dbstate.AttemptActive: true, dbstate.AttemptDraining: true}
	final := map[dbstate.AttemptState]bool{
		dbstate.AttemptSuperseded: true, dbstate.AttemptFailed: true, dbstate.AttemptCancelled: true, dbstate.AttemptCompleted: true,
	}
	for _, s := range allAttemptStates {
		if AttemptLive(s) != live[s] || AttemptUnfenced(s) != unfenced[s] || AttemptFinal(s) != final[s] {
			t.Errorf("%s: live/unfenced/final = %v/%v/%v", s, AttemptLive(s), AttemptUnfenced(s), AttemptFinal(s))
		}
	}
}

// forward lists, per kind, the moves an operation may make without blocking or uncertainty.
var forward = map[dbstate.TaskOpKind]map[dbstate.TaskOpState][]dbstate.TaskOpState{
	dbstate.TaskOpCreate: {
		dbstate.TaskOpRequested:      {dbstate.TaskOpTargetCreating},
		dbstate.TaskOpTargetCreating: {dbstate.TaskOpTargetReady},
		dbstate.TaskOpTargetReady:    {dbstate.TaskOpCompleted},
	},
	dbstate.TaskOpCancel: {
		dbstate.TaskOpRequested:     {dbstate.TaskOpDraining, dbstate.TaskOpCompleted},
		dbstate.TaskOpDraining:      {dbstate.TaskOpSourceFencing, dbstate.TaskOpCompleted},
		dbstate.TaskOpSourceFencing: {dbstate.TaskOpSourceFenced},
		dbstate.TaskOpSourceFenced:  {dbstate.TaskOpCompleted},
	},
	dbstate.TaskOpRetry: {
		dbstate.TaskOpRequested:          {dbstate.TaskOpDraining},
		dbstate.TaskOpDraining:           {dbstate.TaskOpCheckpointRequired},
		dbstate.TaskOpCheckpointRequired: {dbstate.TaskOpCheckpointVerified},
		dbstate.TaskOpCheckpointVerified: {dbstate.TaskOpSourceFencing},
		dbstate.TaskOpSourceFencing:      {dbstate.TaskOpSourceFenced},
		dbstate.TaskOpSourceFenced:       {dbstate.TaskOpTargetCreating},
		dbstate.TaskOpTargetCreating:     {dbstate.TaskOpTargetReady},
		dbstate.TaskOpTargetReady:        {dbstate.TaskOpCompleted},
	},
}

func contains(list []dbstate.TaskOpState, s dbstate.TaskOpState) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestTransitionOperation(t *testing.T) {
	t.Parallel()
	forward[dbstate.TaskOpReassign] = forward[dbstate.TaskOpRetry]
	for kind, edges := range forward {
		for _, from := range allOpStates {
			for _, to := range allOpStates {
				var want bool
				switch {
				case OpFinal(from):
					want = false
				case to == dbstate.TaskOpBlocked:
					want = true
				case to == dbstate.TaskOpUncertain:
					want = from != dbstate.TaskOpUncertain
				case from == dbstate.TaskOpUncertain:
					want = false // covered by TestUncertainOperationResumes
				default:
					want = contains(edges[from], to)
				}
				if from == dbstate.TaskOpUncertain && to != dbstate.TaskOpBlocked && to != dbstate.TaskOpUncertain {
					continue
				}
				err := TransitionOperation(kind, from, from, to)
				if (err == nil) != want {
					t.Errorf("%s %s -> %s: err = %v, want allowed = %v", kind, from, to, err, want)
				}
				if err != nil && !errors.Is(err, ErrTransition) {
					t.Errorf("%s %s -> %s: error %v does not wrap ErrTransition", kind, from, to, err)
				}
			}
		}
	}
}

func TestUncertainOperationResumes(t *testing.T) {
	t.Parallel()
	for kind, edges := range forward {
		for last := range edges {
			for _, to := range allOpStates {
				if to == dbstate.TaskOpUncertain {
					continue
				}
				want := to == dbstate.TaskOpBlocked || contains(edges[last], to)
				err := TransitionOperation(kind, dbstate.TaskOpUncertain, last, to)
				if (err == nil) != want {
					t.Errorf("%s uncertain(after %s) -> %s: err = %v, want allowed = %v", kind, last, to, err, want)
				}
			}
		}
	}
}

func TestTransitionOperationUnknownKind(t *testing.T) {
	t.Parallel()
	if err := TransitionOperation("nope", dbstate.TaskOpRequested, dbstate.TaskOpRequested, dbstate.TaskOpBlocked); !errors.Is(err, ErrTransition) {
		t.Fatalf("err = %v", err)
	}
}
