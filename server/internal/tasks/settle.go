package tasks

import (
	"cmp"
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/sessions"
	"github.com/oldsj/mainloop/server/internal/workspaces"
)

// Stop states of an attempt (task_attempts.stop_state). The state is requested in the statement that
// moves the attempt to draining, so a crash anywhere leaves a durable request. Only the observer
// confirms it, from what kagent reports for the attempt's session; fence releases the writer claim and
// the capacity slot only then. Evidence on the attempt is a diagnostic trail and is never read for
// safety.
const (
	stopNone       = "none"
	stopRequested  = "requested"
	stopConfirmed  = "confirmed"
	stopUnresolved = "unresolved"
)

// Diagnostic evidence written next to a stop state change.
const (
	evidenceStopConfirmed  = "runtime-stop-confirmed"
	evidenceStopUnresolved = "runtime-stop-unresolved"
)

// defaultResolveBound is how many unsuccessful looks at kagent the resolver makes before it raises
// evidence and blocks the task for reconciliation. It keeps looking afterwards, at the slowest pace.
const defaultResolveBound = 6

// defaultSettleIdle is how long the settle sweep leaves an attempt alone after its last change, so
// it never races the handler that is still working on it.
const defaultSettleIdle = 30 * time.Second

// awaitingStop is whether the attempt's stop has been requested and the observer has not yet
// confirmed it.
func awaitingStop(a dbq.TaskAttempt) bool {
	return a.StopState == stopRequested || a.StopState == stopUnresolved
}

// drain moves an attempt that could still act to draining, under the task lock (tasks before
// attempts). It is idempotent: an attempt already past creating/active is left alone. extra runs in
// the same transaction.
func (s *Service) drain(ctx context.Context, attemptID, evidence string, extra func(q *dbq.Queries, t dbq.Task) error) (dbq.TaskAttempt, error) {
	var out dbq.TaskAttempt
	err := s.tx(ctx, func(q *dbq.Queries) ([]events.Draft, error) {
		a, err := q.GetTaskAttempt(ctx, attemptID)
		if err != nil {
			return nil, err
		}
		t, err := q.GetTaskByIDForUpdate(ctx, a.TaskID)
		if err != nil {
			return nil, err
		}
		// A supervisor that stops takes its open children with it now, not when its own stop is
		// confirmed: they must not keep working while that stays unresolved.
		var drafts []events.Draft
		if a.Role == dbstate.AttemptSupervisor {
			if drafts, err = s.cancelOpenChildren(ctx, q, t); err != nil {
				return nil, err
			}
		}
		if out, err = q.GetTaskAttempt(ctx, attemptID); err != nil {
			return nil, err
		}
		if out.State == dbstate.AttemptCreating || out.State == dbstate.AttemptActive {
			args := dbq.MoveAttemptParams{
				ID: out.ID, FromState: out.State, ToState: dbstate.AttemptDraining, CapacityHeld: true,
			}
			if evidence != "" {
				args.EvidenceRef = &evidence
			}
			if out, err = q.MoveAttempt(ctx, args); err != nil {
				return nil, err
			}
		}
		if extra != nil {
			return drafts, extra(q, t)
		}
		return drafts, nil
	})
	return out, err
}

// cancelOpenChildren cancels the child tasks of a supervisor's task that have not ended, in id order,
// under the parent's lock. Their attempts are stopped by the settle sweep.
func (s *Service) cancelOpenChildren(ctx context.Context, q *dbq.Queries, t dbq.Task) ([]events.Draft, error) {
	children, err := q.ListChildTasksForUpdate(ctx, &t.ID)
	if err != nil {
		return nil, err
	}
	var drafts []events.Draft
	for _, c := range children {
		if TaskFinal(c.Status) {
			continue
		}
		after, err := q.UpdateTaskState(ctx, dbq.UpdateTaskStateParams{ID: c.ID, Version: c.Version, Status: dbstate.TaskCancelled})
		if err != nil {
			return nil, err
		}
		if err := q.BlockRequestedCreateOps(ctx, &c.ID); err != nil {
			return nil, err
		}
		drafts = append(drafts, s.updated(after))
	}
	return drafts, nil
}

