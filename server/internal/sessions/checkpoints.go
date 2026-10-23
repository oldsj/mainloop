// Checkpoint and fork, on top of kagent's CheckpointService (issue #288, "warm-state comparison").
// Experimental and owner-only; see docs/specs/warm-state.md.
//
// Both operations follow the reconciler rule (AGENTS.md): a durable intent row, keyed on the
// caller's request_id, is written before the kagent call. kagent's CreateCheckpoint and ForkSession
// are themselves idempotent by request_id, the same contract CreateSession already has (see
// creates.go), so a retry after an uncertain outcome is a repeat of the same call rather than an
// observe-then-act dance: kagent answers from what it already holds for that request_id. A
// definite refusal (invalid argument, not found, failed precondition) is recorded once and never
// retried, since the frozen request would fail the same way again; only a genuinely uncertain
// outcome (OutcomeUnknown, unreachable, a 5xx) leaves the row open for the next attempt or the
// resume sweep.

package sessions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1alpha1"
)

// checkpointCallTimeout bounds one kagent CheckpointService call.
const checkpointCallTimeout = 30 * time.Second

// checkpointIdle is how long a pending checkpoint or an open fork is left alone before the resume
// sweep continues it, so the sweep does not race a request that is still running it.
const checkpointIdle = time.Minute

// CheckpointResult is a checkpoint as the API shows it.
type CheckpointResult struct {
	ID, SessionID, Name, HeadTaskID, State string
	RequestedAt                            time.Time
	ConfirmedAt                            *time.Time
}

// CheckpointView is a checkpoint read back from kagent (GetCheckpoint, ListCheckpoints), not tied
// to one of this process's own intents.
type CheckpointView struct {
	ID, SessionID, HeadTaskID, Name, State string
	CreatedAt                              time.Time
}

// ForkResult is a fork as the API shows it, with the timestamps time-to-running is read from.
type ForkResult struct {
	NewSessionID        string
	RequestedAt         time.Time
	AcceptedAt, ReadyAt *time.Time
}

// terminalDeliveryState is a turn checkpoints.go may name as a session's head task: it ran to an
// end, successful or not, so kagent has a task that will not change under it.
func terminalDeliveryState(st dbstate.DeliveryState) bool {
	return st == dbstate.DeliveryCompleted || st == dbstate.DeliveryFailed || st == dbstate.DeliveryCancelled
}

// classifyKagentFailure reports the definite, never-retried refusal err is, or "" when err is nil
// or an uncertain outcome that should be retried instead.
func classifyKagentFailure(err error) dbstate.FailureReason {
	var se *kagent.SessionError
	if !errors.As(err, &se) {
		return ""
	}
	switch se.Code {
	case connect.CodeInvalidArgument:
		return dbstate.FailureInvalidArgument
	case connect.CodeNotFound:
		return dbstate.FailureNotFound
	case connect.CodeFailedPrecondition:
		return dbstate.FailureFailedPrecondition
	default:
		return ""
	}
}

// mapFailureReason turns a recorded, definite refusal into the error the API maps to its status:
// invalid argument to 422, not found to 404, failed precondition to 409.
func mapFailureReason(reason dbstate.FailureReason, detail string) error {
	switch reason {
	case dbstate.FailureInvalidArgument:
		return fmt.Errorf("%w: %s", ErrInvalid, detail)
	case dbstate.FailureNotFound:
		return ErrNotFound
	case dbstate.FailureFailedPrecondition:
		return conflict("%s", detail)
	default:
		return errors.New(detail)
	}
}

// ---- checkpoint create -------------------------------------------------------------------------

// CreateCheckpoint checkpoints a session's native kagent Session at its latest terminal turn.
// Calling again with requestID continues an interrupted attempt or returns its recorded outcome.
func (s *Service) CreateCheckpoint(ctx context.Context, sessionID, requestID string) (CheckpointResult, error) {
	if _, err := uuid.Parse(requestID); err != nil {
		return CheckpointResult{}, fmt.Errorf("%w: the request id is not a UUID", ErrInvalid)
	}
	if s.Kagent == nil {
		return CheckpointResult{}, &KagentError{Err: errors.New("kagent is not configured")}
	}
	row, err := s.beginCheckpoint(ctx, sessionID, requestID)
	if err != nil {
		return CheckpointResult{}, err
	}
	return s.continueCheckpoint(ctx, row)
}

