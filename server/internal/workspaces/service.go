package workspaces

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/sync/errgroup"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/gitref"
)

// InvalidError is a request that cannot be a workspace; the API answers 422 with the message.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

// ErrProjectNotFound means project_id names no project of the owner.
var ErrProjectNotFound = errors.New("project not found")

const defaultAgentKind = "claude"

// listConcurrency bounds the kagent reads behind one list request.
const listConcurrency = 8

func (s *Service) lookup(ctx context.Context, id string) (wsRow, error) {
	row, err := dbq.New(s.Pool).GetWorkspaceRow(ctx, dbq.GetWorkspaceRowParams{ID: id, UserID: s.OwnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return wsRow{}, ErrNotFound
	}
	if err != nil {
		return wsRow{}, err
	}
	return wsRow(dbq.GetWorkspaceForOpRow(row)), nil
}

// lifecycle is the workspace as last observed from kagent. A Session kagent cannot be asked about
// shows as unknown; it is never an error.
func (s *Service) lifecycle(ctx context.Context, w wsRow) api.WorkspaceLifecycle {
	var create *dbq.WorkspaceOp
	var sess = (*sessionView)(nil)
	if w.KagentSessionID == nil {
		if op, err := dbq.New(s.Pool).GetLatestCreateOp(ctx, w.ID); err == nil {
			create = &op
		}
	} else {
		sess = &sessionView{s.observe(ctx, w.sessionID())}
	}
	var delivery time.Time
	if s.Deliveries != nil {
		delivery, _ = s.Deliveries.LastActivity(ctx, w.ID)
	}
	if sess == nil {
		return project(w, nil, create, delivery)
	}
	return project(w, sess.s, create, delivery)
}

// Get returns one workspace.
func (s *Service) Get(ctx context.Context, id string) (api.WorkspaceLifecycle, error) {
	w, err := s.lookup(ctx, id)
	if err != nil {
		return api.WorkspaceLifecycle{}, err
	}
	return s.lifecycle(ctx, w), nil
}

// List returns the owner's workspaces, newest first.
func (s *Service) List(ctx context.Context) ([]api.WorkspaceLifecycle, error) {
	q := dbq.New(s.Pool)
	rows, err := q.ListWorkspaceRows(ctx, s.OwnerID)
	if err != nil {
		return nil, err
	}
	creates, err := q.ListLatestCreateOps(ctx, s.OwnerID)
	if err != nil {
		return nil, err
	}
	latest := map[string]dbq.WorkspaceOp{}
	for _, op := range creates {
		latest[op.WorkspaceID] = op
	}
	out := make([]api.WorkspaceLifecycle, len(rows))
	var g errgroup.Group
	g.SetLimit(listConcurrency)
	for i, r := range rows {
		g.Go(func() error {
			w := wsRow(dbq.GetWorkspaceForOpRow(r))
			var sess *sessionView
			var create *dbq.WorkspaceOp
			if w.KagentSessionID != nil {
				sess = &sessionView{s.observe(ctx, w.sessionID())}
			} else if op, ok := latest[w.ID]; ok {
				create = &op
			}
			var delivery time.Time
			if s.Deliveries != nil {
				delivery, _ = s.Deliveries.LastActivity(ctx, w.ID)
			}
			if sess == nil {
				out[i] = project(w, nil, create, delivery)
			} else {
				out[i] = project(w, sess.s, create, delivery)
			}
			return nil
		})
	}
	_ = g.Wait()
	return out, nil
}

// Ports lists the workspace's preview ports with their URLs.
func (s *Service) Ports(ctx context.Context, id string) ([]api.WorkspacePreviewPort, error) {
	w, err := s.lookup(ctx, id)
	if err != nil {
		return nil, err
	}
	return previewPorts(s.Config.Preview.BaseURL, w), nil
}

// enqueue queues an operation, or returns the open one of the same kind. Another kind already
// open on the workspace is ErrBusy; a delete supersedes the others.
func (s *Service) enqueue(ctx context.Context, workspaceID string, kind dbstate.WorkspaceOpKind, quietSince *time.Time) (dbq.WorkspaceOp, error) {
	var op dbq.WorkspaceOp
	var err error
	// Losing the insert race to another request aborts the transaction; the second pass then
	// finds that request's operation and joins it when it is of the same kind.
	for range 3 {
		err = pgxTx(ctx, s, func(q *dbq.Queries) error {
			var err error
			op, err = s.insertOp(ctx, q, workspaceID, kind, quietSince)
			return err
		})
		if !errors.Is(err, errRaced) {
			break
		}
	}
	if errors.Is(err, errRaced) {
		err = ErrBusy
	}
	return op, err
}

var errRaced = errors.New("workspaces: lost the operation insert race")

func (s *Service) insertOp(ctx context.Context, q *dbq.Queries, workspaceID string, kind dbstate.WorkspaceOpKind, quietSince *time.Time) (dbq.WorkspaceOp, error) {
	if kind == dbstate.OpDelete {
		if _, err := q.CancelOpenWorkspaceOps(ctx, workspaceID); err != nil {
			return dbq.WorkspaceOp{}, err
		}
		// Push authority ends with the durable delete intent, in the same transaction: a delayed or
		// failing native delete must not leave a window for a new upstream push.
		if err := revokePush(ctx, q, workspaceID); err != nil {
			return dbq.WorkspaceOp{}, err
		}
	}
	open, err := q.GetOpenWorkspaceOp(ctx, workspaceID)
	switch {
	case err == nil:
		if open.Kind == kind {
			return open, nil
		}
		return dbq.WorkspaceOp{}, ErrBusy
	case !errors.Is(err, pgx.ErrNoRows):
		return dbq.WorkspaceOp{}, err
	}
	id := uuid.NewString()
	op, err := q.InsertWorkspaceOp(ctx, dbq.InsertWorkspaceOpParams{
		ID: id, WorkspaceID: workspaceID, Kind: kind, IdempotencyKey: string(kind) + ":" + id,
		QuietSince: quietSince,
	})
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.ConstraintName == "workspace_ops_open" {
		return dbq.WorkspaceOp{}, errRaced
	}
	if errors.As(err, &pg) && pg.Code == "23503" && pg.ConstraintName == "workspace_ops_workspace_id_fkey" {
		return dbq.WorkspaceOp{}, ErrNotFound // the workspace was deleted after it was looked up
	}
	if err != nil {
		return dbq.WorkspaceOp{}, err
	}
	if err := q.NotifyEvent(ctx, dbq.NotifyEventParams{Channel: opChannel, Payload: op.ID}); err != nil {
		return dbq.WorkspaceOp{}, err
	}
	return op, nil
}

func isOpen(state dbstate.WorkspaceOpState) bool {
	return state == dbstate.OpPending || state == dbstate.OpRetrying
}

// await waits for the operation to leave the open states, up to the configured wait. It returns
// the last row it read; an operation whose row vanished (the workspace was deleted) succeeded.
func (s *Service) await(ctx context.Context, op dbq.WorkspaceOp) (dbq.WorkspaceOp, error) {
	q := dbq.New(s.Pool)
	timer := time.NewTimer(s.Config.Workspaces.OperationWait.Duration())
	defer timer.Stop()
	for {
		cur, err := q.GetWorkspaceOp(ctx, op.ID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			op.State = dbstate.OpSucceeded
			return op, nil
		case err != nil:
			return op, err
		}
		op = cur
		if !isOpen(op.State) {
			return op, nil
		}
		select {
		case <-ctx.Done():
			return op, ctx.Err()
		case <-timer.C:
			return op, nil
		case <-time.After(s.poll()):
		}
	}
}

// outcome turns a finished (or still open) operation into the error the caller reports.
func outcome(op dbq.WorkspaceOp) error {
	detail := ""
	if op.LastError != nil {
		detail = *op.LastError
	}
	switch dbstate.WorkspaceOpState(op.State) {
	case dbstate.OpSucceeded:
		return nil
	case dbstate.OpRejected:
		if detail == "" {
			detail = "the operation was refused"
		}
		return &RejectedError{Detail: capitalize(detail)}
	case dbstate.OpCancelled:
		if detail == "" {
			detail = "the operation was superseded by another change"
		}
		return &RejectedError{Detail: capitalize(detail)}
	case dbstate.OpGaveUp:
		return &UnconfirmedError{Detail: "kagent did not confirm the change before the retry window ended"}
	}
	return &UnconfirmedError{Detail: "kagent has not confirmed the change yet; Mainloop keeps retrying"}
}

func capitalize(s string) string {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return s
	}
	return string(s[0]-'a'+'A') + s[1:]
}

