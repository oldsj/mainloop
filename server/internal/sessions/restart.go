package sessions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// RestartOutcome is what a restart did.
type RestartOutcome string

// Restart outcomes.
const (
	// Restarted means the native Session was deleted and its open turns failed.
	Restarted RestartOutcome = "restarted"
	// NoNativeSession means the binding had no native Session: nothing was running to delete.
	NoNativeSession RestartOutcome = "no_native_session"
)

// RestartResult is what a restart did, and the deliveries it settled as failed.
type RestartResult struct {
	Outcome RestartOutcome
	Failed  []string
}

// restartedDetail is the reason a turn that a restart ended is failed with.
const restartedDetail = "the native session was restarted before this turn finished; it was not sent again"

// restartDeleteTimeout bounds the native calls of one restart attempt.
const restartDeleteTimeout = 30 * time.Second

// Restart recovers a session whose turn kagent cannot stop (a wedged runtime that never answers
// CancelTask, so stop-turn cannot confirm anything). It deletes the session's native kagent Session
// and settles the turns that were open on it as failed, never resent: kagent no longer has the
// Session, so the message cannot still be running, and a failed turn releases the queue at once
// where an uncertain one would hold it. The next message creates a new Session, which carries the
// recent conversation (the existing replacement path); the native context is lost.
//
// It follows the reconciler rule. The intent is a session_restarts row keyed by requestID, written
// under the session's send lock before any kagent call, recording the Session to delete. The delete
// runs with no transaction open; kagent is read before every delete, so a repeat never deletes
// twice and never touches a newer Session. The turns are settled in one transaction with the queue
// promotion and the intent's completion. Repeating requestID returns the recorded outcome, or
// continues an interrupted restart; *KagentError is a delete kagent did not confirm, which a repeat
// or the resume sweep continues.
//
// The main thread and standalone native sessions can be restarted. A workspace's Session belongs
// to the workspace service, and a task attempt's runtime to the task's stop path, so both are a
// conflict, as is an ended or archived session and a main thread switching provider.
func (s *Service) Restart(ctx context.Context, sessionID, requestID string) (RestartResult, error) {
	if _, err := uuid.Parse(requestID); err != nil {
		return RestartResult{}, fmt.Errorf("%w: the request id is not a UUID", ErrInvalid)
	}
	if s.Kagent == nil {
		return RestartResult{}, &KagentError{Err: errors.New("kagent is not configured")}
	}
	r, err := s.beginRestart(ctx, sessionID, requestID)
	if err != nil {
		return RestartResult{}, err
	}
	return s.continueRestart(ctx, r)
}

// beginRestart returns the restart intent for requestID, writing it when it is new. A session that
// already has an open restart continues that one.
func (s *Service) beginRestart(ctx context.Context, sessionID, requestID string) (dbq.SessionRestart, error) {
	var out dbq.SessionRestart
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := db.XactLock(ctx, tx, "mainloop:send:"+sessionID); err != nil {
			return err
		}
		q := dbq.New(tx)
		b, err := q.GetOwnedSessionBinding(ctx, dbq.GetOwnedSessionBindingParams{ID: sessionID, UserID: s.OwnerID})
		if errors.Is(err, pgx.ErrNoRows) {
			if sess, serr := q.GetSession(ctx, sessionID); serr == nil && sess.UserID == s.OwnerID {
				return conflict("Session has no native agent binding")
			}
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("get binding: %w", err)
		}
		if r, err := q.GetSessionRestart(ctx, requestID); err == nil {
			if r.SessionID != sessionID {
				return fmt.Errorf("%w: the request id belongs to another session", ErrInvalid)
			}
			out = r
			return nil // a repeat: its eligibility was checked when it was written
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("get restart: %w", err)
		}
		if err := restartable(ctx, q, b); err != nil {
			return err
		}
		if r, err := q.GetOpenSessionRestart(ctx, sessionID); err == nil {
			out = r
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("get open restart: %w", err)
		}
		var ksid *string
		if b.KagentSessionID != nil && *b.KagentSessionID != "" {
			ksid = b.KagentSessionID
		}
		out, err = q.InsertSessionRestart(ctx, dbq.InsertSessionRestartParams{
			ID: requestID, SessionID: sessionID, KagentSessionID: ksid,
		})
		if err != nil {
			return fmt.Errorf("record restart: %w", err)
		}
		return nil
	})
	return out, err
}

