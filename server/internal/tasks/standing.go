package tasks

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/events"
)

// standingRequestID is the create request of a project's standing supervisor. It is the owner's,
// whoever's first use made it, so the supervisor belongs to the project and not to a caller. The
// project's first has the plain id; one started after n retired ones has n+1 appended.
func standingRequestID(projectID string, retired int) string {
	id := "standing-supervisor:" + projectID
	if retired > 0 {
		id += ":" + strconv.Itoa(retired+1)
	}
	return id
}

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
		retired, err := q.CountStandingTasks(ctx, &projectID)
		if err != nil {
			return nil, err
		}
		in := CreateInput{
			RequestID: standingRequestID(projectID, int(retired)), Title: "Supervisor of " + project.FullName,
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
// the wake (a trigger restarts it). A session that is gone is recovered, resting or not (see
// [Service.recoverStanding]).
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
		return s.recoverStanding(ctx, a)
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
	res, err := s.FollowUp(ctx, p, FollowUpInput{TaskID: v.Task.Id, RequestID: in.RequestID, Text: in.Text, kind: "supervisor-send"})
	var e *Error
	if errors.As(err, &e) && e.Reason == ReasonSessionGone {
		// Its session is gone: recover it, and main sends again once the successor has its brief.
		if a, aerr := dbq.New(s.Pool).GetTaskByID(ctx, v.Task.Id); aerr == nil && a.CurrentAttemptID != nil {
			att, aerr := dbq.New(s.Pool).GetTaskAttempt(ctx, *a.CurrentAttemptID)
			if aerr == nil {
				aerr = s.recoverStanding(ctx, att)
			}
			if aerr != nil {
				return FollowUpResult{}, aerr
			}
		}
		return FollowUpResult{}, refusal(409, ReasonSupervisorStarting)
	}
	return res, err
}

// briefed reports whether a task has a current attempt that has been given its first brief, or
// that ended: an ended one is for FollowUp to refuse, not for SupervisorSend to wait on.
func (s *Service) briefed(ctx context.Context, taskID string) (bool, error) {
	q := dbq.New(s.Pool)
	t, err := q.GetTaskByID(ctx, taskID)
	if err != nil {
		return false, err
	}
	if t.Status == dbstate.TaskWaiting && t.Reason != nil && *t.Reason == dbstate.ReasonHandoff {
		return false, nil // a restart is making its successor
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

// Restarts of a standing supervisor that main and Mainloop may make in an hour; the owner's are not
// counted. A crash loop stops at the fourth, and the owner decides.
const (
	maxRestartsPerHour = 3
	restartWindow      = time.Hour
)

// restartLimit refuses a restart past [maxRestartsPerHour] by anyone but the owner. A standing
// supervisor has no lifetime handoff cap: it restarts for as long as the project lives.
func (s *Service) restartLimit(ctx context.Context, q *dbq.Queries, p Principal, t dbq.Task) error {
	if p.Role == "" && !p.Recovery {
		return nil
	}
	n, err := q.CountTaskHandoffsSince(ctx, dbq.CountTaskHandoffsSinceParams{TaskID: t.ID, CreatedAt: s.now().Add(-restartWindow)})
	if err != nil {
		return err
	}
	if n >= maxRestartsPerHour {
		return refusalDetail(409, ReasonHandoffLimit, map[string]any{"handoffs": n, "limit": maxRestartsPerHour, "window": "1h"})
	}
	return nil
}

// RestartSupervisor restarts (fresh: false) or resets (fresh: true) a project's standing
// supervisor for the owner: a retry of its task, keeping its children. The request is keyed by its
// request id and body, not by the task's version, so a retry after the restart moved the task finds
// the first operation.
func (s *Service) RestartSupervisor(ctx context.Context, owner, projectID, requestID string, fresh bool) (api.TaskOperation, error) {
	if n := len(requestID); n == 0 || n > 100 {
		return api.TaskOperation{}, refusal(422, ReasonInvalidRequest)
	}
	p := OwnerPrincipal(owner)
	dig, payload, err := digest(dbstate.TaskOpRetry, "standing:"+projectID, map[string]any{"fresh": fresh})
	if err != nil {
		return api.TaskOperation{}, err
	}
	var op dbq.TaskOperation
	err = events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		q := dbq.New(tx)
		stored, replay, err := request(ctx, q, p, requestID, dig)
		if err != nil {
			return nil, err
		}
		if replay {
			op = stored
			return nil, nil
		}
		project, err := q.GetProject(ctx, projectID)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && project.UserID != owner {
			return nil, refusal(404, ReasonProjectNotFound)
		}
		if err != nil {
			return nil, err
		}
		st, err := q.GetStandingTask(ctx, &projectID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, refusal(404, ReasonTaskNotFound)
		}
		if err != nil {
			return nil, err
		}
		t, err := q.GetTaskForUpdate(ctx, dbq.GetTaskForUpdateParams{ID: st.ID, OwnerID: owner})
		if err != nil {
			return nil, err
		}
		in := ActionInput{RequestID: requestID, TaskID: t.ID, Fresh: fresh}
		var drafts []events.Draft
		op, drafts, err = s.actOn(ctx, q, p, dbstate.TaskOpRetry, in, t, dig, payload)
		return drafts, err
	})
	if err != nil {
		s.noteRefusal(ctx, err)
		return api.TaskOperation{}, err
	}
	return toOperation(op)
}