// beginCheckpoint returns the checkpoint intent for requestID, writing it when it is new. The
// session's send lock is held while the latest turn is read and frozen as expected_head_task_id,
// so a message that starts sending concurrently cannot land between the read and the write.
func (s *Service) beginCheckpoint(ctx context.Context, sessionID, requestID string) (dbq.SessionCheckpoint, error) {
	var out dbq.SessionCheckpoint
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := db.XactLock(ctx, tx, "mainloop:send:"+sessionID); err != nil {
			return err
		}
		q := dbq.New(tx)
		b, err := q.GetOwnedSessionBinding(ctx, dbq.GetOwnedSessionBindingParams{ID: sessionID, UserID: s.OwnerID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("get binding: %w", err)
		}
		if r, err := q.GetSessionCheckpoint(ctx, requestID); err == nil {
			if r.SessionID != sessionID {
				return fmt.Errorf("%w: the request id belongs to another session", ErrInvalid)
			}
			out = r
			return nil // a repeat: its eligibility was checked when it was written
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("get checkpoint intent: %w", err)
		}
		if b.ArchivedAt != nil {
			return conflict("This session is archived.")
		}
		if b.KagentSessionID == nil || *b.KagentSessionID == "" {
			return conflict("This session has no native session yet; nothing to checkpoint.")
		}
		d, err := q.GetLatestDelivery(ctx, sessionID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return conflict("This session has no completed turn to checkpoint yet.")
		case err != nil:
			return fmt.Errorf("get latest delivery: %w", err)
		case !terminalDeliveryState(d.State):
			return conflict("A turn is in progress; checkpoint once it finishes.")
		case d.TaskID == nil || *d.TaskID == "":
			return conflict("The session's latest turn has no task to checkpoint.")
		}
		out, err = q.InsertSessionCheckpoint(ctx, dbq.InsertSessionCheckpointParams{
			ID: requestID, SessionID: sessionID, KagentSessionID: *b.KagentSessionID, ExpectedHeadTaskID: *d.TaskID,
		})
		if err != nil {
			return fmt.Errorf("record checkpoint intent: %w", err)
		}
		return nil
	})
	return out, err
}

// continueCheckpoint calls kagent for a pending intent, or returns a done one's recorded outcome.
func (s *Service) continueCheckpoint(ctx context.Context, r dbq.SessionCheckpoint) (CheckpointResult, error) {
	if r.State == dbstate.CheckpointDone {
		if r.FailureReason != nil {
			return CheckpointResult{}, mapFailureReason(*r.FailureReason, derefStr(r.Detail))
		}
		// A repeat of an already-settled create reads the checkpoint's live state, the same as GET:
		// nothing here stores it, since it can change in kagent after this row settled.
		state := "unspecified"
		if cp, err := s.Kagent.GetCheckpoint(ctx, derefStr(r.CheckpointID)); err == nil {
			state = checkpointStateString(cp.GetState())
		}
		return checkpointResult(r, state), nil
	}
	cctx, cancel := context.WithTimeout(ctx, checkpointCallTimeout)
	defer cancel()
	cp, err := s.Kagent.CreateCheckpoint(cctx, &kagentapi.CreateCheckpointRequest{
		SessionId: r.KagentSessionID, RequestId: r.ID, ExpectedHeadTaskId: r.ExpectedHeadTaskID,
	})
	if reason := classifyKagentFailure(err); reason != "" {
		detail := safeDetail(err.Error())
		if _, ferr := s.finishSessionCheckpoint(ctx, r, nil, nil, &reason, &detail); ferr != nil {
			return CheckpointResult{}, ferr
		}
		return CheckpointResult{}, mapFailureReason(reason, detail)
	}
	if err != nil {
		detail := safeDetail(err.Error())
		if nerr := dbq.New(s.Pool).NoteSessionCheckpointAttempt(ctx, dbq.NoteSessionCheckpointAttemptParams{
			ID: r.ID, Detail: &detail,
		}); nerr != nil {
			err = errors.Join(err, nerr)
		}
		s.log().WarnContext(ctx, "checkpoint create: kagent outcome uncertain", "session_id", r.SessionID,
			"kagent_session_id", r.KagentSessionID, "error", err)
		return CheckpointResult{}, &KagentError{Err: err}
	}
	id, name := cp.GetId(), cp.GetName()
	out, err := s.finishSessionCheckpoint(ctx, r, &id, &name, nil, nil)
	if err != nil {
		return CheckpointResult{}, err
	}
	return checkpointResult(out, checkpointStateString(cp.GetState())), nil
}