// restartable is nil when Restart applies to the binding's session; q runs in the caller's
// transaction, which holds the session's send lock.
func restartable(ctx context.Context, q *dbq.Queries, b dbq.GetOwnedSessionBindingRow) error {
	switch {
	case b.ArchivedAt != nil:
		return conflict("This session is archived; start a new one.")
	case endedStatus(b.SessionStatus):
		return conflict("This session is %s; start a new one.", b.SessionStatus)
	case b.WorkspaceAgentName != nil:
		return conflict("A workspace's runtime is restarted through the workspace, not the session.")
	case b.Role == dbstate.RoleMain && b.MainPending:
		return conflictCode(codePending, "The main thread is switching to %s; restart it once the switch has settled.", b.Kind)
	}
	if _, err := q.LockSessionTaskAttempt(ctx, &b.SessionID); err == nil {
		return conflict("A task's runtime is stopped through the task; cancel or retry the task instead.")
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("check task attempt: %w", err)
	}
	return nil
}

// isRestartable reports whether Restart applies to the binding's session, for the native view.
func (s *Service) isRestartable(ctx context.Context, sessionID string) (bool, error) {
	ok := false
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		b, err := q.GetOwnedSessionBinding(ctx, dbq.GetOwnedSessionBindingParams{ID: sessionID, UserID: s.OwnerID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		err = restartable(ctx, q, b)
		if errors.Is(err, ErrConflict) {
			return nil
		}
		ok = err == nil
		return err
	})
	return ok && err == nil, err
}

// continueRestart deletes the intent's Session, unless kagent shows it already gone, and then
// settles the turns that ran on it. A done intent returns its recorded outcome.
func (s *Service) continueRestart(ctx context.Context, r dbq.SessionRestart) (RestartResult, error) {
	if r.State == dbstate.RestartDone {
		return restartResult(r), nil
	}
	if r.KagentSessionID != nil {
		if err := s.deleteNative(ctx, *r.KagentSessionID); err != nil {
			detail := safeDetail(err.Error())
			if nerr := dbq.New(s.Pool).NoteSessionRestartAttempt(ctx, dbq.NoteSessionRestartAttemptParams{
				ID: r.ID, Detail: &detail,
			}); nerr != nil {
				err = errors.Join(err, nerr)
			}
			s.log().WarnContext(ctx, "restart: native delete unconfirmed", "session_id", r.SessionID,
				"kagent_session_id", *r.KagentSessionID, "error", err)
			return RestartResult{}, &KagentError{Err: err}
		}
	}
	return s.finishRestart(ctx, r)
}

// deleteNative makes sure kagent no longer runs the Session: it is absent, deleted or being deleted
// (kagent has accepted the delete, which cannot be undone, and admits nothing to it). It reads the
// Session first and deletes only one that is still there, so a repeat does not delete twice.
func (s *Service) deleteNative(ctx context.Context, ksid string) error {
	ctx, cancel := context.WithTimeout(ctx, restartDeleteTimeout)
	defer cancel()
	sess, err := s.Kagent.ObserveSession(ctx, ksid)
	switch {
	case nativeAbsent(err):
		return nil
	case err != nil:
		return fmt.Errorf("read the kagent session: %w", err)
	case sessionEnded(sess):
		return nil
	}
	sess, err = s.Kagent.DeleteSession(ctx, ksid)
	switch {
	case nativeAbsent(err):
		return nil
	case err != nil:
		return fmt.Errorf("delete the kagent session: %w", err)
	case sessionEnded(sess):
		return nil
	}
	// The reply showed the Session as it was when the delete was taken: read it once more.
	sess, err = s.Kagent.ObserveSession(ctx, ksid)
	switch {
	case nativeAbsent(err):
		return nil
	case err != nil:
		return fmt.Errorf("read the kagent session after the delete: %w", err)
	case sessionEnded(sess):
		return nil
	}
	return fmt.Errorf("kagent still shows the session as %s after the delete", sess.GetState())
}

// nativeAbsent reports an answer kagent sent saying the Session does not exist.
func nativeAbsent(err error) bool {
	var se *kagent.SessionError
	return errors.As(err, &se) && se.Wire && se.Code == connect.CodeNotFound
}