// RetireSupervisor ends the project's standing supervisor, for the owner only
// (docs/specs/project-supervisor.md#retire). Its open children are cancelled now; the supervisor
// is cancelled as any task is, once its session's stop is confirmed. The project's next first use
// starts a new one.
func (s *Service) RetireSupervisor(ctx context.Context, owner, projectID, requestID string) (api.TaskOperation, error) {
	if n := len(requestID); n == 0 || n > 100 {
		return api.TaskOperation{}, refusal(422, ReasonInvalidRequest)
	}
	p := OwnerPrincipal(owner)
	dig, payload, err := digest(dbstate.TaskOpCancel, "standing:"+projectID, map[string]any{"retire": true})
	if err != nil {
		return api.TaskOperation{}, err
	}
	var op dbq.TaskOperation
	err = events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		q := dbq.New(tx)
		stored, replay, err := request(ctx, q, p, requestID, dig)
		if err != nil {
			return nil, err
		}
		if replay {
			op = stored
			return nil, nil
		}
		// The same lock as a first use, so a retire and a start of the next supervisor are ordered.
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
		st, err := q.GetStandingTask(ctx, &projectID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, refusal(404, ReasonTaskNotFound)
		}
		if err != nil {
			return nil, err
		}
		t, err := q.GetTaskForUpdate(ctx, dbq.GetTaskForUpdateParams{ID: st.ID, OwnerID: owner})
		if err != nil {
			return nil, err
		}
		drafts, err := s.cancelOpenChildren(ctx, q, t)
		if err != nil {
			return nil, err
		}
		in := ActionInput{RequestID: requestID, TaskID: t.ID}
		var more []events.Draft
		op, more, err = s.record(ctx, q, p, dbstate.TaskOpCancel, in, t, dig, payload)
		return append(drafts, more...), err
	})
	if err != nil {
		s.noteRefusal(ctx, err)
		return api.TaskOperation{}, err
	}
	return toOperation(op)
}

// Continuation bounds: messages the old session received after its last report.
const (
	continuationMessages     = 3
	continuationMessageChars = 1000
	continuationChildren     = 50
)