// change queues an operation on the workspace and waits for its outcome.
func (s *Service) change(ctx context.Context, id string, kind dbstate.WorkspaceOpKind) (wsRow, error) {
	w, err := s.lookup(ctx, id)
	if err != nil {
		return wsRow{}, err
	}
	if kind == dbstate.OpDelete {
		if open, err := s.turnOpen(ctx, w); err != nil {
			return wsRow{}, err
		} else if open {
			return wsRow{}, &RejectedError{Detail: "A turn is open; stop it before deleting the workspace"}
		}
	}
	op, err := s.enqueue(ctx, w.ID, kind, nil)
	if err != nil {
		return wsRow{}, err
	}
	final, err := s.await(ctx, op)
	if err != nil {
		return wsRow{}, err
	}
	if err := outcome(final); err != nil {
		return wsRow{}, err
	}
	if kind == dbstate.OpDelete {
		return w, nil
	}
	return s.lookup(ctx, id)
}

// Suspend parks the workspace and returns it once kagent confirms.
func (s *Service) Suspend(ctx context.Context, id string) (api.WorkspaceLifecycle, error) {
	w, err := s.change(ctx, id, dbstate.OpSuspend)
	if err != nil {
		return api.WorkspaceLifecycle{}, err
	}
	return s.lifecycle(ctx, w), nil
}

