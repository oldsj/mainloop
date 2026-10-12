package workspaces

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/gittransport"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// opChannel wakes the operation loop when an operation is queued.
const opChannel = "workspace_ops"

// maxReason bounds what is stored in workspace_ops.last_error.
const maxReason = 500

// Table is the intent table the lifecycle loop claims from.
func Table() reconcile.Table {
	return reconcile.NewTable("workspace_ops", string(dbstate.OpPending), string(dbstate.OpRetrying))
}

// Loop is the reconcile loop that drives queued operations.
func (s *Service) Loop() reconcile.Loop {
	w := s.Config.Workspaces
	return reconcile.Loop{
		Name: "workspace-ops", Table: Table(), Handle: s.handle, Wake: []string{opChannel},
		Backoff: reconcile.Backoff{Base: w.CreateRetryInitial.Duration(), Max: w.CreateRetryMax.Duration()},
	}
}

// Register adds the lifecycle loop and the periodic sweeps to the runner.
func (s *Service) Register(r *reconcile.Runner) {
	r.Add(s.Loop())
	r.Add(s.pushEnrollmentLoop())
	r.AddSweep(reconcile.Sweep{Name: "workspace-push-release", Every: time.Minute, Run: s.releaseEndedPush})
	for _, sw := range s.sweeps() {
		r.AddSweep(sw)
	}
}

// OrphanSweepName is the sweep lease the scheduled orphan sweep and `sweep-orphans --delete` share, so
// the two cannot delete at the same time.
const OrphanSweepName = "workspace-orphans"

func (s *Service) sweeps() []reconcile.Sweep {
	w := s.Config.Workspaces
	out := []reconcile.Sweep{
		{Name: "workspace-archived", Every: w.ArchivedDeleteInterval.Duration(), Run: s.DeleteArchived},
		{Name: OrphanSweepName, Every: w.OrphanSweepInterval.Duration(), Run: s.scheduledOrphanSweep},
	}
	// Without the delivery ledger neither the open-turn check nor delivery activity exists, so the
	// sweep would suspend a workspace in the middle of a turn. Interim: it stays off until the
	// ledger is wired.
	if s.Deliveries == nil {
		s.Log.Warn("idle suspend is off: no delivery ledger is wired, so a running turn cannot be seen")
	} else {
		out = append(out, reconcile.Sweep{Name: "workspace-idle", Every: w.IdleCheckInterval.Duration(), Run: s.SuspendIdle})
	}
	return out
}

// scheduledOrphanSweep deletes orphans and says so when it could not, since nothing else is
// watching the scheduled pass.
func (s *Service) scheduledOrphanSweep(ctx context.Context) error {
	rep, err := s.SweepOrphans(ctx, OrphanOptions{Delete: true})
	if rep.Blocked != "" {
		s.Log.WarnContext(ctx, "orphan sweep blocked", "reason", rep.Blocked, "orphans", len(rep.Orphans))
	}
	if len(rep.Deleted) > 0 {
		s.Log.InfoContext(ctx, "orphan sessions deleted", "count", len(rep.Deleted))
	}
	return err
}

// result is what a handler learned. A zero state with err set is a transient failure.
type result struct {
	state  dbstate.WorkspaceOpState
	reason string
	err    error
	sess   *kagentapi.Session
	bind   string // Session id to record on the binding
	touch  bool   // counts as owner activity
	idle   bool   // the workspace was parked
	purge  bool   // the workspace is gone: delete its rows
	// unchanged: the operation found the workspace already in the state it asked for. Nothing is
	// published for it on its first attempt; a retried attempt may be finishing what an earlier one
	// did without recording it, so that one is.
	unchanged bool
}

