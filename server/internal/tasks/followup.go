package tasks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/sessions"
)

// MaxFollowUpChars bounds a follow-up message.
const MaxFollowUpChars = 8000

// FollowUpInput is a follow-up: a message for a task's own native session.
type FollowUpInput struct {
	TaskID, RequestID, Text string
}

// FollowUpResult says what became of a follow-up.
type FollowUpResult struct {
	TaskID, AttemptID, SessionID, MessageID string
	// How is "sent" (the message starts the session's next turn) or "queued" (it waits behind the
	// turn that is open).
	How string
	// Reopened is set when the follow-up reopened a completed task on its attempt's session.
	Reopened bool
	// Version is the task's version after the follow-up.
	Version int
}

// API is the result on the wire.
func (r FollowUpResult) API() api.TaskFollowUpResult {
	return api.TaskFollowUpResult{
		TaskId: r.TaskID, AttemptId: r.AttemptID, SessionId: r.SessionID, MessageId: r.MessageID,
		Delivery: api.TaskFollowUpResultDelivery(r.How), Reopened: r.Reopened, Version: r.Version,
	}
}

// FollowUp sends a message to the native session of a task the principal manages: the owner and
// main any task, a supervisor its direct children. The task keeps its native session, so the agent
// keeps its context:
//
//   - A task whose current attempt is active (running, waiting or blocked) gets the message as the
//     session's next turn, and moves back to running: someone answered, and the task's completion
//     waits for the agent's next report.
//   - A completed coordination task reopens on the same attempt and session: the attempt is active
//     again with a fresh stop fence and capacity, the session takes messages again, and the
//     delivery loop republishes the binding's MCP credential before it sends.
//
// The kagent Session is checked first: a follow-up to a Session kagent no longer has is refused
// (session_gone) rather than sent to a new, empty Session; an asleep one is fine, the send wakes
// it. Everything after that commits in one transaction under the session's send lock, so two
// follow-ups, or a follow-up and a cancel or a completion, are serialized: there is never a
// reopened task without its message, nor a second writer. The message id derives from the
// principal and request id, so a retry finds the first send and never sends twice.
func (s *Service) FollowUp(ctx context.Context, p Principal, in FollowUpInput) (FollowUpResult, error) {
	if !p.canManage() {
		return FollowUpResult{}, refusal(403, ReasonManagementDenied)
	}
	if n := len(in.RequestID); n == 0 || n > 100 || strings.TrimSpace(in.Text) == "" ||
		utf8.RuneCountInString(in.Text) > MaxFollowUpChars {
		return FollowUpResult{}, refusal(422, ReasonInvalidRequest)
	}
	t, a, err := s.followUpTarget(ctx, p, in.TaskID)
	if err != nil {
		return FollowUpResult{}, err
	}
	if s.Sessions == nil {
		return FollowUpResult{}, errors.New("tasks: no session runtime")
	}
	if s.Liveness != nil {
		live, known, err := s.Liveness.ObserveLiveness(ctx, *a.SessionID)
		if err != nil {
			return FollowUpResult{}, fmt.Errorf("tasks: read the task's native session: %w", err)
		}
		if known && (!live.Exists || live.Terminal) {
			return FollowUpResult{}, refusal(409, ReasonSessionGone)
		}
	}
	mid := uuid.NewSHA1(uuid.NameSpaceURL, fmt.Appendf(nil, "mainloop:follow-up:%s:%s", p.Key(), in.RequestID)).String()
	source, text := sessions.SourceUser, in.Text
	switch p.Role {
	case dbstate.RoleMain:
		source, text = sessions.SourceMainloop, "[message from main]\n"+in.Text
	case dbstate.RoleSupervisor:
		source, text = sessions.SourceMainloop, fmt.Sprintf("[message from supervisor task %s]\n%s", p.TaskID, in.Text)
	}
	out := FollowUpResult{TaskID: t.ID, AttemptID: a.ID, SessionID: *a.SessionID, MessageID: mid, How: "sent"}
	err = events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		// The send lock first, as every send and session end takes it before the attempt: the task
		// and attempt locks below then cannot invert that order.
		if err := sessions.LockSend(ctx, tx, *a.SessionID); err != nil {
			return nil, err
		}
		q := dbq.New(tx)
		cur, err := q.GetTaskByIDForUpdate(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		if err := ensureActive(ctx, q, p); err != nil {
			return nil, err
		}
		if cur.CurrentAttemptID == nil || *cur.CurrentAttemptID != a.ID {
			return nil, refusal(409, ReasonStaleAttempt)
		}
		att, err := q.GetTaskAttempt(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		after, reopened, err := s.prepareFollowUp(ctx, q, p, cur, att)
		if err != nil {
			return nil, err
		}
		sent, err := s.Sessions.SendInTx(ctx, tx, source, *a.SessionID, mid, text, sessions.SendOptions{})
		if err != nil {
			return nil, sendRefusal(err)
		}
		out.Reopened, out.Version = reopened, int(after.Version)
		if sent.Queued {
			out.How = "queued"
		}
		if after.Version == cur.Version {
			return nil, nil
		}
		return []events.Draft{s.updated(after)}, nil
	})
	if err = sessions.AsBusy(err); err != nil {
		if errors.Is(err, sessions.ErrBusy) {
			// Another send recorded an open turn between this one's checks and its insert; the
			// follow-up rolled back with nothing written, and the caller retries with its request id.
			return FollowUpResult{}, refusal(409, ReasonOperationPending)
		}
		return FollowUpResult{}, err
	}
	return out, nil
}