// Resume wakes the workspace and returns it once kagent confirms. Resuming counts as activity.
func (s *Service) Resume(ctx context.Context, id string) (api.WorkspaceLifecycle, error) {
	w, err := s.change(ctx, id, dbstate.OpResume)
	if err != nil {
		return api.WorkspaceLifecycle{}, err
	}
	return s.lifecycle(ctx, w), nil
}

// Delete removes the workspace's kagent Session and then its rows. The rows stay until kagent
// confirms the Session is gone.
func (s *Service) Delete(ctx context.Context, id string) error {
	_, err := s.change(ctx, id, dbstate.OpDelete)
	return err
}

// Refresh re-establishes a workspace that has no Session yet: it queues a fresh create with a new
// retry window. A workspace that has its Session is just read again.
func (s *Service) Refresh(ctx context.Context, id string) (api.WorkspaceLifecycle, error) {
	w, err := s.lookup(ctx, id)
	if err != nil {
		return api.WorkspaceLifecycle{}, err
	}
	if w.KagentSessionID == nil {
		op, err := s.enqueue(ctx, w.ID, dbstate.OpCreate, nil)
		if err != nil {
			return api.WorkspaceLifecycle{}, err
		}
		final, err := s.await(ctx, op)
		if err != nil {
			return api.WorkspaceLifecycle{}, err
		}
		if dbstate.WorkspaceOpState(final.State) == dbstate.OpRejected {
			return api.WorkspaceLifecycle{}, outcome(final)
		}
		if w, err = s.lookup(ctx, id); err != nil {
			return api.WorkspaceLifecycle{}, err
		}
	}
	return s.lifecycle(ctx, w), nil
}

// wakeRounds bounds how often Wake starts over because another operation took the workspace
// between its checks.
const wakeRounds = 4