func (s *Service) handle(ctx context.Context, c reconcile.Claim) (reconcile.Outcome, error) {
	q := dbq.New(s.Pool)
	op, err := q.GetWorkspaceOp(ctx, c.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return reconcile.Recorded(), nil // the workspace was deleted under it
	}
	if err != nil {
		return reconcile.Retry(), err
	}
	if op.State != dbstate.OpPending && op.State != dbstate.OpRetrying {
		return reconcile.Recorded(), nil // cancelled while queued
	}
	row, err := q.GetWorkspaceForOp(ctx, op.WorkspaceID)
	if err != nil {
		return reconcile.Retry(), err
	}
	w := wsRow(row)
	var res result
	switch op.Kind {
	case dbstate.OpCreate:
		res = s.doCreate(ctx, w)
	case dbstate.OpSuspend:
		res = s.doSuspend(ctx, op, w)
	case dbstate.OpResume:
		res = s.doResume(ctx, w)
	case dbstate.OpDelete:
		res = s.doDelete(ctx, w)
	default:
		res = result{state: dbstate.OpRejected, reason: "unknown operation"}
	}
	if ctx.Err() != nil {
		return reconcile.Retry(), ctx.Err() // shutting down or lease lost: the next claim observes again
	}
	return reconcile.Recorded(), s.finalize(ctx, c, op, w, res)
}