// followUpTarget reads, before any lock, the task a follow-up names and the attempt whose session
// will take it, and refuses what can never take one. The transaction checks it all again. The reads
// are in a transaction of their own so a revoked credential is refused before anything is read.
func (s *Service) followUpTarget(ctx context.Context, p Principal, id string) (t dbq.Task, a dbq.TaskAttempt, err error) {
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		if err := ensureActive(ctx, q, p); err != nil {
			return err
		}
		var err error
		t, err = q.GetTaskByID(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && t.OwnerID != p.OwnerID {
			return refusal(404, ReasonTaskNotFound)
		}
		if err != nil {
			return err
		}
		read, manage := p.access(t)
		if !read {
			return refusal(404, ReasonTaskNotFound)
		}
		if !manage {
			return refusal(403, ReasonManagementDenied)
		}
		if err := followUpStatus(t); err != nil {
			return err
		}
		if t.CurrentAttemptID == nil {
			return refusal(409, ReasonTaskNotRunning)
		}
		if a, err = q.GetTaskAttempt(ctx, *t.CurrentAttemptID); err != nil {
			return err
		}
		if a.SessionID == nil {
			return refusal(409, ReasonTaskNotRunning)
		}
		return nil
	})
	return t, a, err
}

// followUpStatus refuses the task statuses that never take a follow-up: an owner's cancel is final,
// a failed task is retried, and a completed code task's branch has merged, so more work on it is a
// new task.
func followUpStatus(t dbq.Task) error {
	switch t.Status {
	case dbstate.TaskCancelled, dbstate.TaskFailed:
		return refusal(409, ReasonTaskTerminal)
	case dbstate.TaskCompleted:
		if t.Mode == dbstate.TaskModeCode {
			return refusal(409, ReasonFollowUpCode)
		}
	}
	return nil
}