// finishSessionCheckpoint settles a pending intent, or returns the row unchanged if a concurrent
// caller already settled it.
func (s *Service) finishSessionCheckpoint(
	ctx context.Context, r dbq.SessionCheckpoint, checkpointID, name *string, reason *dbstate.FailureReason, detail *string,
) (dbq.SessionCheckpoint, error) {
	var out dbq.SessionCheckpoint
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		cur, err := q.GetSessionCheckpoint(ctx, r.ID)
		if err != nil {
			return fmt.Errorf("get checkpoint intent: %w", err)
		}
		if cur.State == dbstate.CheckpointDone {
			out = cur
			return nil
		}
		if _, err := q.FinishSessionCheckpointDone(ctx, dbq.FinishSessionCheckpointDoneParams{
			ID: r.ID, CheckpointID: checkpointID, CheckpointName: name, FailureReason: reason, Detail: detail,
		}); err != nil {
			return fmt.Errorf("finish checkpoint intent: %w", err)
		}
		now := time.Now()
		cur.State, cur.CheckpointID, cur.CheckpointName = dbstate.CheckpointDone, checkpointID, name
		cur.FailureReason, cur.Detail, cur.ConfirmedAt = reason, detail, &now
		out = cur
		return nil
	})
	return out, err
}

func checkpointResult(r dbq.SessionCheckpoint, state string) CheckpointResult {
	return CheckpointResult{
		ID: derefStr(r.CheckpointID), SessionID: r.SessionID, Name: derefStr(r.CheckpointName),
		HeadTaskID: r.ExpectedHeadTaskID, State: state, RequestedAt: r.RequestedAt, ConfirmedAt: r.ConfirmedAt,
	}
}

// CheckpointSweep continues pending checkpoints whose kagent outcome was cut off or uncertain.
func (s *Service) CheckpointSweep() reconcile.Sweep {
	return reconcile.Sweep{Name: "resume-session-checkpoints", Every: 30 * time.Second, Run: s.ResumeCheckpoints}
}

