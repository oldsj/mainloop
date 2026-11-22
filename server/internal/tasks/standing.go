package tasks

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/events"
)

// standingRequestID is the create request of a project's standing supervisor. It is the owner's,
// whoever's first use made it, so the supervisor belongs to the project and not to a caller.
func standingRequestID(projectID string) string { return "standing-supervisor:" + projectID }

// StandingBrief is the stored brief of a project's standing supervisor. What it is and how it
// works is rendered around it on every attempt (see [DelegatedBrief]), so a change of wording
// reaches the next attempt without rewriting the task.
func StandingBrief(fullName string) string {
	return "Be the standing supervisor of " + fullName + ". Work arrives as messages from main and the owner."
}

// EnsureSupervisor answers the project's standing supervisor, creating it on first use
// (docs/specs/project-supervisor.md). A project has one for its life: concurrent first uses
// serialize on a per-project lock and the unique index behind it, and make one task.
func (s *Service) EnsureSupervisor(ctx context.Context, owner, projectID string) (api.TaskView, error) {
	var id string
	err := events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		q := dbq.New(tx)
		if err := q.LockTaskRequest(ctx, "standing\x1f"+projectID); err != nil {
			return nil, err
		}
		project, err := q.GetProject(ctx, projectID)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && project.UserID != owner {
			return nil, refusal(404, ReasonProjectNotFound)
		}
		if err != nil {
			return nil, err
		}
		existing, err := q.GetStandingTask(ctx, &projectID)
		if err == nil {
			id = existing.ID
			return nil, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		in := CreateInput{
			RequestID: standingRequestID(projectID), Title: "Supervisor of " + project.FullName,
			Brief: StandingBrief(project.FullName), Mode: dbstate.TaskModeCoordination,
			ProjectID: projectID, standing: true,
		}
		dig, payload, err := digest(dbstate.TaskOpCreate, "", in.payload())
		if err != nil {
			return nil, err
		}
		_, t, err := s.createTx(ctx, q, OwnerPrincipal(owner), in, dig, payload)
		if err != nil {
			return nil, err
		}
		id = t.ID
		return []events.Draft{s.updated(t)}, nil
	})
	if err != nil {
		s.noteRefusal(ctx, err)
		return api.TaskView{}, err
	}
	return s.Get(ctx, OwnerPrincipal(owner), id)
}

// checkStanding is the liveness check of a standing supervisor's attempt (see [Service.checkLiveness]
// for every other task). It rests and wakes: with no turn running, nothing waiting on the owner and
// no message on its way, a running standing supervisor moves to waiting/idle, which is not a stall;
// a resting one whose session has a turn again moves back to running. A turn that is open but records
// nothing for the stall timeout is still stalled, and a wake still has the runtime bound, counted from
// the wake (a trigger restarts it). A session that is gone blocks it, resting or not.
func (s *Service) checkStanding(ctx context.Context, a dbq.TaskAttempt) error {
	if a.BindingID == nil {
		return nil
	}
	q := dbq.New(s.Pool)
	t, err := q.GetTaskByID(ctx, a.TaskID)
	if err != nil {
		return err
	}
	resting := t.Status == dbstate.TaskWaiting
	now := s.now()
	live, known, err := s.Liveness.ObserveLiveness(ctx, *a.BindingID)
	switch {
	case err != nil && resting, err == nil && !known && resting:
		if err != nil {
			s.log().WarnContext(ctx, "tasks: observe standing supervisor", "attempt", a.ID, "error", err)
		}
		return s.touchLiveness(ctx, a.ID)
	case err != nil:
		s.log().WarnContext(ctx, "tasks: observe attempt liveness", "attempt", a.ID, "error", err)
		return s.checkUnreadLiveness(ctx, a, now)
	case !known:
		return s.checkUnobservedLiveness(ctx, a, now)
	}
	if a.LivenessFailingSince != nil {
		if err := q.ClearAttemptLivenessFailure(ctx, a.ID); err != nil {
			return err
		}
	}
	active, changed := lastActivity(live, a, now)
	if changed && live.Exists && !live.Terminal {
		if err := q.RecordAttemptProgress(ctx, dbq.RecordAttemptProgressParams{
			ID: a.ID, ProgressMarker: &live.Progress, ProgressAt: &now,
		}); err != nil {
			return err
		}
	}
	idle := TaskState{Status: dbstate.TaskWaiting, Reason: dbstate.ReasonIdle}
	running := TaskState{Status: dbstate.TaskRunning}
	if !live.Exists || live.Terminal && !live.Recoverable {
		if resting {
			return s.moveStanding(ctx, a, idle, TaskState{Status: dbstate.TaskBlocked, Reason: dbstate.ReasonSessionLost})
		}
		return s.blockAttempt(ctx, a, dbstate.ReasonSessionLost)
	}
	busy := live.Progress != "" || live.AwaitingOwner
	if !busy && a.SessionID != nil {
		n, err := q.CountSessionPendingDeliveries(ctx, *a.SessionID)
		if err != nil {
			return err
		}
		busy = n > 0
	}
	switch {
	case resting && busy:
		return s.moveStanding(ctx, a, idle, running)
	case resting:
		return s.touchLiveness(ctx, a.ID)
	case !busy:
		return s.moveStanding(ctx, a, running, idle)
	case live.Progress != "" && !active.IsZero() && now.Sub(active) > s.stallTimeout():
		return s.blockAttempt(ctx, a, dbstate.ReasonStalled)
	case s.overRuntime(a, now):
		return s.blockAttempt(ctx, a, dbstate.ReasonRuntimeLimit)
	}
	return s.touchLiveness(ctx, a.ID)
}

// moveStanding moves a standing supervisor from one state to another, once: only while its attempt
// is still the active current one and the task is still exactly in from. Anything else that moved
// it since the sweep's read owns it now. Locks the task, then re-reads the attempt, as
// [Service.blockAttempt] does.
func (s *Service) moveStanding(ctx context.Context, a dbq.TaskAttempt, from, to TaskState) error {
	return s.tx(ctx, func(q *dbq.Queries) ([]events.Draft, error) {
		t, err := q.GetTaskByIDForUpdate(ctx, a.TaskID)
		if err != nil {
			return nil, err
		}
		cur, err := q.GetTaskAttempt(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		if cur.State != dbstate.AttemptActive || t.CurrentAttemptID == nil || *t.CurrentAttemptID != a.ID || !t.Standing {
			return nil, nil
		}
		now := TaskState{Status: t.Status}
		if t.Reason != nil {
			now.Reason = *t.Reason
		}
		if now != from || TransitionTask(from, to) != nil {
			return nil, nil
		}
		args := dbq.UpdateTaskStateParams{ID: t.ID, Version: t.Version, Status: to.Status}
		if to.Reason != "" {
			args.Reason = &to.Reason
		}
		after, err := q.UpdateTaskState(ctx, args)
		if err != nil {
			return nil, err
		}
		return []events.Draft{s.updated(after)}, nil
	})
}