// stop acts on a draining attempt: it cancels its session, which closes Mainloop's own open
// deliveries, and removes its workspace when the agent never ran. What the session's Cancel reports
// never confirms a stop (it may be a repeat on a cancelled session, or have no kagent session to ask):
// [Service.resolveStop] asks the observer, and only the observer confirms. stop is safe to repeat, and
// is skipped once the resolver has looked: it has already done its part, and the observer cancels
// whatever runs.
func (s *Service) stop(ctx context.Context, a dbq.TaskAttempt) error {
	if a.ResolveAttempts > 0 {
		return nil
	}
	if a.SessionID != nil {
		if _, err := s.Sessions.Cancel(ctx, *a.SessionID); err != nil && !errors.Is(err, sessions.ErrNotFound) {
			return err
		}
	}
	if a.BriefDeliveryID != nil || s.Workspaces == nil {
		return nil
	}
	// The agent never started, so nothing it did needs to stay: remove the workspace too.
	id := a.WorkspaceID
	if id == nil {
		// A create that was cut off before the attempt recorded its workspace: workspace creation
		// has no caller-supplied key, so it is found by owner, project, branch and time.
		var err error
		if id, err = s.orphanWorkspace(ctx, a); err != nil || id == nil {
			return err
		}
	}
	if err := s.Workspaces.Delete(ctx, *id); err != nil && !errors.Is(err, workspaces.ErrNotFound) {
		return err
	}
	return nil
}