// ResumeCheckpoints runs one pass of [Service.CheckpointSweep].
func (s *Service) ResumeCheckpoints(ctx context.Context) error {
	if s.Kagent == nil {
		return nil
	}
	rows, err := dbq.New(s.Pool).ListDueSessionCheckpoints(ctx, dbq.ListDueSessionCheckpointsParams{
		IdleSeconds: checkpointIdle.Seconds(), LimitCount: 20,
	})
	if err != nil {
		return fmt.Errorf("list due checkpoints: %w", err)
	}
	var errs []error
	for _, r := range rows {
		if _, err := s.continueCheckpoint(ctx, r); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ---- checkpoint read and delete ----------------------------------------------------------------

// sessionByKagentID is the owner's Mainloop session bound to a kagent Session, or ErrNotFound.
func (s *Service) sessionByKagentID(ctx context.Context, kagentSessionID string) (dbq.Session, error) {
	q := dbq.New(s.Pool)
	b, err := q.GetAgentBindingByKagentSession(ctx, &kagentSessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return dbq.Session{}, ErrNotFound
	}
	if err != nil {
		return dbq.Session{}, fmt.Errorf("get binding: %w", err)
	}
	sess, err := q.GetSession(ctx, b.SessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return dbq.Session{}, ErrNotFound
	}
	if err != nil {
		return dbq.Session{}, fmt.Errorf("get session: %w", err)
	}
	if sess.UserID != s.OwnerID {
		return dbq.Session{}, ErrNotFound
	}
	return sess, nil
}

func toCheckpointView(cp *kagentapi.Checkpoint) CheckpointView {
	return CheckpointView{
		ID: cp.GetId(), SessionID: cp.GetSessionId(), HeadTaskID: cp.GetHeadTaskId(), Name: cp.GetName(),
		State: checkpointStateString(cp.GetState()), CreatedAt: cp.GetCreatedAt().AsTime(),
	}
}

func checkpointStateString(st kagentapi.CheckpointState) string {
	switch st {
	case kagentapi.CheckpointState_CHECKPOINT_STATE_CREATING:
		return "creating"
	case kagentapi.CheckpointState_CHECKPOINT_STATE_READY:
		return "ready"
	case kagentapi.CheckpointState_CHECKPOINT_STATE_FAILED:
		return "failed"
	case kagentapi.CheckpointState_CHECKPOINT_STATE_DELETING:
		return "deleting"
	default:
		return "unspecified"
	}
}

// GetCheckpoint reads one checkpoint, scoped to the owner's own session.
func (s *Service) GetCheckpoint(ctx context.Context, checkpointID string) (CheckpointView, error) {
	if s.Kagent == nil {
		return CheckpointView{}, ErrUnavailable
	}
	cp, err := s.Kagent.GetCheckpoint(ctx, checkpointID)
	if nativeAbsent(err) {
		return CheckpointView{}, ErrNotFound
	}
	if err != nil {
		return CheckpointView{}, &KagentError{Err: err}
	}
	if _, err := s.sessionByKagentID(ctx, cp.GetSessionId()); err != nil {
		return CheckpointView{}, err
	}
	return toCheckpointView(cp), nil
}

// ListCheckpoints reads every checkpoint of the owner's session. A session with no native Session
// yet has none.
func (s *Service) ListCheckpoints(ctx context.Context, sessionID string) ([]CheckpointView, error) {
	if s.Kagent == nil {
		return nil, ErrUnavailable
	}
	b, err := dbq.New(s.Pool).GetOwnedSessionBinding(ctx, dbq.GetOwnedSessionBindingParams{ID: sessionID, UserID: s.OwnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get binding: %w", err)
	}
	if b.KagentSessionID == nil || *b.KagentSessionID == "" {
		return []CheckpointView{}, nil
	}
	cps, err := s.Kagent.ListCheckpoints(ctx, *b.KagentSessionID)
	if err != nil {
		return nil, &KagentError{Err: err}
	}
	out := make([]CheckpointView, len(cps))
	for i, cp := range cps {
		out[i] = toCheckpointView(cp)
	}
	return out, nil
}

// DeleteCheckpoint deletes a checkpoint, scoped to the owner's own session. One that is already
// gone is not an error.
func (s *Service) DeleteCheckpoint(ctx context.Context, checkpointID string) error {
	if s.Kagent == nil {
		return ErrUnavailable
	}
	cp, err := s.Kagent.GetCheckpoint(ctx, checkpointID)
	if nativeAbsent(err) {
		return nil
	}
	if err != nil {
		return &KagentError{Err: err}
	}
	if _, err := s.sessionByKagentID(ctx, cp.GetSessionId()); err != nil {
		return err
	}
	if err := s.Kagent.DeleteCheckpoint(ctx, checkpointID); err != nil {
		if nativeAbsent(err) {
			// kagent's BeginDeleteSessionCheckpoint answers NotFound both for a checkpoint that is
			// genuinely gone and for one a fork still pins (issue #311): its own deletion skips a
			// pinned checkpoint instead of refusing outright. A fresh read tells which; answering 200
			// here while the checkpoint still exists would not be true.
			return s.checkStillPinned(ctx, checkpointID)
		}
		var se *kagent.SessionError
		if errors.As(err, &se) && se.Code == connect.CodeFailedPrecondition {
			return conflict("%s", safeDetail(se.Detail))
		}
		return &KagentError{Err: err}
	}
	return nil
}

// checkStillPinned re-reads a checkpoint kagent just answered NotFound for deleting, to tell a
// checkpoint that is truly gone (not an error) from one a fork still pins (409, naming the pinning
// fork session(s) when Mainloop's own fork records know them).
func (s *Service) checkStillPinned(ctx context.Context, checkpointID string) error {
	cp, err := s.Kagent.GetCheckpoint(ctx, checkpointID)
	if nativeAbsent(err) {
		return nil
	}
	if err != nil {
		return &KagentError{Err: err}
	}
	detail := "checkpoint is pinned by a fork"
	if rows, err := dbq.New(s.Pool).ListForksByCheckpoint(ctx, cp.GetId()); err == nil && len(rows) > 0 {
		ids := make([]string, 0, len(rows))
		for _, id := range rows {
			if id != nil {
				ids = append(ids, *id)
			}
		}
		if len(ids) > 0 {
			word := "session"
			if len(ids) > 1 {
				word = "sessions"
			}
			detail = "checkpoint is pinned by fork " + word + " " + strings.Join(ids, ", ")
		}
	}
	return conflict("%s", detail)
}

// ---- fork ---------------------------------------------------------------------------------------

// Fork forks a checkpoint into a new Mainloop session bound to the forked kagent Session, with the
// same project, provider profile and Agent as the checkpoint's source session, and parent_session_id
// set to it. Calling again with requestID continues an interrupted attempt or returns its recorded
// outcome.
func (s *Service) Fork(ctx context.Context, checkpointID, requestID string, freshConversation bool, title string) (ForkResult, error) {
	if _, err := uuid.Parse(requestID); err != nil {
		return ForkResult{}, fmt.Errorf("%w: the request id is not a UUID", ErrInvalid)
	}
	if s.Kagent == nil {
		return ForkResult{}, &KagentError{Err: errors.New("kagent is not configured")}
	}
	row, err := s.beginFork(ctx, checkpointID, requestID, freshConversation, title)
	if err != nil {
		return ForkResult{}, err
	}
	return s.continueFork(ctx, row)
}

// beginFork returns the fork intent for requestID, writing it when it is new. Resolving the
// checkpoint's source session needs a kagent read (GetCheckpoint); the intent row is written only
// after that read settles, never while holding it across a transaction.
func (s *Service) beginFork(ctx context.Context, checkpointID, requestID string, fresh bool, title string) (dbq.CheckpointFork, error) {
	if r, err := dbq.New(s.Pool).GetCheckpointFork(ctx, requestID); err == nil {
		if r.CheckpointID != checkpointID {
			return dbq.CheckpointFork{}, fmt.Errorf("%w: the request id belongs to another checkpoint", ErrInvalid)
		}
		return r, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return dbq.CheckpointFork{}, fmt.Errorf("get fork intent: %w", err)
	}
	cp, err := s.Kagent.GetCheckpoint(ctx, checkpointID)
	if nativeAbsent(err) {
		return dbq.CheckpointFork{}, ErrNotFound
	}
	if err != nil {
		return dbq.CheckpointFork{}, &KagentError{Err: err}
	}
	src, err := s.sessionByKagentID(ctx, cp.GetSessionId())
	if err != nil {
		return dbq.CheckpointFork{}, err
	}
	var out dbq.CheckpointFork
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		// A concurrent caller may have written the intent between the read above and here.
		if r, err := q.GetCheckpointFork(ctx, requestID); err == nil {
			out = r
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("get fork intent: %w", err)
		}
		out, err = q.InsertCheckpointFork(ctx, dbq.InsertCheckpointForkParams{
			ID: requestID, CheckpointID: checkpointID, SourceSessionID: src.ID, FreshConversation: fresh, Title: title,
		})
		if err != nil {
			return fmt.Errorf("record fork intent: %w", err)
		}
		return nil
	})
	return out, err
}

// continueFork drives a fork intent forward one step: requested calls kagent's ForkSession and
// creates the new session and binding; accepted tries, once, to observe the forked runtime ready.
// Neither step blocks long: an interrupted or slow fork is picked up again by a repeat request or
// the resume sweep.
func (s *Service) continueFork(ctx context.Context, r dbq.CheckpointFork) (ForkResult, error) {
	if r.State == dbstate.ForkFailed {
		return ForkResult{}, mapFailureReason(*r.FailureReason, derefStr(r.Detail))
	}
	if r.State == dbstate.ForkRequested {
		next, err := s.acceptFork(ctx, r)
		if err != nil {
			return ForkResult{}, err
		}
		r = next
	}
	if r.State == dbstate.ForkAccepted {
		if next, ok := s.observeForkReady(ctx, r); ok {
			r = next
		}
	}
	return forkResult(r), nil
}

// acceptFork calls kagent's ForkSession for a requested intent and creates the new session.
func (s *Service) acceptFork(ctx context.Context, r dbq.CheckpointFork) (dbq.CheckpointFork, error) {
	cctx, cancel := context.WithTimeout(ctx, checkpointCallTimeout)
	defer cancel()
	forked, err := s.Kagent.ForkSession(cctx, &kagentapi.ForkSessionRequest{
		CheckpointId: r.CheckpointID, RequestId: r.ID, FreshConversation: r.FreshConversation,
	})
	if reason := classifyKagentFailure(err); reason != "" {
		detail := safeDetail(err.Error())
		if _, ferr := s.finishForkFailed(ctx, r, reason, detail); ferr != nil {
			return dbq.CheckpointFork{}, ferr
		}
		return dbq.CheckpointFork{}, mapFailureReason(reason, detail)
	}
	if err != nil {
		detail := safeDetail(err.Error())
		if nerr := dbq.New(s.Pool).NoteCheckpointForkAttempt(ctx, dbq.NoteCheckpointForkAttemptParams{
			ID: r.ID, Detail: &detail,
		}); nerr != nil {
			err = errors.Join(err, nerr)
		}
		s.log().WarnContext(ctx, "fork: kagent outcome uncertain", "checkpoint_id", r.CheckpointID, "error", err)
		return dbq.CheckpointFork{}, &KagentError{Err: err}
	}
	return s.commitFork(ctx, r, forked)
}

// commitFork creates the forked session and its binding, bound to the kagent Session ForkSession
// returned, and finishes the fork intent in the same transaction. A concurrent caller that already
// did this is left unchanged.
func (s *Service) commitFork(ctx context.Context, r dbq.CheckpointFork, forked *kagentapi.Session) (dbq.CheckpointFork, error) {
	q := dbq.New(s.Pool)
	src, err := q.GetSession(ctx, r.SourceSessionID)
	if err != nil {
		return dbq.CheckpointFork{}, fmt.Errorf("get source session: %w", err)
	}
	srcBinding, err := q.GetAgentBinding(ctx, r.SourceSessionID)
	if err != nil {
		return dbq.CheckpointFork{}, fmt.Errorf("get source binding: %w", err)
	}
	// A forked session whose source is a workspace (a git checkout bound to a kagent Session; not
	// every session has one, see completedSession in the tests) gets its own workspace row too, so
	// it is a first-class workspace session (issue #311): GET, suspend, resume, idle suspend and
	// delete all work on it like any other workspace, and delete releases the forked kagent Session
	// through that ordinary path. The manifest is copied from the source; the forked binding
	// inserted below already carries the forked kagent Session id, so no create is ever sent for
	// this workspace row. The fork has no Mainloop MCP credential (MCPGrantKind stays none, below):
	// kagent's ForkSession carries no credential reference to attach one to (see
	// docs/specs/warm-state.md and #311).
	srcWorkspace, err := q.GetWorkspaceForOp(ctx, r.SourceSessionID)
	hasWorkspace := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return dbq.CheckpointFork{}, fmt.Errorf("get source workspace: %w", err)
	}
	title := r.Title
	if title == "" {
		title = "Fork of " + src.Title
	}
	forkedID := forked.GetId()
	newID := uuid.NewString()
	var out dbq.CheckpointFork
	err = db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		cur, err := q.GetCheckpointFork(ctx, r.ID)
		if err != nil {
			return fmt.Errorf("get fork intent: %w", err)
		}
		if cur.State != dbstate.ForkRequested {
			out = cur
			return nil // settled by a concurrent caller
		}
		if _, err := q.CreateSession(ctx, dbq.CreateSessionParams{
			ID: newID, UserID: s.OwnerID, ProjectID: src.ProjectID, Title: title, Description: src.Description,
			RepoURL: src.RepoURL, BranchName: src.BranchName, BaseBranch: src.BaseBranch, Color: src.Color,
		}); err != nil {
			return fmt.Errorf("create forked session: %w", err)
		}
		ref := forked.GetAgent()
		var ns, name *string
		if v := ref.GetNamespace(); v != "" {
			ns = &v
		}
		if v := ref.GetName(); v != "" {
			name = &v
		}
		if _, err := q.CreateForkedAgentBinding(ctx, dbq.CreateForkedAgentBindingParams{
			SessionID: newID, Kind: srcBinding.Kind, Role: dbstate.RoleAgent, MCPGrantKind: dbstate.GrantNone,
			ParentSessionID: &r.SourceSessionID, KagentSessionID: &forkedID,
			KagentAgentNamespace: ns, KagentAgentName: name,
		}); err != nil {
			return fmt.Errorf("create forked binding: %w", err)
		}
		if hasWorkspace {
			agentNamespace, agentName := srcWorkspace.AgentNamespace, srcWorkspace.AgentName
			if v := ref.GetNamespace(); v != "" {
				agentNamespace = v
			}
			if v := ref.GetName(); v != "" {
				agentName = v
			}
			if _, err := q.InsertWorkspace(ctx, dbq.InsertWorkspaceParams{
				ID: newID, UserID: s.OwnerID, ProjectID: srcWorkspace.ProjectID, RepoURL: srcWorkspace.RepoURL,
				Ref: srcWorkspace.Ref, Branch: srcWorkspace.Branch, Depth: srcWorkspace.Depth,
				AgentKind: srcWorkspace.AgentKind, AgentNamespace: agentNamespace, AgentName: agentName,
				Ports: srcWorkspace.Ports, IdleTimeoutMinutes: srcWorkspace.IdleTimeoutMinutes,
				Environment: srcWorkspace.Environment, RequestID: uuid.NewString(),
			}); err != nil {
				return fmt.Errorf("create forked workspace: %w", err)
			}
		}
		if _, err := q.SetSessionStatus(ctx, dbq.SetSessionStatusParams{ID: newID, Status: dbstate.SessionActive}); err != nil {
			return fmt.Errorf("activate forked session: %w", err)
		}
		if _, err := q.FinishCheckpointForkAccepted(ctx, dbq.FinishCheckpointForkAcceptedParams{
			ID: r.ID, ForkedKagentSessionID: &forkedID, NewSessionID: &newID,
		}); err != nil {
			return fmt.Errorf("finish fork intent: %w", err)
		}
		now := time.Now()
		cur.State, cur.ForkedKagentSessionID, cur.NewSessionID = dbstate.ForkAccepted, &forkedID, &newID
		cur.AcceptedAt = &now
		out = cur
		return nil
	})
	return out, err
}

