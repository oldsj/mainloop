package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/providers"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// maxContinuationReport bounds the predecessor's report quoted in a successor's brief, in runes.
const maxContinuationReport = 4000

// handoff shapes a retry or reassign, under the task lock. It moves the task to waiting/handoff
// and, when its attempt is still running, drains that attempt in this transaction, so the old agent
// loses MCP before the request returns. The operation then stops it, fences it and makes the
// successor; see [Service.driveHandoff]. Without [Service.CodeHandoff] a code task's is recorded
// blocked with reason handoff_unavailable: nothing could read where its successor starts.
func (s *Service) handoff(ctx context.Context, q *dbq.Queries, p Principal, t dbq.Task, kind dbstate.TaskOpKind, in ActionInput, op *dbq.InsertTaskOperationParams) (dbq.Task, []events.Draft, error) {
	if t.Status == dbstate.TaskCancelled || t.Status == dbstate.TaskCompleted {
		return t, nil, refusal(409, ReasonTaskTerminal)
	}
	if kind == dbstate.TaskOpReassign {
		if err := s.checkTarget(t, in.TargetProfileID); err != nil {
			return t, nil, err
		}
	}
	if t.Mode == dbstate.TaskModeCode && !s.CodeHandoff {
		if TaskFinal(t.Status) {
			return t, nil, refusal(409, ReasonTaskTerminal)
		}
		reason := dbstate.ReasonHandoffUnavailable
		op.State, op.Reason, op.SourceAttemptID = dbstate.TaskOpBlocked, &reason, t.CurrentAttemptID
		after, err := q.TouchTask(ctx, dbq.TouchTaskParams{ID: t.ID, Version: t.Version})
		return after, nil, err
	}
	if t.CurrentAttemptID == nil {
		return t, nil, refusal(409, ReasonTaskNotRunning)
	}
	if n, err := q.OpenOperationsForTask(ctx, &t.ID); err != nil {
		return t, nil, err
	} else if n > 0 {
		return t, nil, refusal(409, ReasonOperationPending)
	}
	if t.Standing {
		if err := s.restartLimit(ctx, q, p, t); err != nil {
			return t, nil, err
		}
	} else if lim := s.Limits.MaxHandoffs; lim > 0 && p.Role != "" {
		n, err := q.CountTaskHandoffs(ctx, t.ID)
		if err != nil {
			return t, nil, err
		}
		if n >= int64(lim) {
			return t, nil, refusalDetail(409, ReasonHandoffLimit, map[string]any{"handoffs": n, "limit": lim})
		}
	}
	a, err := q.GetTaskAttempt(ctx, *t.CurrentAttemptID)
	if err != nil {
		return t, nil, err
	}
	op.SourceAttemptID = &a.ID
	switch {
	case !TaskFinal(t.Status) && a.State == dbstate.AttemptActive:
		if err := TransitionOperation(kind, dbstate.TaskOpRequested, dbstate.TaskOpRequested, dbstate.TaskOpDraining); err != nil {
			return t, nil, err
		}
		from := TaskState{Status: t.Status}
		if t.Reason != nil {
			from.Reason = *t.Reason
		}
		if err := TransitionTask(from, TaskState{Status: dbstate.TaskWaiting, Reason: dbstate.ReasonHandoff}); err != nil {
			return t, nil, err
		}
		_, drafts, err := s.drainLocked(ctx, q, t, a.ID, "handoff-requested:"+string(kind))
		if err != nil {
			return t, nil, err
		}
		op.State, op.LastConfirmedStep = dbstate.TaskOpDraining, dbstate.TaskOpDraining
		reason := dbstate.ReasonHandoff
		after, err := q.UpdateTaskState(ctx, dbq.UpdateTaskStateParams{ID: t.ID, Version: t.Version, Status: dbstate.TaskWaiting, Reason: &reason})
		return after, drafts, err
	case t.Status == dbstate.TaskFailed && AttemptFinal(a.State):
		// Nothing runs: the operation makes the successor from requested. A child takes a slot of its
		// parent again, so the parent must still be open and have one.
		if t.ParentTaskID != nil {
			parent, err := q.GetTaskByID(ctx, *t.ParentTaskID)
			if err != nil {
				return t, nil, err
			}
			if TaskFinal(parent.Status) {
				return t, nil, refusal(409, ReasonParentNotActive)
			}
			if err := s.admit(ctx, q, &t); err != nil {
				return t, nil, err
			}
		}
		after, err := q.ReopenFailedTask(ctx, dbq.ReopenFailedTaskParams{ID: t.ID, Version: t.Version})
		if errors.Is(err, pgx.ErrNoRows) {
			return t, nil, refusal(409, ReasonStaleVersion)
		}
		return after, nil, err
	}
	// The attempt is still being stopped (a failed task's settle has not finished) or created.
	return t, nil, refusal(409, ReasonTaskSettling)
}

