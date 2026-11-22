package tasks

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// defaultOverflowWait is how long a task given an overflow slot may wait for runtime capacity.
const defaultOverflowWait = 15 * time.Minute

// runtimeWaitBatch bounds one pass of the runtime-wait sweep.
const runtimeWaitBatch = 100

// evidenceNoRuntimeCapacity prefixes the attempt evidence of a task the runtime-wait sweep failed.
const evidenceNoRuntimeCapacity = "provision-aborted: no runtime capacity within "

func (s *Service) overflowWait() time.Duration {
	return cmp.Or(s.Limits.OverflowWait, defaultOverflowWait)
}

// RuntimeWaitSweep ends the waits of tasks given an overflow slot (docs/specs/tasks.md#capacity).
// Such a task is admitted above the base global limit, where the cluster may have no free worker yet
// for its runtime; while it waits (tasks.runtime_wait_since), a send kagent does not accept is put
// back and tried again rather than failed (sessions.turn.awaitRuntimeCapacity). The wait ends when
// kagent has accepted a message for the task's current attempt: the runtime was placed. A wait that
// runs past the window fails the task and stops its attempt, which releases the slot once the stop is
// confirmed. No lock is held across kagent: the sweep reads only the ledger.
func (s *Service) RuntimeWaitSweep() reconcile.Sweep {
	return reconcile.Sweep{Name: "task-runtime-wait", Every: s.Limits.Interval, Run: s.runRuntimeWaits}
}

func (s *Service) runRuntimeWaits(ctx context.Context) error {
	rows, err := dbq.New(s.Pool).ListRuntimeWaits(ctx, dbq.ListRuntimeWaitsParams{
		WindowSeconds: s.overflowWait().Seconds(), RowLimit: runtimeWaitBatch,
	})
	if err != nil {
		return err
	}
	var errs []error
	for _, w := range rows {
		switch {
		case w.Accepted:
			err = s.endRuntimeWait(ctx, w)
		case w.Expired:
			err = s.expireRuntimeWait(ctx, w)
		default:
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("task %s: %w", w.ID, err))
		}
	}
	return errors.Join(errs...)
}

// endRuntimeWait clears a wait whose runtime kagent has placed, if it is still the same wait.
func (s *Service) endRuntimeWait(ctx context.Context, w dbq.ListRuntimeWaitsRow) error {
	return s.tx(ctx, func(q *dbq.Queries) ([]events.Draft, error) {
		since := w.WaitSince
		t, err := q.ClearTaskRuntimeWait(ctx, dbq.ClearTaskRuntimeWaitParams{ID: w.ID, RuntimeWaitSince: &since})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // ended or restarted since the sweep read it
		}
		if err != nil {
			return nil, err
		}
		return []events.Draft{s.updated(t)}, nil
	})
}

// expireRuntimeWait fails a task that waited for runtime capacity past the window and stops its
// current attempt, under the task lock. The trigger on tasks clears the wait with the status. The
// attempt's stop is then settled like any other; its slot is released once kagent confirms it.
func (s *Service) expireRuntimeWait(ctx context.Context, w dbq.ListRuntimeWaitsRow) error {
	var attemptID string
	err := s.tx(ctx, func(q *dbq.Queries) ([]events.Draft, error) {
		t, err := q.GetTaskByIDForUpdate(ctx, w.ID)
		if err != nil {
			return nil, err
		}
		if t.RuntimeWaitSince == nil || !t.RuntimeWaitSince.Equal(w.WaitSince) || TaskFinal(t.Status) {
			return nil, nil // ended or restarted since the sweep read it
		}
		from := TaskState{Status: t.Status}
		if t.Reason != nil {
			from.Reason = *t.Reason
		}
		if err := TransitionTask(from, TaskState{Status: dbstate.TaskFailed}); err != nil {
			return nil, nil // a state that cannot fail; the next pass looks again
		}
		var drafts []events.Draft
		if t.CurrentAttemptID != nil {
			attemptID = *t.CurrentAttemptID
			var ds []events.Draft
			if _, ds, err = s.drainLocked(ctx, q, t, attemptID, evidenceNoRuntimeCapacity+s.overflowWait().String()); err != nil {
				return nil, err
			}
			drafts = append(drafts, ds...)
			if t, err = q.GetTaskByID(ctx, t.ID); err != nil {
				return nil, err
			}
		}
		after, err := q.UpdateTaskState(ctx, dbq.UpdateTaskStateParams{ID: t.ID, Version: t.Version, Status: dbstate.TaskFailed})
		if err != nil {
			return nil, err
		}
		s.log().WarnContext(ctx, "task failed: no runtime capacity", "task", t.ID, "waited", s.overflowWait())
		return append(drafts, s.updated(after)), nil
	})
	if err != nil || attemptID == "" {
		return err
	}
	// The task has ended; the settle sweep finishes the attempt if this is cut short.
	if err := s.settle(ctx, attemptID); err != nil {
		s.log().WarnContext(ctx, "settle after runtime wait", "attempt", attemptID, "error", err)
	}
	return nil
}