// standingContinuation is a standing supervisor successor's continuation, read from the database
// with no model call (the old session may be gone): its last report, unless the restart was a
// reset; its open children and loops; and the newest messages its old session got after that
// report, so a message that woke a dying session is not lost.
func standingContinuation(ctx context.Context, q *dbq.Queries, a, prev dbq.TaskAttempt, t dbq.Task) (string, error) {
	fresh := false
	if op, err := q.GetOperationForTarget(ctx, &a.ID); err == nil {
		var body struct {
			Fresh bool `json:"fresh"`
		}
		if json.Unmarshal(op.RequestPayload, &body) == nil {
			fresh = body.Fresh
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	var b strings.Builder
	b.WriteString("## Continuation\n")
	fmt.Fprintf(&b, "You were restarted: you are attempt %d, in a new session, replacing attempt %d, and you do not have "+
		"its context. You are still this project's standing supervisor, on the same task. Your workers kept running "+
		"and report to you; `task_list` shows them. Check what exists before redoing anything.", a.Number, prev.Number)
	since := prev.CreatedAt
	if fresh {
		b.WriteString(" This was a reset: start from the facts below, not from the old session's account.")
	} else {
		rep, err := q.LatestTaskReport(ctx, t.ID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			b.WriteString("\n\nNo report was made before.")
		case err != nil:
			return "", err
		default:
			summary := rep.Summary
			if r := []rune(summary); len(r) > maxContinuationReport {
				summary = string(r[:maxContinuationReport]) + "…"
			}
			fmt.Fprintf(&b, "\n\nYour last report (%s):\n%s", rep.Outcome, summary)
			since = rep.CreatedAt
		}
	}

	children, err := q.ListChildTasks(ctx, dbq.ListChildTasksParams{OwnerID: t.OwnerID, ParentTaskID: &t.ID})
	if err != nil {
		return "", err
	}
	b.WriteString("\n\n### Open workers\n")
	open := 0
	for _, c := range children {
		if TaskFinal(c.Status) {
			continue
		}
		if open++; open > continuationChildren {
			b.WriteString("- …more; `task_list` has all of them.\n")
			break
		}
		fmt.Fprintf(&b, "- [%s](/tasks/%s): %s", c.Title, c.ID, c.Status)
		if c.Reason != nil {
			fmt.Fprintf(&b, "/%s", *c.Reason)
		}
		if c.PrNumber != nil {
			fmt.Fprintf(&b, "; pull request #%d %s, CI %s", *c.PrNumber, c.PrState, c.CiState)
		}
		if rep, err := q.LatestTaskReport(ctx, c.ID); err == nil {
			line, _, _ := strings.Cut(rep.Summary, "\n")
			if r := []rune(line); len(r) > 200 {
				line = string(r[:200]) + "…"
			}
			fmt.Fprintf(&b, "; last report (%s): %s", rep.Outcome, line)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		}
		b.WriteString("\n")
	}
	if open == 0 {
		b.WriteString("None.\n")
	}

	loops, err := q.ListTaskLoops(ctx, dbq.ListTaskLoopsParams{OwnerID: t.OwnerID, RecipientKey: "parent-task:" + t.ID, MaxRows: maxListedLoops})
	if err != nil {
		return "", err
	}
	b.WriteString("\n### Armed loops\n")
	armed := 0
	for _, l := range loops {
		if l.State != "armed" && l.State != "paused" {
			continue
		}
		armed++
		fmt.Fprintf(&b, "- %s (%s trigger, %s): %s\n", l.ID, l.Trigger, l.State, l.Note)
	}
	if armed == 0 {
		b.WriteString("None.\n")
	}

	if !fresh && prev.SessionID != nil {
		msgs, err := q.ListSessionMessagesSince(ctx, dbq.ListSessionMessagesSinceParams{
			SessionID: *prev.SessionID, Since: since, SkipID: prev.BriefDeliveryID, MaxRows: continuationMessages,
		})
		if err != nil {
			return "", err
		}
		if len(msgs) > 0 {
			b.WriteString("\n### Messages to the old session after that report (newest first)\n")
			for _, m := range msgs {
				text := m.Content
				if r := []rune(text); len(r) > continuationMessageChars {
					text = string(r[:continuationMessageChars]) + "…"
				}
				fmt.Fprintf(&b, "- %s:\n%s\n", m.CreatedAt.UTC().Format(time.RFC3339), text)
			}
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// recoverRequestID keys the recovery of a standing supervisor's attempt whose session is gone: the
// liveness sweep and a send that both notice start one operation.
func recoverRequestID(attemptID string) string { return "standing-recover:" + attemptID }

// recoverStanding restarts a standing supervisor whose native session is gone, as Mainloop: a
// restart that keeps its children, keyed by the gone attempt. A restart already under way is joined,
// not doubled. Past the hourly limit the supervisor is blocked session_lost for the owner instead,
// so a crash loop stops.
func (s *Service) recoverStanding(ctx context.Context, a dbq.TaskAttempt) error {
	p := Principal{Recovery: true}
	return s.tx(ctx, func(q *dbq.Queries) ([]events.Draft, error) {
		t, err := q.GetTaskByIDForUpdate(ctx, a.TaskID)
		if err != nil {
			return nil, err
		}
		if !t.Standing || TaskFinal(t.Status) || t.CurrentAttemptID == nil || *t.CurrentAttemptID != a.ID {
			return nil, nil
		}
		p.OwnerID = t.OwnerID
		reqID := recoverRequestID(a.ID)
		dig, payload, err := digest(dbstate.TaskOpRetry, t.ID, map[string]any{"recover": a.ID})
		if err != nil {
			return nil, err
		}
		if _, replay, err := request(ctx, q, p, reqID, dig); err != nil || replay {
			return nil, err
		}
		if n, err := q.OpenOperationsForTask(ctx, &t.ID); err != nil || n > 0 {
			return nil, err
		}
		op, drafts, err := s.actOn(ctx, q, p, dbstate.TaskOpRetry, ActionInput{RequestID: reqID, TaskID: t.ID}, t, dig, payload)
		var e *Error
		if errors.As(err, &e) && e.Reason == ReasonHandoffLimit {
			s.log().WarnContext(ctx, "tasks: standing supervisor restarted too often; blocked for the owner", "task", t.ID)
			return s.blockStanding(ctx, q, t, dbstate.ReasonSessionLost)
		}
		if err != nil {
			return nil, err
		}
		s.log().InfoContext(ctx, "tasks: recovering standing supervisor", "task", t.ID, "attempt", a.ID, "operation", op.ID)
		return drafts, nil
	})
}

// blockStanding moves a standing supervisor to blocked with a reason, from running or rest.
func (s *Service) blockStanding(ctx context.Context, q *dbq.Queries, t dbq.Task, reason dbstate.TaskReason) ([]events.Draft, error) {
	if t.Status == dbstate.TaskBlocked {
		return nil, nil
	}
	after, err := q.UpdateTaskState(ctx, dbq.UpdateTaskStateParams{ID: t.ID, Version: t.Version, Status: dbstate.TaskBlocked, Reason: &reason})
	if err != nil {
		return nil, err
	}
	return []events.Draft{s.updated(after)}, nil
}