// orphanWorkspace finds the workspace a cut-off create made for an attempt, if there is one.
func (s *Service) orphanWorkspace(ctx context.Context, a dbq.TaskAttempt) (*string, error) {
	q := dbq.New(s.Pool)
	t, err := q.GetTaskByID(ctx, a.TaskID)
	if err != nil {
		return nil, err
	}
	if t.Mode != dbstate.TaskModeCode || t.ProjectID == nil || t.CheckoutBranch == nil {
		return nil, nil
	}
	id, err := q.FindUnattachedWorkspace(ctx, dbq.FindUnattachedWorkspaceParams{
		UserID: t.OwnerID, ProjectID: *t.ProjectID, Branch: *t.CheckoutBranch, CreatedAt: a.CreatedAt,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// errStopUnconfirmed means an attempt cannot be fenced: the observer has not confirmed its agent stopped.
var errStopUnconfirmed = errors.New("tasks: the attempt's stop is not confirmed")

// fence revokes the attempt's credentials, releases its writer claim and its capacity slot, and
// moves it draining → fenced in one transaction. It refuses an attempt whose stop is not confirmed,
// because the agent may still be writing. extra runs in the same transaction.
func (s *Service) fence(ctx context.Context, attemptID string, extra func(q *dbq.Queries) error) (dbq.TaskAttempt, error) {
	var out dbq.TaskAttempt
	err := s.tx(ctx, func(q *dbq.Queries) ([]events.Draft, error) {
		a, err := q.GetTaskAttempt(ctx, attemptID)
		if err != nil {
			return nil, err
		}
		if _, err := q.GetTaskByIDForUpdate(ctx, a.TaskID); err != nil {
			return nil, err
		}
		if out, err = q.GetTaskAttempt(ctx, attemptID); err != nil {
			return nil, err
		}
		if out.State == dbstate.AttemptDraining {
			if out.StopState != stopConfirmed {
				return nil, errStopUnconfirmed
			}
			if out.BindingID != nil {
				if _, err := agentauth.RevokeInTx(ctx, q, *out.BindingID); err != nil {
					return nil, err
				}
			}
			if out.WriterGeneration != nil {
				if _, err := q.ReleaseTaskWriter(ctx, dbq.ReleaseTaskWriterParams{
					AttemptID: &out.ID, Generation: *out.WriterGeneration, FenceEvidenceRef: ptr("attempt-fenced:" + out.ID),
				}); err != nil {
					return nil, err
				}
			}
			if out, err = q.MoveAttempt(ctx, dbq.MoveAttemptParams{
				ID: out.ID, FromState: dbstate.AttemptDraining, ToState: dbstate.AttemptFenced,
			}); err != nil {
				return nil, err
			}
		}
		if extra != nil {
			return nil, extra(q)
		}
		return nil, nil
	})
	return out, err
}

// finish ends a fenced attempt and its task. The task is locked first, then (for a supervisor) its
// child tasks in id order, then the attempt: tasks before attempts, never an attempt and then
// child tasks. A task that is still open ends as taskIfOpen. When a supervisor ends, its children
// that have not ended are cancelled; their attempts are stopped by the settle sweep.
func (s *Service) finish(ctx context.Context, attemptID string, taskIfOpen dbstate.TaskStatus, extra func(q *dbq.Queries) error) error {
	return s.tx(ctx, func(q *dbq.Queries) ([]events.Draft, error) {
		a, err := q.GetTaskAttempt(ctx, attemptID)
		if err != nil {
			return nil, err
		}
		t, err := q.GetTaskByIDForUpdate(ctx, a.TaskID)
		if err != nil {
			return nil, err
		}
		var drafts []events.Draft
		if a.Role == dbstate.AttemptSupervisor {
			// Normally done by drain already; this covers a task that ended some other way.
			if drafts, err = s.cancelOpenChildren(ctx, q, t); err != nil {
				return nil, err
			}
		}
		if a, err = q.GetTaskAttempt(ctx, attemptID); err != nil {
			return nil, err
		}
		if !TaskFinal(t.Status) {
			after, err := q.UpdateTaskState(ctx, dbq.UpdateTaskStateParams{ID: t.ID, Version: t.Version, Status: taskIfOpen})
			if err != nil {
				return nil, err
			}
			t = after
			drafts = append(drafts, s.updated(after))
		}
		if a.State == dbstate.AttemptFenced {
			to := dbstate.AttemptFailed
			switch t.Status {
			case dbstate.TaskCompleted:
				to = dbstate.AttemptCompleted
			case dbstate.TaskCancelled:
				to = dbstate.AttemptCancelled
			}
			if _, err := q.MoveAttempt(ctx, dbq.MoveAttemptParams{
				ID: a.ID, FromState: dbstate.AttemptFenced, ToState: to,
			}); err != nil {
				return nil, err
			}
			if err := q.BlockCreateOpsForAttempt(ctx, &a.ID); err != nil {
				return nil, err
			}
		}
		if extra != nil {
			return drafts, extra(q)
		}
		return drafts, nil
	})
}

// settle stops an attempt and ends it: drain, stop the runtime, fence, final. Every step is
// idempotent on the attempt's state, so a crash anywhere resumes from the stored state. It is the
// path of an aborted provisioning and of the sweep for attempts whose task ended.
func (s *Service) settle(ctx context.Context, attemptID string) error {
	a, err := s.drain(ctx, attemptID, "", nil)
	if err != nil {
		return err
	}
	if AttemptFinal(a.State) {
		return nil
	}
	if a.State == dbstate.AttemptDraining {
		if err := s.stop(ctx, a); err != nil {
			return err
		}
		if a, err = dbq.New(s.Pool).GetTaskAttempt(ctx, attemptID); err != nil {
			return err
		}
		// The attempt stays draining, with its claim and slot, until the observer has seen the agent
		// stopped; the sweep looks again when the backoff is over.
		if wait, err := s.resolveStop(ctx, a); err != nil || wait > 0 {
			return err
		}
		if a, err = dbq.New(s.Pool).GetTaskAttempt(ctx, attemptID); err != nil {
			return err
		}
	}
	if a.State == dbstate.AttemptDraining {
		if a, err = s.fence(ctx, attemptID, nil); err != nil {
			return err
		}
	}
	if a.State == dbstate.AttemptFenced {
		return s.finish(ctx, attemptID, dbstate.TaskFailed, nil)
	}
	return nil
}

// SettleSweep ends the attempts that are still running or half-ended although they no longer
// should be: those of tasks that ended, and those left draining or fenced by a crash. An attempt
// that a cancel operation is draining is left to the operation.
func (s *Service) SettleSweep() reconcile.Sweep {
	return reconcile.Sweep{Name: "task-settle", Every: s.Limits.Interval, Run: s.runSettle}
}

func (s *Service) runSettle(ctx context.Context) error {
	q := dbq.New(s.Pool)
	idle := cmp.Or(s.Limits.SettleIdle, defaultSettleIdle)
	rows, err := q.ListUnsettledAttempts(ctx, dbq.ListUnsettledAttemptsParams{Limit: 50, IdleSeconds: idle.Seconds()})
	if err != nil {
		return err
	}
	var errs []error
	for _, a := range rows {
		open, err := q.ListOpenOperationsForSource(ctx, &a.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if len(open) > 0 {
			continue
		}
		if err := s.settle(ctx, a.ID); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// driveCancel runs a cancel operation that has an attempt: drain and stop it, fence it, then end
// the attempt and cancel the task. The task keeps its status until the attempt is fenced.
func (s *Service) driveCancel(ctx context.Context, c reconcile.Claim, op dbq.TaskOperation) (reconcile.Outcome, error) {
	if op.SourceAttemptID == nil {
		return reconcile.Done(string(dbstate.TaskOpCompleted)), nil
	}
	src := *op.SourceAttemptID
	switch op.State {
	case dbstate.TaskOpDraining:
		a, err := s.drain(ctx, src, "", nil)
		if err != nil {
			return reconcile.Outcome{}, err
		}
		if a.State == dbstate.AttemptDraining {
			if err := s.stop(ctx, a); err != nil {
				return reconcile.Outcome{}, err
			}
			if a, err = dbq.New(s.Pool).GetTaskAttempt(ctx, src); err != nil {
				return reconcile.Outcome{}, err
			}
			// The cancel waits for the observer to confirm the stop; the attempt keeps its claim and slot.
			if wait, err := s.resolveStop(ctx, a); err != nil || wait > 0 {
				if err != nil {
					return reconcile.Outcome{}, err
				}
				return reconcile.RetryAfter(wait), nil
			}
		}
		return s.moveOp(ctx, c, op, dbstate.TaskOpSourceFencing)
	case dbstate.TaskOpSourceFencing:
		_, err := s.fence(ctx, src, func(q *dbq.Queries) error {
			return advance(ctx, q, c, op, dbstate.TaskOpSourceFenced, nil, nil)
		})
		if err != nil {
			return reconcile.Outcome{}, err
		}
		return reconcile.Recorded(), nil
	case dbstate.TaskOpSourceFenced:
		err := s.finish(ctx, src, dbstate.TaskCancelled, func(q *dbq.Queries) error {
			return advance(ctx, q, c, op, dbstate.TaskOpCompleted, nil, nil)
		})
		if err != nil {
			return reconcile.Outcome{}, err
		}
		return reconcile.Recorded(), nil
	}
	return reconcile.Done(string(dbstate.TaskOpBlocked)), nil
}

func (s *Service) moveOp(ctx context.Context, c reconcile.Claim, op dbq.TaskOperation, to dbstate.TaskOpState) (reconcile.Outcome, error) {
	err := s.tx(ctx, func(q *dbq.Queries) ([]events.Draft, error) {
		return nil, advance(ctx, q, c, op, to, nil, nil)
	})
	if err != nil {
		return reconcile.Outcome{}, err
	}
	return reconcile.Recorded(), nil
}

// CompleteSweep completes a coordination task whose supervisor reported completion once its
// children have all ended and its report has reached its recipients. Only the task moves; its
// attempt is stopped by the settle sweep.
func (s *Service) CompleteSweep() reconcile.Sweep {
	return reconcile.Sweep{Name: "task-complete", Every: s.Limits.Interval, Run: s.runComplete}
}

func (s *Service) runComplete(ctx context.Context) error {
	ids, err := dbq.New(s.Pool).ListReconcilingCoordinationTasks(ctx, 50)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if err := s.completeCoordination(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Service) completeCoordination(ctx context.Context, id string) error {
	return s.tx(ctx, func(q *dbq.Queries) ([]events.Draft, error) {
		t, err := q.GetTaskByIDForUpdate(ctx, id)
		if err != nil {
			return nil, err
		}
		if t.Status != dbstate.TaskWaiting || t.Reason == nil || *t.Reason != dbstate.ReasonReconciliation {
			return nil, nil
		}
		children, err := q.ListChildTasksForUpdate(ctx, &t.ID)
		if err != nil {
			return nil, err
		}
		for _, c := range children {
			if !TaskFinal(c.Status) {
				return nil, nil
			}
		}
		open, err := q.CountOpenEventDeliveries(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		if open > 0 {
			return nil, nil
		}
		if err := TransitionTask(TaskState{Status: t.Status, Reason: *t.Reason}, TaskState{Status: dbstate.TaskCompleted}); err != nil {
			return nil, err
		}
		after, err := q.UpdateTaskState(ctx, dbq.UpdateTaskStateParams{ID: t.ID, Version: t.Version, Status: dbstate.TaskCompleted})
		if err != nil {
			return nil, err
		}
		return []events.Draft{s.updated(after)}, nil
	})
}