func (s *Service) finishForkFailed(ctx context.Context, r dbq.CheckpointFork, reason dbstate.FailureReason, detail string) (dbq.CheckpointFork, error) {
	var out dbq.CheckpointFork
	err := db.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		cur, err := q.GetCheckpointFork(ctx, r.ID)
		if err != nil {
			return fmt.Errorf("get fork intent: %w", err)
		}
		if cur.State != dbstate.ForkRequested {
			out = cur
			return nil
		}
		if _, err := q.FinishCheckpointForkFailed(ctx, dbq.FinishCheckpointForkFailedParams{
			ID: r.ID, FailureReason: &reason, Detail: &detail,
		}); err != nil {
			return fmt.Errorf("finish fork intent: %w", err)
		}
		cur.State, cur.FailureReason, cur.Detail = dbstate.ForkFailed, &reason, &detail
		out = cur
		return nil
	})
	return out, err
}

// observeForkReady makes one best-effort, bounded check of whether the forked runtime is READY,
// so a fast fork can answer with ready_at set without the caller polling. A slow or failed check
// changes nothing; the resume sweep or a repeat request tries again.
func (s *Service) observeForkReady(ctx context.Context, r dbq.CheckpointFork) (dbq.CheckpointFork, bool) {
	octx, cancel := context.WithTimeout(ctx, sessionStateTimeout)
	defer cancel()
	sess, err := s.Kagent.ObserveSession(octx, *r.ForkedKagentSessionID)
	if err != nil || sess.GetState() != kagentapi.RuntimeState_RUNTIME_STATE_READY {
		return r, false
	}
	n, err := dbq.New(s.Pool).FinishCheckpointForkReady(ctx, r.ID)
	if err != nil || n == 0 {
		return r, false
	}
	now := time.Now()
	r.State, r.ReadyAt = dbstate.ForkReady, &now
	return r, true
}

func forkResult(r dbq.CheckpointFork) ForkResult {
	return ForkResult{NewSessionID: derefStr(r.NewSessionID), RequestedAt: r.RequestedAt, AcceptedAt: r.AcceptedAt, ReadyAt: r.ReadyAt}
}

// ForkSweep continues open forks whose kagent outcome was cut off, uncertain, or whose runtime was
// not yet observed ready.
func (s *Service) ForkSweep() reconcile.Sweep {
	return reconcile.Sweep{Name: "resume-checkpoint-forks", Every: 30 * time.Second, Run: s.ResumeForks}
}

// ResumeForks runs one pass of [Service.ForkSweep].
func (s *Service) ResumeForks(ctx context.Context) error {
	if s.Kagent == nil {
		return nil
	}
	rows, err := dbq.New(s.Pool).ListDueCheckpointForks(ctx, dbq.ListDueCheckpointForksParams{
		IdleSeconds: checkpointIdle.Seconds(), LimitCount: 20,
	})
	if err != nil {
		return fmt.Errorf("list due forks: %w", err)
	}
	var errs []error
	for _, r := range rows {
		if _, err := s.continueFork(ctx, r); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
