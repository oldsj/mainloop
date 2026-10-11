package tasks

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// defaultResolveBackoff paces the resolver: it looks soon after a stop kagent did not confirm, then
// less and less often.
var defaultResolveBackoff = reconcile.Backoff{Base: 5 * time.Second, Max: 5 * time.Minute, Jitter: 0.2}

func (s *Service) resolveBackoff() reconcile.Backoff {
	if s.Limits.ResolveBackoff == (reconcile.Backoff{}) {
		return defaultResolveBackoff
	}
	return s.Limits.ResolveBackoff
}

func (s *Service) resolveBound() int {
	if s.Limits.ResolveBound > 0 {
		return s.Limits.ResolveBound
	}
	return defaultResolveBound
}

// blockReconciliation moves an open task to blocked/reconciliation when that transition is allowed,
// returning the event. A task that already ended, or already is blocked that way, is left alone.
func (s *Service) blockReconciliation(ctx context.Context, q *dbq.Queries, t dbq.Task) ([]events.Draft, error) {
	from := TaskState{Status: t.Status}
	if t.Reason != nil {
		from.Reason = *t.Reason
	}
	to := TaskState{Status: dbstate.TaskBlocked, Reason: dbstate.ReasonReconciliation}
	if TaskFinal(t.Status) || from == to || TransitionTask(from, to) != nil {
		return nil, nil
	}
	reason := to.Reason
	after, err := q.UpdateTaskState(ctx, dbq.UpdateTaskStateParams{ID: t.ID, Version: t.Version, Status: to.Status, Reason: &reason})
	if err != nil {
		return nil, err
	}
	return []events.Draft{s.updated(after)}, nil
}

// resolveStop is the resolver of a requested stop. For a draining attempt whose stop the observer has
// not confirmed, it asks the StopObserver whether anything still runs for the session. When nothing
// does and admitted sends have resolved without an outcome changing since observation began, it
// confirms the stop and returns 0: the attempt can be fenced. Otherwise it counts the look,
// schedules the next one with backoff and returns how long to wait. After the bound it marks the
// stop unresolved and blocks the task for reconciliation, once; it keeps looking, and a later
// confirmation still ends the attempt.
//
// Without an observer the service has no kagent client, so no agent can be running. An attempt with
// no session never started an agent.
func (s *Service) resolveStop(ctx context.Context, a dbq.TaskAttempt) (wait time.Duration, err error) {
	if !awaitingStop(a) {
		return 0, nil
	}
	q := dbq.New(s.Pool)
	// Capture the durable delivery version before any native reads. Native I/O holds no
	// transaction; confirmation atomically checks that this observation is still current.
	version, err := q.GetAttemptDeliveryOutcomeVersion(ctx, a.ID)
	if err != nil {
		return 0, err
	}
	stopped := s.StopObserver == nil || a.SessionID == nil
	if !stopped {
		var oerr error
		stopped, oerr = s.StopObserver.Stopped(ctx, *a.SessionID)
		if oerr != nil {
			s.log().WarnContext(ctx, "observe requested stop", "attempt", a.ID, "error", oerr)
			stopped = false
		}
	}
	if stopped {
		if _, err := q.ConfirmAttemptStop(ctx, dbq.ConfirmAttemptStopParams{ID: a.ID, OutcomeVersion: version}); err == nil {
			return 0, q.AppendAttemptEvidence(ctx, dbq.AppendAttemptEvidenceParams{ID: a.ID, Column2: evidenceStopConfirmed})
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return 0, err
		}
		// A concurrent resolver may have confirmed already. Otherwise an admitted delivery is
		// still sending/uncertain or changed outcome during observation: retain the reservation and
		// take the normal backoff path. A delivered task must be observed again, even if its send ended.
		current, err := q.GetTaskAttempt(ctx, a.ID)
		if err != nil {
			return 0, err
		}
		if current.StopState == stopConfirmed {
			return 0, nil
		}
	}
	wait = s.resolveBackoff().Delay(int(a.ResolveAttempts) + 1)
	n, err := q.DeferAttemptResolve(ctx, dbq.DeferAttemptResolveParams{ID: a.ID, DelaySeconds: wait.Seconds()})
	if err != nil {
		return 0, err
	}
	if int(n) >= s.resolveBound() {
		if err := s.raiseUnresolved(ctx, a.ID); err != nil {
			return 0, err
		}
	}
	return wait, nil
}

// raiseUnresolved marks the attempt's stop unresolved and blocks its task, once.
func (s *Service) raiseUnresolved(ctx context.Context, attemptID string) error {
	return s.tx(ctx, func(q *dbq.Queries) ([]events.Draft, error) {
		a, err := q.GetTaskAttempt(ctx, attemptID)
		if err != nil {
			return nil, err
		}
		t, err := q.GetTaskByIDForUpdate(ctx, a.TaskID)
		if err != nil {
			return nil, err
		}
		if _, err := q.MarkAttemptStopUnresolved(ctx, attemptID); errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // already raised, or confirmed meanwhile
		} else if err != nil {
			return nil, err
		}
		if err := q.AppendAttemptEvidence(ctx, dbq.AppendAttemptEvidenceParams{ID: a.ID, Column2: evidenceStopUnresolved}); err != nil {
			return nil, err
		}
		return s.blockReconciliation(ctx, q, t)
	})
}