// finishRestart settles a restart whose Session kagent no longer runs, in one transaction under the
// session's send lock: the binding forgets the Session (fenced on it, so a newer one is kept), every
// delivery sent or maybe sent to it is failed, the oldest queued message is admitted and the intent
// is done. A delivery the loop still holds is ended from outside its claim, so its next fenced write
// matches nothing.
func (s *Service) finishRestart(ctx context.Context, r dbq.SessionRestart) (RestartResult, error) {
	var out dbq.SessionRestart
	var ended []dbq.Delivery
	err := events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		ended = nil
		if err := db.XactLock(ctx, tx, "mainloop:send:"+r.SessionID); err != nil {
			return nil, err
		}
		q := dbq.New(tx)
		cur, err := q.GetSessionRestart(ctx, r.ID)
		if err != nil {
			return nil, fmt.Errorf("get restart: %w", err)
		}
		if cur.State == dbstate.RestartDone {
			out = cur
			return nil, nil // settled by a concurrent caller
		}
		var drafts []events.Draft
		failed := []string{}
		if r.KagentSessionID != nil {
			newReq := uuid.NewString()
			if _, err := q.ForgetDeletedKagentSession(ctx, dbq.ForgetDeletedKagentSessionParams{
				NewRequestID: &newReq, SessionID: r.SessionID, OldSessionID: *r.KagentSessionID,
			}); err != nil {
				return nil, fmt.Errorf("forget the deleted session: %w", err)
			}
			open, err := q.ListDeliveriesOnKagentSession(ctx, dbq.ListDeliveriesOnKagentSessionParams{
				SessionID: r.SessionID, KagentSessionID: *r.KagentSessionID,
			})
			if err != nil {
				return nil, fmt.Errorf("list open turns: %w", err)
			}
			detail := restartedDetail
			for _, d := range open {
				_, err := q.EndDelivery(ctx, dbq.EndDeliveryParams{
					ID: d.ID, FromStates: []string{string(dbstate.DeliverySending), string(dbstate.DeliveryDelivered), string(dbstate.DeliveryUncertain)},
					State: dbstate.DeliveryFailed, Detail: &detail,
				})
				if errors.Is(err, pgx.ErrNoRows) {
					continue
				}
				if err != nil {
					return nil, fmt.Errorf("fail delivery: %w", err)
				}
				ended = append(ended, d)
				failed = append(failed, d.ID)
				drafts = append(drafts, endedEvent(s.OwnerID, r.SessionID, d.ID))
			}
		}
		promoted, err := promoteNextQueued(ctx, q, r.SessionID, s.opts().UncertainHold.Seconds())
		if err != nil {
			return nil, err
		}
		if promoted != "" {
			if err := q.NotifyEvent(ctx, dbq.NotifyEventParams{Channel: DeliveryChannel, Payload: promoted}); err != nil {
				return nil, err
			}
		}
		if err := q.RefreshSessionStatus(ctx, r.SessionID); err != nil {
			return nil, fmt.Errorf("refresh session status: %w", err)
		}
		if _, err := q.FinishSessionRestart(ctx, dbq.FinishSessionRestartParams{ID: r.ID, FailedMessageIds: failed}); err != nil {
			return nil, fmt.Errorf("finish restart: %w", err)
		}
		out = cur
		out.State, out.FailedMessageIds = dbstate.RestartDone, failed
		return drafts, nil
	})
	if err != nil {
		return RestartResult{}, err
	}
	for _, d := range ended {
		s.endProgress(s.OwnerID, d.SessionID, d.ID, d.CreatedAt, "")
	}
	if len(ended) > 0 {
		s.log().InfoContext(ctx, "restart: native session deleted and its open turns failed",
			"session_id", r.SessionID, "failed", len(ended))
	}
	return restartResult(out), nil
}

func restartResult(r dbq.SessionRestart) RestartResult {
	out := RestartResult{Outcome: Restarted, Failed: r.FailedMessageIds}
	if r.KagentSessionID == nil {
		out.Outcome = NoNativeSession
	}
	if out.Failed == nil {
		out.Failed = []string{}
	}
	return out
}

// restartIdle is how long a requested restart is left alone before the sweep continues it, so the
// sweep does not race the request that is still running it.
const restartIdle = time.Minute

// RestartSweep continues requested restarts that were cut off or whose delete kagent did not
// confirm. It reads kagent before it deletes, like the request.
func (s *Service) RestartSweep() reconcile.Sweep {
	return reconcile.Sweep{Name: "resume-session-restarts", Every: 30 * time.Second, Run: s.ResumeRestarts}
}

// ResumeRestarts runs one pass of [Service.RestartSweep].
func (s *Service) ResumeRestarts(ctx context.Context) error {
	if s.Kagent == nil {
		return nil
	}
	rows, err := dbq.New(s.Pool).ListDueSessionRestarts(ctx, dbq.ListDueSessionRestartsParams{
		IdleSeconds: restartIdle.Seconds(), LimitCount: 20,
	})
	if err != nil {
		return fmt.Errorf("list due restarts: %w", err)
	}
	var errs []error
	for _, r := range rows {
		if _, err := s.continueRestart(ctx, r); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// kagentSessionGone reports whether kagent says the Session no longer exists or is being deleted,
// so nothing sent to it can still be running. A failed read is not an answer.
func (s *Service) kagentSessionGone(ctx context.Context, ksid string) bool {
	if ksid == "" {
		return false
	}
	sess, err := s.Kagent.ObserveSession(ctx, ksid)
	if err != nil {
		return nativeAbsent(err)
	}
	return sessionEnded(sess)
}
