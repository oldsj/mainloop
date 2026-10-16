package tasks

import (
	"cmp"
	"context"
	"errors"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// Defaults for the liveness sweep's bounds, taken when configuration gives none: 45 minutes of no
// activity, 6 hours of total runtime, and a minute between passes. They are a safety net, not the
// usual outcome of a task.
const (
	defaultStallTimeout  = 45 * time.Minute
	defaultMaxRuntime    = 6 * time.Hour
	defaultLivenessEvery = time.Minute
)

// LivenessObserver reads whether an attempt's kagent session still exists, whether its runtime can
// no longer run again, and when it last changed. *kagent.Client satisfies it through
// [kagent.Client.ObserveLiveness]. Without one the liveness sweep does nothing: it has no kagent
// client to ask, so it would otherwise block every running task on no evidence at all.
type LivenessObserver interface {
	ObserveLiveness(ctx context.Context, sessionID string) (kagent.SessionLiveness, error)
}

func (s *Service) stallTimeout() time.Duration { return cmp.Or(s.Limits.StallTimeout, defaultStallTimeout) }
func (s *Service) maxRuntime() time.Duration    { return cmp.Or(s.Limits.MaxRuntime, defaultMaxRuntime) }
func (s *Service) livenessEvery() time.Duration { return cmp.Or(s.Limits.LivenessEvery, defaultLivenessEvery) }

// LivenessSweep bounds every task whose current attempt is active: it is blocked, once, when
// kagent reports the attempt's session lost or terminal (session_lost), when the session has shown
// no activity for the stall timeout (stalled), or when the attempt has run longer than the runtime
// bound (runtime_limit). It cancels nothing native and retries nothing; it only raises the owner's
// attention, through the task's reason, so a session kagent has forgotten about does not leave a
// task reading "running" forever.
func (s *Service) LivenessSweep() reconcile.Sweep {
	return reconcile.Sweep{Name: "task-liveness", Every: s.livenessEvery(), Run: s.runLiveness}
}

func (s *Service) runLiveness(ctx context.Context) error {
	if s.Liveness == nil {
		return nil
	}
	q := dbq.New(s.Pool)
	rows, err := q.ListActiveAttemptsForLiveness(ctx, 100)
	if err != nil {
		return err
	}
	var errs []error
	for _, a := range rows {
		if err := s.checkLiveness(ctx, a); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// checkLiveness looks at one active attempt and blocks its task on the first bound it crosses. A
// session kagent has lost or ended is checked first: there is no point timing anything out once
// the session itself is gone. The stall timeout is checked next, since it is normally far shorter
// than the runtime bound and is the more informative reason; the runtime bound is the last-resort
// safety net, for a session that keeps reporting activity without ever finishing.
func (s *Service) checkLiveness(ctx context.Context, a dbq.TaskAttempt) error {
	if a.SessionID == nil {
		return nil
	}
	live, err := s.Liveness.ObserveLiveness(ctx, *a.SessionID)
	if err != nil {
		s.log().WarnContext(ctx, "tasks: observe attempt liveness", "attempt", a.ID, "error", err)
		return nil
	}
	now := s.now()
	switch {
	case !live.Exists || live.Terminal:
		return s.blockAttempt(ctx, a, dbstate.ReasonSessionLost)
	case !live.LastActivity.IsZero() && now.Sub(live.LastActivity) > s.stallTimeout():
		return s.blockAttempt(ctx, a, dbstate.ReasonStalled)
	case now.Sub(a.CreatedAt) > s.maxRuntime():
		return s.blockAttempt(ctx, a, dbstate.ReasonRuntimeLimit)
	}
	return nil
}

// blockAttempt moves an attempt's task to blocked under reason, once: a task already final, no
// longer pointed at this attempt, or already in exactly that state is left alone. The task row is
// locked before anything is read of it, as every other task write in this package does.
func (s *Service) blockAttempt(ctx context.Context, a dbq.TaskAttempt, reason dbstate.TaskReason) error {
	return s.tx(ctx, func(q *dbq.Queries) ([]events.Draft, error) {
		t, err := q.GetTaskByIDForUpdate(ctx, a.TaskID)
		if err != nil {
			return nil, err
		}
		if t.CurrentAttemptID == nil || *t.CurrentAttemptID != a.ID {
			return nil, nil
		}
		from := TaskState{Status: t.Status}
		if t.Reason != nil {
			from.Reason = *t.Reason
		}
		to := TaskState{Status: dbstate.TaskBlocked, Reason: reason}
		if TaskFinal(t.Status) || from == to || TransitionTask(from, to) != nil {
			return nil, nil
		}
		after, err := q.UpdateTaskState(ctx, dbq.UpdateTaskStateParams{ID: t.ID, Version: t.Version, Status: to.Status, Reason: &reason})
		if err != nil {
			return nil, err
		}
		return []events.Draft{s.updated(after)}, nil
	})
}
