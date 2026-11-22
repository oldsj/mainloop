package tasks

import (
	"cmp"
	"context"
	"errors"
	"time"

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

// defaultSupervisorStartWait is [Limits.SupervisorStartWait]'s default: a coordination attempt has
// no workspace, so its first brief is normally recorded within seconds of the create.
const defaultSupervisorStartWait = 30 * time.Second

// SupervisorSendInput is main's message for a project's standing supervisor.
type SupervisorSendInput struct {
	ProjectID, RequestID, Text string
}

// SupervisorSend is main's way to a project (docs/specs/project-supervisor.md): it starts the
// project's standing supervisor if it has none and sends text to its session, as [Service.FollowUp]
// does, waking it. A supervisor still starting is waited on until its first brief is recorded, so the
// message never runs before the brief; past the wait it is refused supervisor_starting and sent again
// under the same request_id. The message id derives from main and request_id, so a retry never sends
// twice.
func (s *Service) SupervisorSend(ctx context.Context, p Principal, in SupervisorSendInput) (FollowUpResult, error) {
	if p.Role != dbstate.RoleMain {
		return FollowUpResult{}, refusal(403, ReasonManagementDenied)
	}
	if n := len(in.ProjectID); n == 0 || n > 100 {
		return FollowUpResult{}, refusal(422, ReasonInvalidRequest)
	}
	if err := ensureActive(ctx, dbq.New(s.Pool), p); err != nil {
		return FollowUpResult{}, err
	}
	v, err := s.EnsureSupervisor(ctx, p.OwnerID, in.ProjectID)
	if err != nil {
		return FollowUpResult{}, err
	}
	deadline := s.now().Add(cmp.Or(s.Limits.SupervisorStartWait, defaultSupervisorStartWait))
	for {
		ready, err := s.briefed(ctx, v.Task.Id)
		if err != nil {
			return FollowUpResult{}, err
		}
		if ready {
			break
		}
		if !s.now().Before(deadline) {
			return FollowUpResult{}, refusal(409, ReasonSupervisorStarting)
		}
		select {
		case <-ctx.Done():
			return FollowUpResult{}, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return s.FollowUp(ctx, p, FollowUpInput{TaskID: v.Task.Id, RequestID: in.RequestID, Text: in.Text, kind: "supervisor-send"})
}

// briefed reports whether a task has a current attempt that has been given its first brief, or
// that ended: an ended one is for FollowUp to refuse, not for SupervisorSend to wait on.
func (s *Service) briefed(ctx context.Context, taskID string) (bool, error) {
	q := dbq.New(s.Pool)
	t, err := q.GetTaskByID(ctx, taskID)
	if err != nil {
		return false, err
	}
	if t.CurrentAttemptID == nil {
		return TaskFinal(t.Status) || t.Status == dbstate.TaskBlocked, nil
	}
	a, err := q.GetTaskAttempt(ctx, *t.CurrentAttemptID)
	if err != nil {
		return false, err
	}
	return a.BriefDeliveryID != nil || a.State != dbstate.AttemptCreating && a.State != dbstate.AttemptActive, nil
}