// finalize records the outcome fenced on the claim, with the domain change and the event in one
// transaction. A claim that no longer holds the row (cancelled, lease lost) records nothing.
func (s *Service) finalize(ctx context.Context, c reconcile.Claim, op dbq.WorkspaceOp, w wsRow, res result) error {
	kind := op.Kind
	retry, delay := false, time.Duration(0)
	if res.state == "" {
		perm, reason := classify(res.err)
		res.reason = reason
		switch {
		case perm:
			res.state = dbstate.OpRejected
		case op.FirstFailedAt != nil && s.now().Sub(*op.FirstFailedAt) >= s.Config.Workspaces.CreateRetryWindow.Duration():
			res.state = dbstate.OpGaveUp
		default:
			retry, delay = true, s.retryDelay(int(op.Failures)+1)
		}
		s.Log.WarnContext(ctx, "workspace operation failed", "workspace", w.ID, "op", op.ID,
			"kind", op.Kind, "state", res.state, "retry", retry, "error", res.err)
	}
	reason := clip(res.reason)
	var lastError *string
	if reason != "" {
		lastError = &reason
	}
	quiet := res.unchanged && c.Attempts <= 1 && op.Failures == 0
	publish := kind == dbstate.OpCreate || (res.state == dbstate.OpSucceeded && !res.purge && !quiet)
	attempts := int32(c.Attempts) //nolint:gosec // a claim count, far below 2^31
	err := s.settle(ctx, w, res.sess, publish, func(q *dbq.Queries) (bool, error) {
		var n int64
		var err error
		if retry {
			n, err = q.RetryWorkspaceOp(ctx, dbq.RetryWorkspaceOpParams{
				ID: c.ID, LeaseOwner: &c.Owner, Attempts: attempts, LastError: lastError,
				DelaySeconds: delay.Seconds(),
			})
		} else {
			n, err = q.FinishWorkspaceOp(ctx, dbq.FinishWorkspaceOpParams{
				ID: c.ID, LeaseOwner: &c.Owner, Attempts: attempts,
				State: res.state, LastError: lastError,
			})
		}
		if err != nil || n == 0 {
			return false, err
		}
		if kind == dbstate.OpCreate && (res.state == dbstate.OpRejected || res.state == dbstate.OpGaveUp || res.state == dbstate.OpCancelled) {
			if err := revokePush(ctx, q, w.ID); err != nil {
				return false, err
			}
		}
		if res.bind != "" {
			if n, err := q.BindWorkspaceSession(ctx, dbq.BindWorkspaceSessionParams{SessionID: w.ID, KagentSessionID: &res.bind}); err != nil {
				return false, err
			} else if n == 0 {
				return false, fmt.Errorf("workspace %s is already bound to another kagent session", w.ID)
			}
		}
		if res.touch {
			if _, err := q.TouchWorkspace(ctx, w.ID); err != nil {
				return false, err
			}
		}
		if res.idle {
			if err := q.MarkWorkspaceIdleSuspended(ctx, w.ID); err != nil {
				return false, err
			}
		}
		if res.purge {
			if err := revokePush(ctx, q, w.ID); err != nil {
				return false, err
			}
			// The session row goes first: an Issue that planned a credential holds it FOR SHARE, so the
			// delete waits for that plan to commit and the revoke after it sees the row.
			if _, err := q.DeleteSession(ctx, w.ID); err != nil {
				return false, err
			}
			if _, err := agentauth.RevokeInTx(ctx, q, w.ID); err != nil {
				return false, err
			}
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("record %s %s: %w", op.Kind, op.ID, err)
	}
	return nil
}

// settle runs one outcome write and the event it implies in one transaction. apply reports false
// when the claim no longer holds the row; nothing is then published.
func (s *Service) settle(ctx context.Context, w wsRow, sess *kagentapi.Session, publish bool, apply func(q *dbq.Queries) (bool, error)) error {
	return events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		q := dbq.New(tx)
		ok, err := apply(q)
		if err != nil || !ok || !publish {
			return nil, err
		}
		bumped, err := q.BumpWorkspace(ctx, w.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // purged
		}
		if err != nil {
			return nil, err
		}
		w.UpdatedAt, w.LastActiveAt = bumped.UpdatedAt, bumped.LastActiveAt
		var create *dbq.WorkspaceOp
		if op, err := q.GetLatestCreateOp(ctx, w.ID); err == nil {
			create = &op
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		lc := project(w, sess, create, time.Time{})
		return []events.Draft{{UserID: w.UserID, Type: events.TypeWorkspaceUpdated, Payload: api.EventWorkspaceUpdated{Workspace: lc}}}, nil
	})
}

func (s *Service) retryDelay(failures int) time.Duration {
	cfg := s.Config.Workspaces
	d := cfg.CreateRetryInitial.Duration()
	for i := 1; i < failures && d < cfg.CreateRetryMax.Duration(); i++ {
		d *= 2
	}
	return min(d, cfg.CreateRetryMax.Duration())
}

// transient marks an error that must be retried whatever kagent's status says.
type transient struct{ err error }

func (e *transient) Error() string { return e.err.Error() }
func (e *transient) Unwrap() error { return e.err }

// unconfirmed is kagent answering without proving the operation took effect.
type unconfirmed struct{ detail string }

func (e *unconfirmed) Error() string { return e.detail }

// classify says whether an error will never succeed on retry, with a reason fit to show.
func classify(err error) (permanent bool, reason string) {
	var tr *transient
	if errors.As(err, &tr) {
		_, reason = classify(tr.err)
		return false, reason
	}
	var cfg *kagent.ConfigError
	var se *kagent.SessionError
	var un *kagent.Unreachable
	var ou *kagent.OutcomeUnknown
	var uc *unconfirmed
	switch {
	case errors.Is(err, gittransport.ErrWorkspaceEnded):
		return true, "the workspace has ended"
	case errors.Is(err, agentauth.ErrBindingEnded), errors.Is(err, agentauth.ErrInvalidGrant):
		return true, "the workspace's agent credential cannot be issued"
	case errors.Is(err, agentauth.ErrCleanupPending), errors.Is(err, agentauth.ErrRevoked):
		return false, "the workspace's previous agent credential is still being cleaned up"
	case errors.As(err, &cfg):
		return true, "kagent refused Mainloop's credentials"
	case errors.As(err, &se):
		switch se.Code {
		case connect.CodeInvalidArgument:
			return true, "kagent rejected the workspace request"
		case connect.CodePermissionDenied, connect.CodeUnauthenticated:
			return true, "kagent refused the request"
		case connect.CodeNotFound:
			return true, "kagent does not have the agent or session"
		case connect.CodeAlreadyExists:
			return true, "kagent already used the request id for a different workspace"
		case connect.CodeUnimplemented:
			return true, "kagent does not support the request"
		case connect.CodeFailedPrecondition:
			// Only a request id that belongs to a deleted Session is permanent: a replay cannot
			// bring it back. kagent uses the same code while an Agent has no ready revision, its
			// golden preparation failed or composition is not configured, and those clear up.
			if strings.Contains(strings.ToLower(se.Detail), "deleted") {
				return true, "the runtime session for this request was deleted"
			}
			return false, "kagent is not ready for this change"
		}
		return false, "kagent reported an error"
	case errors.As(err, &un):
		return false, "kagent is unreachable"
	case errors.As(err, &ou):
		return false, "kagent did not answer"
	case errors.As(err, &uc):
		return false, uc.detail
	}
	return false, "unexpected error"
}

func clip(s string) string {
	if len(s) > maxReason {
		return s[:maxReason]
	}
	return s
}

// refused is kagent rejecting a create outright: an answer about the request, not about whether
// kagent or the network is healthy. Credential and configuration refusals say nothing about
// whether an earlier attempt made a Session, so they are not refusals here.
func refused(err error) bool {
	var se *kagent.SessionError
	if !errors.As(err, &se) {
		return false
	}
	perm, _ := classify(err)
	return perm
}

// replayFoundNoSession is the delete's rule for a replayed create: it answered with a definite
// refusal, so kagent looked up the workspace's request id, found no Session holding it and would
// not make one. There is nothing to delete. A refusal about the request (see refused) and a
// FAILED_PRECONDITION of any kind both mean that; kagent also uses FAILED_PRECONDITION while an
// Agent is not ready, which is still an answer given after the lookup found nothing.
func replayFoundNoSession(err error) bool {
	var se *kagent.SessionError
	if !errors.As(err, &se) {
		return false
	}
	return se.Code == connect.CodeFailedPrecondition || refused(err)
}

func notFoundErr(err error) bool {
	var se *kagent.SessionError
	return errors.As(err, &se) && se.Code == connect.CodeNotFound
}

// createRequest is the one request every attempt sends: the agent was resolved and stored when the
// workspace was created, so later config changes cannot make a replay a different request.
func (w wsRow) createRequest() *kagentapi.CreateSessionRequest {
	req := &kagentapi.CreateSessionRequest{
		Agent:     &kagentapi.ResourceReference{Namespace: w.AgentNamespace, Name: w.AgentName},
		RequestId: w.RequestID,
		Workspace: &kagentapi.Workspace{Repo: w.RepoURL, Ref: w.Ref, Branch: w.Branch, Depth: w.Depth},
	}
	if e := w.environment(); e != nil {
		req.DevelopmentEnvironment = &kagentapi.DevelopmentEnvironment{
			Image: e.Image, Platform: e.Platform, PolicyIdentity: e.PolicyIdentity,
		}
	}
	return req
}

func (s *Service) doCreate(ctx context.Context, w wsRow) result {
	if w.KagentSessionID != nil {
		return result{state: dbstate.OpSucceeded, sess: s.observe(ctx, w.sessionID())}
	}
	// The agent's MCP credential is published first, outside any transaction. Issue is idempotent,
	// so a retried create sends the same reference.
	req := w.createRequest()
	creds, err := s.Credentials.SessionCredentials(ctx, w.ID, s.Config.MCPOrigin())
	if err != nil {
		return result{err: err}
	}
	req.Credentials = creds
	ref, err := s.gitReadForCreate(ctx, w)
	if err != nil {
		return result{err: err}
	}
	if ref != nil {
		req.Credentials = append(req.Credentials, ref)
	}
	ref, err = s.gitPushForCreate(ctx, w)
	if err != nil {
		return result{err: err}
	}
	if ref != nil {
		req.Credentials = append(req.Credentials, ref)
	}
	sess, err := s.Kagent.CreateSession(ctx, req)
	if err != nil {
		return result{err: err}
	}
	if sess.GetId() == "" {
		return result{err: &unconfirmed{"kagent returned no session id"}}
	}
	return result{state: dbstate.OpSucceeded, sess: sess, bind: sess.GetId()}
}

// activity is the newest sign of use: the workspace's own and its deliveries'.
func (s *Service) activity(ctx context.Context, w wsRow) (time.Time, error) {
	at, err := dbq.New(s.Pool).GetWorkspaceActivity(ctx, w.ID)
	if err != nil {
		return time.Time{}, err
	}
	if s.Deliveries != nil {
		d, err := s.Deliveries.LastActivity(ctx, w.ID)
		if err != nil {
			return time.Time{}, err
		}
		if d.After(at) {
			at = d
		}
	}
	return at, nil
}

func (s *Service) turnOpen(ctx context.Context, w wsRow) (bool, error) {
	if s.Deliveries == nil {
		return false, nil
	}
	return s.Deliveries.HasOpen(ctx, w.ID)
}

func (s *Service) doSuspend(ctx context.Context, op dbq.WorkspaceOp, w wsRow) result {
	if w.KagentSessionID == nil {
		return result{state: dbstate.OpRejected, reason: "the workspace has no runtime session yet"}
	}
	if op.QuietSince != nil {
		at, err := s.activity(ctx, w)
		if err != nil {
			return result{err: err}
		}
		if at.After(*op.QuietSince) {
			return result{state: dbstate.OpCancelled, reason: "the workspace was used since the idle check"}
		}
	}
	if open, err := s.turnOpen(ctx, w); err != nil {
		return result{err: err}
	} else if open {
		return result{state: dbstate.OpRejected, reason: "a turn is open"}
	}
	idle := op.QuietSince != nil // queued by the idle sweep, not asked for
	sess, err := s.Kagent.GetSession(ctx, w.sessionID())
	if err != nil {
		if idle && notFoundErr(err) {
			// Mark the workspace so the sweep does not queue the same doomed suspend every pass.
			return result{state: dbstate.OpRejected, reason: "kagent no longer has the session", idle: true}
		}
		return result{err: err}
	}
	busy := sess.GetOperation() == kagentapi.RuntimeOperation_RUNTIME_OPERATION_SUSPEND ||
		sess.GetOperation() == kagentapi.RuntimeOperation_RUNTIME_OPERATION_RESUME
	switch {
	case sess.GetState() == kagentapi.RuntimeState_RUNTIME_STATE_SUSPENDED:
		return result{state: dbstate.OpSucceeded, sess: sess, idle: true}
	case sess.GetState() == kagentapi.RuntimeState_RUNTIME_STATE_READY && !busy:
		sess, err = s.Kagent.SuspendSession(ctx, w.sessionID())
		if err != nil {
			return result{err: err}
		}
		if sess.GetState() == kagentapi.RuntimeState_RUNTIME_STATE_SUSPENDED ||
			sess.GetOperation() == kagentapi.RuntimeOperation_RUNTIME_OPERATION_SUSPEND {
			return result{state: dbstate.OpSucceeded, sess: sess, idle: true}
		}
		return result{err: &unconfirmed{"kagent did not confirm the suspend"}}
	case idle && (busy || sess.GetState() == kagentapi.RuntimeState_RUNTIME_STATE_CREATING):
		return result{err: &unconfirmed{"the workspace is changing state"}} // look again shortly
	}
	// Failed, deleting or otherwise unsuitable: an idle suspend marks the workspace, so the sweep
	// leaves it alone until it is used again.
	return result{state: dbstate.OpRejected, reason: "the workspace is not in a state that can be suspended", sess: sess, idle: idle}
}

func (s *Service) doResume(ctx context.Context, w wsRow) result {
	if w.KagentSessionID == nil {
		return result{state: dbstate.OpRejected, reason: "the workspace has no runtime session yet"}
	}
	sess, err := s.Kagent.GetSession(ctx, w.sessionID())
	if err != nil {
		return result{err: err}
	}
	switch sess.GetState() {
	case kagentapi.RuntimeState_RUNTIME_STATE_READY:
		return result{state: dbstate.OpSucceeded, sess: sess, touch: true, unchanged: true}
	case kagentapi.RuntimeState_RUNTIME_STATE_SUSPENDED:
		sess, err = s.Kagent.ResumeSession(ctx, w.sessionID())
		if err != nil {
			return result{err: err}
		}
		if sess.GetState() == kagentapi.RuntimeState_RUNTIME_STATE_READY ||
			sess.GetOperation() == kagentapi.RuntimeOperation_RUNTIME_OPERATION_RESUME {
			return result{state: dbstate.OpSucceeded, sess: sess, touch: true}
		}
		return result{err: &unconfirmed{"kagent did not confirm the resume"}}
	case kagentapi.RuntimeState_RUNTIME_STATE_CREATING:
		return result{err: &unconfirmed{"the workspace is still starting"}}
	}
	return result{state: dbstate.OpRejected, reason: "the workspace is not in a state that can be resumed", sess: sess}
}

func (s *Service) doDelete(ctx context.Context, w wsRow) result {
	if open, err := s.turnOpen(ctx, w); err != nil {
		return result{err: err}
	} else if open {
		return result{state: dbstate.OpRejected, reason: "a turn is open"}
	}
	q := dbq.New(s.Pool)
	sid := w.sessionID()
	if sid == "" {
		attempted, err := q.WorkspaceCreateAttempted(ctx, w.ID)
		if err != nil {
			return result{err: err}
		}
		if !attempted {
			return result{state: dbstate.OpSucceeded, purge: true}
		}
		// A create may have reached kagent without the reply being recorded. The idempotent
		// create returns that Session, so the delete removes it rather than leaking it.
		// The replay carries the credential reference the original create did (kagent refuses a
		// replay that differs), whether or not the credential is still live; it makes none.
		req := w.createRequest()
		b, err := q.GetAgentBinding(ctx, w.ID)
		if err != nil {
			return result{err: &transient{err}}
		}
		req.Credentials = s.Credentials.ReplayCredentials(w.ID, s.Config.MCPOrigin(), b.Role, b.MCPGrantKind)
		ref, err := gitReadForReplay(w)
		if err != nil {
			return result{err: &transient{err}}
		}
		if ref != nil {
			req.Credentials = append(req.Credentials, ref)
		}
		ref, err = frozenReference(w.GitPushCredential)
		if err != nil {
			return result{err: &transient{err}}
		}
		if ref != nil {
			req.Credentials = append(req.Credentials, ref)
		}
		sess, err := s.Kagent.CreateSession(ctx, req)
		if replayFoundNoSession(err) {
			// No Session holds the request id, or the orphan sweep reclaims it if a kagent-side
			// change made this replay refuse a request an earlier attempt had accepted.
			return result{state: dbstate.OpSucceeded, purge: true}
		}
		if err != nil {
			return result{err: &transient{err}}
		}
		sid = sess.GetId()
		if sid == "" {
			return result{err: &unconfirmed{"kagent returned no session id"}}
		}
		if n, err := q.BindWorkspaceSession(ctx, dbq.BindWorkspaceSessionParams{SessionID: w.ID, KagentSessionID: &sid}); err != nil || n == 0 {
			return result{err: &unconfirmed{"could not record the runtime session before deleting it"}}
		}
	}
	sess, err := s.Kagent.DeleteSession(ctx, sid)
	switch {
	case notFoundErr(err):
	case err != nil:
		return result{err: err}
	case sess.GetId() != sid || sess.GetState() != kagentapi.RuntimeState_RUNTIME_STATE_DELETED:
		return result{err: &unconfirmed{"kagent did not confirm the delete"}}
	}
	return result{state: dbstate.OpSucceeded, purge: true}
}

// observe reads a Session for a lifecycle view: nil when it cannot be read, which shows as unknown.
func (s *Service) observe(ctx context.Context, id string) *kagentapi.Session {
	sess, err := s.Kagent.GetSession(ctx, id)
	if err != nil {
		if !notFoundErr(err) {
			s.Log.DebugContext(ctx, "kagent session unreadable", "session", id, "error", err)
		}
		return nil
	}
	return sess
}