// checkTarget refuses a reassign target that can never run the task.
func (s *Service) checkTarget(t dbq.Task, target string) error {
	role := providers.RoleSupervisor
	if t.ParentTaskID != nil {
		role = roleChild
	}
	if _, err := s.Providers.Resolve(target, role, true); err != nil {
		switch {
		case errors.Is(err, providers.ErrUnknown):
			return refusal(422, ReasonProviderUnknown)
		case errors.Is(err, providers.ErrNoRole):
			return refusal(422, ReasonProviderRole)
		case errors.Is(err, providers.ErrDisabled):
			return refusal(422, ReasonProviderDisabled)
		}
		return err
	}
	return nil
}

// driveHandoff runs a retry or reassign: stop the source attempt and wait for the observer to
// confirm it, then fence it and make the successor in one transaction, then provision the successor
// and send it its brief, as a create does. A source that had already ended skips to the successor.
// A code task's successor first reads the task branch's head on origin (checkpoint_required).
func (s *Service) driveHandoff(ctx context.Context, c reconcile.Claim, op dbq.TaskOperation) (reconcile.Outcome, error) {
	if op.SourceAttemptID == nil {
		return reconcile.Done(string(dbstate.TaskOpBlocked)), nil
	}
	src := *op.SourceAttemptID
	var err error
	switch op.State {
	case dbstate.TaskOpRequested:
		err = s.tx(ctx, func(q *dbq.Queries) ([]events.Draft, error) {
			return s.successor(ctx, q, c, op)
		})
	case dbstate.TaskOpDraining:
		if wait, err := s.stopSource(ctx, src); err != nil || wait > 0 {
			if err != nil {
				return reconcile.Outcome{}, err
			}
			return reconcile.RetryAfter(wait), nil
		}
		return s.moveOp(ctx, c, op, dbstate.TaskOpSourceFencing)
	case dbstate.TaskOpSourceFencing:
		_, err = s.fence(ctx, src, func(q *dbq.Queries) ([]events.Draft, error) {
			return s.successor(ctx, q, c, op)
		})
	case dbstate.TaskOpCheckpointRequired:
		return s.checkpoint(ctx, c, op)
	case dbstate.TaskOpTargetCreating:
		return s.createRuntime(ctx, c, op)
	case dbstate.TaskOpTargetReady:
		return s.sendBrief(ctx, c, op)
	default:
		return reconcile.Done(string(dbstate.TaskOpBlocked)), nil
	}
	var e *Error
	switch {
	case errors.Is(err, errWriterHeld), errors.As(err, &e) && (e.Reason == ReasonChildCapacity || e.Reason == ReasonGlobalCapacity):
		return s.waitForSlot(ctx, c, op)
	case err != nil:
		return reconcile.Outcome{}, err
	}
	return reconcile.Recorded(), nil
}

// successor makes the attempt that replaces the operation's source, in the caller's transaction:
// it links the source to it (a fenced source becomes superseded), makes it the task's current
// attempt and moves the operation to target_creating, or checkpoint_required for a code task. The
// source's writer claim on the branch was released by its fence, so a code successor takes it. A task that ended or moved to another attempt
// meanwhile (a cancel won) blocks the operation instead, and its own settlement ends the source.
func (s *Service) successor(ctx context.Context, q *dbq.Queries, c reconcile.Claim, op dbq.TaskOperation) ([]events.Draft, error) {
	t, err := q.GetTaskByIDForUpdate(ctx, *op.TaskID)
	if err != nil {
		return nil, err
	}
	src, err := q.GetTaskAttempt(ctx, *op.SourceAttemptID)
	if err != nil {
		return nil, err
	}
	ended := src.State == dbstate.AttemptFenced || AttemptFinal(src.State)
	if TaskFinal(t.Status) || t.CurrentAttemptID == nil || *t.CurrentAttemptID != src.ID || !ended || src.SuccessorID != nil {
		return nil, advance(ctx, q, c, op, dbstate.TaskOpBlocked, nil, nil)
	}
	if t.ParentTaskID != nil {
		parent, err := q.GetTaskByID(ctx, *t.ParentTaskID)
		if err != nil {
			return nil, err
		}
		if TaskFinal(parent.Status) {
			// The parent ended during the handoff: the task ends too, and the source is settled with it.
			after, err := q.UpdateTaskState(ctx, dbq.UpdateTaskStateParams{ID: t.ID, Version: t.Version, Status: dbstate.TaskCancelled})
			if err != nil {
				return nil, err
			}
			return []events.Draft{s.updated(after)}, advance(ctx, q, c, op, dbstate.TaskOpBlocked, nil, nil)
		}
		// A fenced source gave its slot up in this transaction, so the successor takes it back. One
		// that ended earlier holds none, and another child may have taken it since the request.
		if src.State != dbstate.AttemptFenced {
			if err := s.admit(ctx, q, &t); err != nil {
				return nil, err
			}
		}
	}
	if op.Kind == dbstate.TaskOpReassign {
		target, err := reassignTarget(op)
		if err != nil {
			return nil, err
		}
		if target != t.AssignedProfileID {
			if t, err = q.SetTaskProfile(ctx, dbq.SetTaskProfileParams{ID: t.ID, AssignedProfileID: target}); err != nil {
				return nil, err
			}
		}
	}
	att, err := s.insertAttempt(ctx, q, t, &src.ID)
	if err != nil {
		return nil, err
	}
	if _, err := q.SupersedeAttempt(ctx, dbq.SupersedeAttemptParams{ID: src.ID, SuccessorID: &att.ID}); err != nil {
		return nil, err
	}
	reason := dbstate.ReasonHandoff
	after, err := q.SetTaskAttempt(ctx, dbq.SetTaskAttemptParams{
		ID: t.ID, CurrentAttemptID: &att.ID, Status: dbstate.TaskWaiting, Reason: &reason,
	})
	if err != nil {
		return nil, err
	}
	next := dbstate.TaskOpTargetCreating
	if t.Mode == dbstate.TaskModeCode {
		next = dbstate.TaskOpCheckpointRequired
	}
	return []events.Draft{s.updated(after)}, advance(ctx, q, c, op, next, nil, &att.ID)
}

