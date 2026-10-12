package sessions

import (
	"context"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// ErrorDelay is the wait before a task is read again after a failed read, for a delivery that has
// been claimed attempts times.
func ErrorDelay(o Options, uncertain bool, attempts int) time.Duration {
	t := &turn{s: &Service{Opts: o}, c: reconcile.Claim{Attempts: attempts}, cur: dbstate.DeliveryDelivered}
	if uncertain {
		t.cur = dbstate.DeliveryUncertain
	}
	return t.errorDelay()
}

// ParkedDelay is the wait before a task parked for an answer is read again.
func ParkedDelay(o Options, attempts int) time.Duration {
	t := &turn{s: &Service{Opts: o}, c: reconcile.Claim{Attempts: attempts}}
	return t.parkedDelay()
}

// AssistantID is the id of the mirrored reply of a task.
func AssistantID(sessionID, taskID string) string { return assistantID(sessionID, a2a.TaskID(taskID)) }

// Handle runs the delivery handler for a claim of the delivery with the given id and state.
func Handle(s *Service, ctx context.Context, id, state string) (reconcile.Outcome, error) {
	return s.handle(ctx, reconcile.Claim{ID: id, State: state})
}