// Wake makes sure the workspace is running before something talks to it (preview traffic, a
// message for its session). A running workspace is only touched. One that is being suspended is
// waited for, and then resumed; a suspended one is resumed through kagent and the call returns
// once kagent confirms. Concurrent calls share one resume operation. A caller whose context ends
// does not cancel the resume; the lifecycle loop finishes it. It returns a *RejectedError for a
// workspace that cannot be resumed and an *UnconfirmedError when kagent has not confirmed in time,
// so the caller can retry.
func (s *Service) Wake(ctx context.Context, workspaceID string) error {
	for range wakeRounds {
		w, err := s.lookup(ctx, workspaceID)
		if err != nil {
			return err
		}
		open, err := dbq.New(s.Pool).GetOpenWorkspaceOp(ctx, workspaceID)
		switch {
		case err == nil && open.Kind == dbstate.OpDelete:
			return &RejectedError{Detail: "The workspace is being deleted"}
		case err == nil && open.Kind == dbstate.OpSuspend:
			// A suspend that started before the message is let finish; then the workspace is resumed.
			// One that starts after the message was recorded finds the open turn and stands down.
			if _, err := s.await(ctx, open); err != nil {
				return err
			}
			continue
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		if w.KagentSessionID != nil {
			if sess := s.observe(ctx, w.sessionID()); sess != nil {
				if state, _ := stateOf(sess); state == api.WorkspaceObservedStateRunning {
					return s.Touch(ctx, workspaceID)
				}
			}
		}
		if _, err := s.change(ctx, workspaceID, dbstate.OpResume); !errors.Is(err, ErrBusy) {
			return err
		}
	}
	return ErrBusy
}

// WakeForPreview makes sure the workspace is running before preview traffic reaches it, without
// waiting out other work. A running workspace is only touched. If a suspend (or a create) is open
// it answers ErrBusy at once, and the proxy shows its waking page with Retry-After instead of
// holding the request; a delete is a *RejectedError. Otherwise a suspended workspace has its resume
// queued once (concurrent calls share the open one) and the call returns once kagent confirms, for
// a bounded wait. A caller whose context ends does not cancel the resume; the lifecycle loop
// finishes it. It returns an *UnconfirmedError when kagent has not confirmed in time, so the caller
// can retry. [Service.Wake] is the variant that waits out an open suspend, for the delivery path.
func (s *Service) WakeForPreview(ctx context.Context, workspaceID string) error {
	w, err := s.lookup(ctx, workspaceID)
	if err != nil {
		return err
	}
	open, err := dbq.New(s.Pool).GetOpenWorkspaceOp(ctx, workspaceID)
	switch {
	case err == nil && open.Kind == dbstate.OpDelete:
		return &RejectedError{Detail: "The workspace is being deleted"}
	case err == nil && open.Kind != dbstate.OpResume:
		return ErrBusy
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	if w.KagentSessionID != nil {
		if sess := s.observe(ctx, w.sessionID()); sess != nil {
			if state, _ := stateOf(sess); state == api.WorkspaceObservedStateRunning {
				return s.Touch(ctx, workspaceID)
			}
		}
	}
	_, err = s.change(ctx, workspaceID, dbstate.OpResume)
	return err
}

// Touch records owner activity (preview traffic), which defers the idle suspend.
func (s *Service) Touch(ctx context.Context, workspaceID string) error {
	_, err := dbq.New(s.Pool).TouchWorkspace(ctx, workspaceID)
	return err
}

// Create validates the request, records the workspace with a pending create operation and waits
// a bounded time for kagent. A create kagent rejects outright is removed and reported as a
// *RejectedError; one that is still in flight or failing transiently returns the workspace, and the
// operation keeps retrying.
func (s *Service) Create(ctx context.Context, body api.CreateWorkspaceRequest) (api.WorkspaceLifecycle, error) {
	return s.create(ctx, body, uuid.NewString())
}

// CreateWithRequestID is [Service.Create] made idempotent on the caller's request id, a UUID. A
// repeat call, concurrent or later, returns the workspace the first call made (waiting for its
// create as the first call did) instead of making another: the id is also the request id kagent
// sees, so no second Session can exist either. The body of a repeat is not compared. A workspace
// that has since been deleted is gone; the same id then makes a new one, which kagent refuses as it
// does any replay of a deleted Session.
func (s *Service) CreateWithRequestID(ctx context.Context, requestID string, body api.CreateWorkspaceRequest) (api.WorkspaceLifecycle, error) {
	id, err := uuid.Parse(requestID)
	if err != nil {
		return api.WorkspaceLifecycle{}, &InvalidError{"request id must be a UUID"}
	}
	requestID = id.String()
	if w, err := s.repeat(ctx, requestID); !errors.Is(err, ErrNotFound) {
		return w, err
	}
	w, err := s.create(ctx, body, requestID)
	if isRequestIDTaken(err) {
		return s.repeat(ctx, requestID)
	}
	return w, err
}

// LookupByRequestID returns the workspace made with a caller request id, or ErrNotFound.
func (s *Service) LookupByRequestID(ctx context.Context, requestID string) (api.WorkspaceLifecycle, error) {
	w, err := s.byRequestID(ctx, requestID)
	if err != nil {
		return api.WorkspaceLifecycle{}, err
	}
	return s.lifecycle(ctx, w), nil
}

func (s *Service) byRequestID(ctx context.Context, requestID string) (wsRow, error) {
	id, err := uuid.Parse(requestID)
	if err != nil {
		return wsRow{}, ErrNotFound
	}
	row, err := dbq.New(s.Pool).GetWorkspaceByRequestID(ctx, dbq.GetWorkspaceByRequestIDParams{RequestID: id.String(), UserID: s.OwnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return wsRow{}, ErrNotFound
	}
	if err != nil {
		return wsRow{}, err
	}
	return wsRow(dbq.GetWorkspaceForOpRow(row)), nil
}

// repeat answers a repeated create with the workspace the first one made, waiting for that
// create the way the first call did.
func (s *Service) repeat(ctx context.Context, requestID string) (api.WorkspaceLifecycle, error) {
	w, err := s.byRequestID(ctx, requestID)
	if err != nil {
		return api.WorkspaceLifecycle{}, err
	}
	if op, err := dbq.New(s.Pool).GetLatestCreateOp(ctx, w.ID); err == nil && isOpen(op.State) {
		if _, err := s.await(ctx, op); err != nil {
			return api.WorkspaceLifecycle{}, err
		}
		if w, err = s.lookup(ctx, w.ID); err != nil {
			return api.WorkspaceLifecycle{}, err
		}
	}
	return s.lifecycle(ctx, w), nil
}

func isRequestIDTaken(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "workspaces_request_id_key"
}

func (s *Service) create(ctx context.Context, body api.CreateWorkspaceRequest, requestID string) (api.WorkspaceLifecycle, error) {
	p, err := s.plan(ctx, body)
	if err != nil {
		return api.WorkspaceLifecycle{}, err
	}
	p.insert.RequestID = requestID
	var op dbq.WorkspaceOp
	err = events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		q := dbq.New(tx)
		if _, err := q.CreateSession(ctx, dbq.CreateSessionParams{
			ID: p.id, UserID: s.OwnerID, ProjectID: &p.project.ID, Title: p.project.FullName,
			RepoURL: &p.repoURL, BranchName: &p.branch, BaseBranch: baseBranch(p.ref),
		}); err != nil {
			return nil, err
		}
		if _, err := q.CreateAgentBinding(ctx, dbq.CreateAgentBindingParams{
			SessionID: p.id, Kind: p.kind, Role: dbstate.RoleAgent, MCPGrantKind: dbstate.GrantWorkspace,
		}); err != nil {
			return nil, err
		}
		w, err := q.InsertWorkspace(ctx, p.insert)
		if err != nil {
			return nil, err
		}
		if op, err = s.insertOp(ctx, q, p.id, dbstate.OpCreate, nil); err != nil {
			return nil, err
		}
		row := wsRow{ID: w.ID, UserID: w.UserID, ProjectID: w.ProjectID, RepoURL: w.RepoURL, Ref: w.Ref,
			Branch: w.Branch, Depth: w.Depth, AgentKind: w.AgentKind, Ports: w.Ports,
			IdleTimeoutMinutes: w.IdleTimeoutMinutes, Environment: w.Environment, RequestID: w.RequestID,
			CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt}
		lc := project(row, nil, &op, time.Time{})
		return []events.Draft{{UserID: s.OwnerID, Type: events.TypeWorkspaceUpdated, Payload: api.EventWorkspaceUpdated{Workspace: lc}}}, nil
	})
	if err != nil {
		return api.WorkspaceLifecycle{}, fmt.Errorf("create workspace: %w", err)
	}
	final, err := s.await(ctx, op)
	if err != nil {
		return api.WorkspaceLifecycle{}, err
	}
	if final.State == dbstate.OpRejected && final.Attempts == 1 {
		// kagent refused the very first request: nothing was made, so nothing is kept. A refusal
		// after earlier attempts of unknown outcome keeps the rows (the workspace shows as
		// failed) so a Session an earlier attempt made can still be found and deleted.
		if err := s.dropRejected(context.WithoutCancel(ctx), p.id, final); err != nil {
			s.Log.WarnContext(ctx, "could not remove a rejected workspace", "workspace", p.id, "error", err)
		}
		return api.WorkspaceLifecycle{}, outcome(final)
	}
	w, err := s.lookup(ctx, p.id)
	if err != nil {
		return api.WorkspaceLifecycle{}, err
	}
	return s.lifecycle(ctx, w), nil
}

// dropRejected removes a workspace whose create kagent refused and publishes its failed state.
// The event contract has no removal event; clients drop the workspace when they next list. It
// removes nothing once a newer operation exists (a Refresh landed after the refusal was recorded,
// and its create may already have been sent): the workspace row is locked so none can be added
// between the check and the delete.
func (s *Service) dropRejected(ctx context.Context, id string, create dbq.WorkspaceOp) error {
	return events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		q := dbq.New(tx)
		// An operation insert needs a share lock on this row for its foreign key, so it waits here.
		// Lock first, then read, so what is read is what is decided on.
		if _, err := q.LockWorkspace(ctx, id); errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		} else if err != nil {
			return nil, err
		}
		row, err := q.GetWorkspaceForOp(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		latest, err := q.GetLatestCreateOp(ctx, id)
		if err != nil {
			return nil, err
		}
		if latest.ID != create.ID || latest.State == dbstate.OpPending || latest.State == dbstate.OpRetrying {
			return nil, nil
		}
		if _, err := q.GetOpenWorkspaceOp(ctx, id); !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		// The session row first, so an Issue holding it FOR SHARE finishes before the revoke runs.
		if err := revokePush(ctx, q, id); err != nil {
			return nil, err
		}
		if _, err := q.DeleteSession(ctx, id); err != nil {
			return nil, err
		}
		if _, err := agentauth.RevokeInTx(ctx, q, id); err != nil {
			return nil, err
		}
		lc := project(wsRow(row), nil, &create, time.Time{})
		return []events.Draft{{UserID: row.UserID, Type: events.TypeWorkspaceUpdated, Payload: api.EventWorkspaceUpdated{Workspace: lc}}}, nil
	})
}