// reassignTarget reads the provider a reassign names from its recorded request.
func reassignTarget(op dbq.TaskOperation) (string, error) {
	var payload struct {
		Target string `json:"target_profile_id"`
	}
	if err := json.Unmarshal(op.RequestPayload, &payload); err != nil {
		return "", err
	}
	if payload.Target == "" {
		return "", errors.New("tasks: a reassign records no target")
	}
	return payload.Target, nil
}

// continuation is the part of a successor's brief that says what came before: which attempt it
// replaces, why, where a code successor's workspace starts, and that attempt's last report. It
// reads only rows that no longer change once the successor's workspace is made, so a resend of the
// brief carries the same text.
func continuation(ctx context.Context, q *dbq.Queries, a dbq.TaskAttempt, t dbq.Task) (string, error) {
	if a.PredecessorID == nil {
		return "", nil
	}
	prev, err := q.GetTaskAttempt(ctx, *a.PredecessorID)
	if err != nil {
		return "", err
	}
	if t.Standing {
		return standingContinuation(ctx, q, a, prev, t)
	}
	var b strings.Builder
	b.WriteString("## Continuation\n")
	if prev.ProfileID != a.ProfileID {
		fmt.Fprintf(&b, "This task was reassigned from provider %s. ", prev.ProfileID)
	} else {
		b.WriteString("This task was retried. ")
	}
	fmt.Fprintf(&b, "Attempt %d was stopped and you are attempt %d, in a new session: you do not have its context. "+
		"It may have done part of the work, so check what exists before redoing it.", prev.Number, a.Number)
	if t.Mode == dbstate.TaskModeCoordination && prev.Role == dbstate.AttemptSupervisor {
		b.WriteString(" Child tasks it had open were cancelled with it; `task_list` shows them.")
	}
	if t.Mode == dbstate.TaskModeCode {
		note, err := codeStart(ctx, q, a, prev, t)
		if err != nil {
			return "", err
		}
		b.WriteString("\n\n" + note)
	}
	rep, err := q.LatestAttemptReport(ctx, prev.ID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		b.WriteString("\n\nIt made no report.")
	case err != nil:
		return "", err
	default:
		summary := rep.Summary
		if r := []rune(summary); len(r) > maxContinuationReport {
			summary = string(r[:maxContinuationReport]) + "…"
		}
		fmt.Fprintf(&b, "\n\nIts last report (%s):\n%s", rep.Outcome, summary)
	}
	return b.String(), nil
}

// codeStart says where a code successor's workspace starts: the head of the task branch on origin,
// with what the predecessor pushed, or the base when the branch was never pushed.
func codeStart(ctx context.Context, q *dbq.Queries, a, prev dbq.TaskAttempt, t dbq.Task) (string, error) {
	ref, err := q.GetHandoffCheckpoint(ctx, &a.ID)
	if err != nil {
		return "", err
	}
	branch, sha := "", ""
	if t.CheckoutBranch != nil {
		branch = *t.CheckoutBranch
	}
	if a.InitialRef != nil {
		sha = *a.InitialRef
	}
	if ref != nil && *ref == "refs/heads/"+branch {
		return fmt.Sprintf("Your workspace starts at %s, the head of branch `%s` on origin: it has what attempt %d pushed, "+
			"and nothing it did not push. If the branch has a pull request, keep working on that one.", sha, branch, prev.Number), nil
	}
	base := "the repository's default branch"
	if ref != nil && *ref != defaultBranchRef {
		base = "`" + *ref + "`"
	}
	return fmt.Sprintf("Attempt %d never pushed branch `%s`, so none of its work is in your workspace: it starts at %s, "+
		"the head of %s. Push your work to `%s` as usual.", prev.Number, branch, sha, base, branch), nil
}