// prepareFollowUp moves the task and attempt so the follow-up's message is taken, under the task
// lock and the session's send lock. It returns the task as it is now and whether it was reopened.
func (s *Service) prepareFollowUp(ctx context.Context, q *dbq.Queries, p Principal, t dbq.Task, a dbq.TaskAttempt) (dbq.Task, bool, error) {
	if err := followUpStatus(t); err != nil {
		return t, false, err
	}
	// A cancel, retry or reassign that is draining this attempt wins: the follow-up would only be
	// cancelled by its stop.
	if open, err := q.ListOpenOperationsForSource(ctx, &a.ID); err != nil {
		return t, false, err
	} else if len(open) > 0 {
		return t, false, refusal(409, ReasonOperationPending)
	}
	if t.Status == dbstate.TaskCompleted {
		return s.reopen(ctx, q, t, a)
	}
	if a.State != dbstate.AttemptActive || a.StopState != stopNone {
		return t, false, refusal(409, ReasonTaskNotRunning)
	}
	if t.Status == dbstate.TaskRunning {
		return t, false, nil
	}
	if t.Status == dbstate.TaskQueued {
		return t, false, refusal(409, ReasonTaskNotRunning)
	}
	if t.Status == dbstate.TaskBlocked && t.Reason != nil && *t.Reason == dbstate.ReasonRuntimeLimit {
		// The runtime bound is the owner's to lift: their follow-up starts it again.
		if p.Role != "" {
			return t, false, refusal(409, ReasonRuntimeLimitOwner)
		}
		if err := q.RestartAttemptRuntime(ctx, a.ID); err != nil {
			return t, false, err
		}
	}
	from := TaskState{Status: t.Status}
	if t.Reason != nil {
		from.Reason = *t.Reason
	}
	if err := TransitionTask(from, TaskState{Status: dbstate.TaskRunning}); err != nil {
		return t, false, err
	}
	after, err := q.UpdateTaskState(ctx, dbq.UpdateTaskStateParams{ID: t.ID, Version: t.Version, Status: dbstate.TaskRunning})
	return after, false, err
}

// reopen moves a completed coordination task back to running on its settled attempt and session.
// A task whose attempt has not finished settling is refused for now: its stop is still being
// confirmed, and the attempt cannot take a message until it has.
func (s *Service) reopen(ctx context.Context, q *dbq.Queries, t dbq.Task, a dbq.TaskAttempt) (dbq.Task, bool, error) {
	if a.State != dbstate.AttemptCompleted || a.StopState != stopConfirmed {
		return t, false, refusal(409, ReasonTaskSettling)
	}
	if t.ParentTaskID != nil {
		parent, err := q.GetTaskByID(ctx, *t.ParentTaskID)
		if err != nil {
			return t, false, err
		}
		if TaskFinal(parent.Status) {
			return t, false, refusal(409, ReasonParentNotActive)
		}
		if err := s.checkCapacity(ctx, q, t.OwnerID, *t.ParentTaskID); err != nil {
			return t, false, err
		}
	}
	if n, err := q.ReopenTaskSession(ctx, *a.SessionID); err != nil {
		return t, false, err
	} else if n == 0 {
		return t, false, refusal(409, ReasonSessionArchived)
	}
	if _, err := q.ReopenAttempt(ctx, dbq.ReopenAttemptParams{ID: a.ID, EvidenceRef: "attempt-reopened:follow-up"}); errors.Is(err, pgx.ErrNoRows) {
		return t, false, refusal(409, ReasonTaskSettling)
	} else if err != nil {
		return t, false, err
	}
	after, err := q.ReopenTask(ctx, dbq.ReopenTaskParams{ID: t.ID, Version: t.Version})
	if errors.Is(err, pgx.ErrNoRows) {
		return t, false, refusal(409, ReasonStaleVersion)
	}
	return after, true, err
}

// sendRefusal maps the sessions ledger's refusal of the message to a task refusal.
func sendRefusal(err error) error {
	switch {
	case errors.Is(err, sessions.ErrNotFound), errors.Is(err, sessions.ErrConflict):
		return refusal(409, ReasonTaskNotRunning)
	case errors.Is(err, sessions.ErrInvalid):
		return refusal(422, ReasonInvalidRequest)
	}
	return err
}