func baseBranch(ref string) string {
	if ref == "" {
		return "main"
	}
	return ref
}

type plan struct {
	id      string
	kind    string
	project dbq.Project
	repoURL string
	ref     string
	branch  string
	insert  dbq.InsertWorkspaceParams
}

// plan validates a create request and resolves everything it needs before any row is written.
func (s *Service) plan(ctx context.Context, body api.CreateWorkspaceRequest) (plan, error) {
	hasProject, hasRepo := body.ProjectId != nil && *body.ProjectId != "", body.Repo != nil && *body.Repo != ""
	if hasProject == hasRepo {
		return plan{}, &InvalidError{"Provide exactly one of project_id or repo"}
	}
	kind := defaultAgentKind
	if body.AgentKind != nil && *body.AgentKind != "" {
		kind = *body.AgentKind
	}
	profile, err := s.Providers.Resolve(kind, "agent", true)
	if err != nil {
		return plan{}, &InvalidError{err.Error()}
	}
	agent := profile.Agents["agent"]
	depth := 0
	if body.Depth != nil {
		depth = *body.Depth
	}
	if depth < 0 || depth > maxDepth {
		return plan{}, &InvalidError{fmt.Sprintf("depth must be between 0 and %d", maxDepth)}
	}
	ports, idle := []api.WorkspacePort{}, defaultIdleMin
	if body.Dev != nil {
		if body.Dev.Ports != nil {
			ports = *body.Dev.Ports
		}
		if body.Dev.IdleTimeoutMinutes != nil {
			idle = *body.Dev.IdleTimeoutMinutes
		}
	}
	if idle < 1 || idle > 1440 {
		return plan{}, &InvalidError{"idle_timeout_minutes must be between 1 and 1440"}
	}
	if err := validatePorts(ports); err != nil {
		return plan{}, &InvalidError{err.Error()}
	}
	for field, v := range map[string]*string{"ref": body.Ref, "branch": body.Branch} {
		if v != nil && *v != "" && !gitref.Valid(*v) {
			return plan{}, &InvalidError{fmt.Sprintf("%s is not a valid git name", field)}
		}
	}

	q := dbq.New(s.Pool)
	var project dbq.Project
	if hasProject {
		project, err = q.GetOwnedProject(ctx, dbq.GetOwnedProjectParams{ID: *body.ProjectId, UserID: s.OwnerID})
		if errors.Is(err, pgx.ErrNoRows) {
			return plan{}, ErrProjectNotFound
		}
	} else {
		repo, perr := github.ParseRepo(*body.Repo)
		if perr != nil {
			return plan{}, &InvalidError{"repo must be a GitHub repository (owner/name or its https URL)"}
		}
		project, err = q.ImportProject(ctx, dbq.ImportProjectParams{
			ID: uuid.NewString(), UserID: s.OwnerID, Owner: repo.Owner, Name: repo.Name,
			FullName: repo.FullName(), HTMLURL: repo.HTMLURL(),
		})
	}
	if err != nil {
		return plan{}, err
	}
	ref := project.DefaultBranch
	if body.Ref != nil && *body.Ref != "" {
		ref = *body.Ref
	}
	branch := ""
	if body.Branch != nil {
		branch = *body.Branch
	}
	if branch == "" {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			return plan{}, err
		}
		branch = "mainloop/" + hex.EncodeToString(b[:])
	}
	env, err := s.environment(ctx, project.ID)
	if err != nil {
		return plan{}, err
	}
	portJSON, err := json.Marshal(ports)
	if err != nil {
		return plan{}, err
	}
	id := uuid.NewString()
	return plan{
		id: id, kind: kind, project: project, repoURL: project.HTMLURL, ref: ref, branch: branch,
		insert: dbq.InsertWorkspaceParams{
			ID: id, UserID: s.OwnerID, ProjectID: project.ID, RepoURL: project.HTMLURL, Ref: ref,
			Branch: branch, Depth: int32(depth), AgentKind: kind, AgentNamespace: agent.Namespace, AgentName: agent.Name,
			Ports:              portJSON,
			IdleTimeoutMinutes: int32(idle), Environment: env, RequestID: uuid.NewString(),
		},
	}, nil
}

// environment resolves the project's selected development environment to the exact image the
// workspace will run, or nil when the project has none validated.
func (s *Service) environment(ctx context.Context, projectID string) (json.RawMessage, error) {
	v, err := dbq.New(s.Pool).GetWorkspaceEnvironmentVersion(ctx, projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if v.Registry == nil || v.Repository == nil || v.PlatformManifestDigest == nil {
		return nil, nil
	}
	platform := s.Config.WorkspaceDevelopmentPlatform
	if v.Architecture != nil {
		platform = "linux/" + *v.Architecture
	}
	return json.Marshal(api.WorkspaceEnvironment{
		EnvironmentId: v.EnvironmentID, VersionId: v.VersionID, Platform: platform,
		Image:          *v.Registry + "/" + *v.Repository + "@" + *v.PlatformManifestDigest,
		PolicyIdentity: v.VersionID + ":" + v.ValidatorVersion,
	})
}
