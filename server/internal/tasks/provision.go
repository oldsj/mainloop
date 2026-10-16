package tasks

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/sessions"
	"github.com/oldsj/mainloop/server/internal/workspaces"
)

// OpChannel wakes the operation loop; the service notifies it when it records an operation.
const OpChannel = "mainloop_task_ops"

// WorkspaceRuntime is the part of [workspaces.Service] provisioning uses.
type WorkspaceRuntime interface {
	CreateWithRequestID(ctx context.Context, requestID string, body api.CreateWorkspaceRequest) (api.WorkspaceLifecycle, error)
	Delete(ctx context.Context, id string) error
}

// SessionRuntime is the part of [sessions.Service] delivery and settlement use.
type SessionRuntime interface {
	SendToAs(ctx context.Context, source sessions.Source, sessionID, messageID, text string) (sessions.Sent, error)
	SendAs(ctx context.Context, source sessions.Source, messageID, conversationID, text string) (sessions.Sent, error)
	EnsureMain(ctx context.Context) (sessions.Main, error)
	Cancel(ctx context.Context, sessionID string) (sessions.CancelResult, error)
}

// Register adds the task loops and sweeps to the runner. Without Workspaces and Sessions the rows
// recorded by the service wait, so nothing is added; the projection sweep needs an Observer and
// does nothing without one.
func (s *Service) Register(r *reconcile.Runner) {
	if s.Workspaces == nil || s.Sessions == nil {
		return
	}
	r.Add(s.OpLoop())
	r.Add(s.DeliveryLoop())
	r.AddSweep(s.SettleSweep())
	r.AddSweep(s.CompleteSweep())
	r.AddSweep(s.BriefSweep())
	r.AddSweep(s.ProjectionSweep())
	r.AddSweep(s.LivenessSweep())
}

// OpTable is the table of operations the loop drives. Retry and reassign operations are recorded
// blocked and never reach it.
func OpTable() reconcile.Table {
	return reconcile.NewTable("task_operations",
		string(dbstate.TaskOpRequested), string(dbstate.TaskOpDraining), string(dbstate.TaskOpSourceFencing),
		string(dbstate.TaskOpSourceFenced), string(dbstate.TaskOpTargetCreating), string(dbstate.TaskOpTargetReady))
}

// OpLoop drives create and cancel operations: provisioning of a new attempt, and the drain, fence
// and final steps of a cancel. It needs Workspaces and Sessions.
func (s *Service) OpLoop() reconcile.Loop {
	return reconcile.Loop{
		Name: "task-operations", Table: OpTable(), Handle: s.handleOp,
		Interval: s.Limits.Interval, Wake: []string{OpChannel},
	}
}

// tx runs fn in a transaction and publishes the events it returns with the commit.
func (s *Service) tx(ctx context.Context, fn func(q *dbq.Queries) ([]events.Draft, error)) error {
	return events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) { return fn(dbq.New(tx)) })
}

func (s *Service) notify(ctx context.Context, q *dbq.Queries, channel, payload string) error {
	return q.NotifyEvent(ctx, dbq.NotifyEventParams{Channel: channel, Payload: payload})
}

// advance moves a claimed operation to a new state in the caller's transaction, fenced on the
// claim: a claim that lost its lease rolls the whole transaction back. The lease is released and
// the next step is due at once.
func advance(ctx context.Context, q *dbq.Queries, c reconcile.Claim, op dbq.TaskOperation, to dbstate.TaskOpState, reason *dbstate.TaskReason, attempt *string) error {
	if err := TransitionOperation(op.Kind, op.State, op.LastConfirmedStep, to); err != nil {
		return err
	}
	step := to
	if to == dbstate.TaskOpBlocked {
		step = op.LastConfirmedStep
	}
	_, err := q.AdvanceTaskOperation(ctx, dbq.AdvanceTaskOperationParams{
		State: to, Step: step, Reason: reason, AttemptID: attempt, TargetAttemptID: attempt,
		ID: c.ID, LeaseOwner: &c.Owner, Attempts: int32(c.Attempts), //nolint:gosec // a small counter
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return reconcile.ErrLeaseLost
	}
	return err
}

func (s *Service) handleOp(ctx context.Context, c reconcile.Claim) (reconcile.Outcome, error) {
	op, err := dbq.New(s.Pool).GetTaskOperationByID(ctx, c.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return reconcile.Recorded(), nil
	}
	if err != nil {
		return reconcile.Outcome{}, err
	}
	if op.TaskID == nil || op.State != dbstate.TaskOpState(c.State) {
		return reconcile.Retry(), nil
	}
	var out reconcile.Outcome
	switch op.Kind {
	case dbstate.TaskOpCreate:
		out, err = s.driveCreate(ctx, c, op)
	case dbstate.TaskOpCancel:
		out, err = s.driveCancel(ctx, c, op)
	default:
		return reconcile.Done(string(dbstate.TaskOpBlocked)), nil
	}
	if errors.Is(err, reconcile.ErrLeaseLost) {
		s.log().WarnContext(ctx, "task operation lease lost", "operation", c.ID, "error", err)
		return reconcile.Recorded(), nil
	}
	return out, err
}

// errWriterHeld means another attempt (or a stale one that was not fenced) holds the branch.
var errWriterHeld = errors.New("tasks: the branch has a writer")

func (s *Service) driveCreate(ctx context.Context, c reconcile.Claim, op dbq.TaskOperation) (reconcile.Outcome, error) {
	switch op.State {
	case dbstate.TaskOpRequested:
		return s.beginCreate(ctx, c, op)
	case dbstate.TaskOpTargetCreating:
		return s.createRuntime(ctx, c, op)
	case dbstate.TaskOpTargetReady:
		return s.sendBrief(ctx, c, op)
	}
	return reconcile.Done(string(dbstate.TaskOpBlocked)), nil
}

// attemptShape is what an attempt of a task is made of.
type attemptShape struct {
	role  dbstate.AttemptRole
	depth int32
}

func shapeOf(t dbq.Task) attemptShape {
	if t.ParentTaskID == nil {
		return attemptShape{role: dbstate.AttemptSupervisor, depth: 1}
	}
	return attemptShape{role: dbstate.AttemptChild, depth: 2}
}

// agentOf is the kagent Agent that runs an attempt: the workspace Agent for code, otherwise the
// profile's Agent for the attempt's role.
func agentOf(prof config.ProviderProfile, shape attemptShape, mode dbstate.TaskMode) (config.AgentRef, error) {
	key := agentKey(shape, mode)
	ref, ok := prof.Agents[key]
	if !ok {
		return ref, fmt.Errorf("provider %q has no %q Agent", prof.ID, key)
	}
	return ref, nil
}

// agentKey is the profile Agent role an attempt runs as.
func agentKey(shape attemptShape, mode dbstate.TaskMode) string {
	if mode == dbstate.TaskModeCode {
		return string(dbstate.RoleAgent)
	}
	return string(shape.role)
}

func bindingRole(shape attemptShape) dbstate.BindingRole {
	if shape.role == dbstate.AttemptSupervisor {
		return dbstate.RoleSupervisor
	}
	return dbstate.RoleChild
}

// beginCreate re-checks that the task is still open, then records the attempt, its writer claim
// (code) or its session and binding (coordination), and moves the task to its current attempt, all
// in one transaction. Nothing outside the database has been touched, so a crash leaves either
// nothing or all of it.
func (s *Service) beginCreate(ctx context.Context, c reconcile.Claim, op dbq.TaskOperation) (reconcile.Outcome, error) {
	var held bool
	err := s.tx(ctx, func(q *dbq.Queries) ([]events.Draft, error) {
		t, err := q.GetTaskByIDForUpdate(ctx, *op.TaskID)
		if err != nil {
			return nil, err
		}
		if TaskFinal(t.Status) || t.CurrentAttemptID != nil {
			return nil, advance(ctx, q, c, op, dbstate.TaskOpBlocked, nil, nil)
		}
		if t.ParentTaskID != nil {
			parent, err := q.GetTaskByID(ctx, *t.ParentTaskID)
			if err != nil {
				return nil, err
			}
			if TaskFinal(parent.Status) {
				// The parent ended before this child was provisioned: it never starts.
				after, err := q.UpdateTaskState(ctx, dbq.UpdateTaskStateParams{ID: t.ID, Version: t.Version, Status: dbstate.TaskCancelled})
				if err != nil {
					return nil, err
				}
				return []events.Draft{s.updated(after)}, advance(ctx, q, c, op, dbstate.TaskOpBlocked, nil, nil)
			}
		}
		shape := shapeOf(t)
		prof, err := s.Providers.Resolve(t.AssignedProfileID, agentKey(shape, t.Mode), false)
		if err != nil {
			return nil, err
		}
		agent, err := agentOf(prof, shape, t.Mode)
		if err != nil {
			return nil, err
		}
		number, err := q.NextAttemptNumber(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		a := dbq.InsertTaskAttemptParams{
			ID: uuid.NewString(), TaskID: t.ID, Number: number, ProfileID: prof.ID,
			NativeProvider: dbstate.NativeProvider(prof.NativeProvider), ConfigurationRevision: prof.ConfigurationRevision,
			AgentNamespace: agent.Namespace, AgentName: agent.Name, Role: shape.role, Depth: shape.depth,
		}
		var repo string
		if t.Mode == dbstate.TaskModeCode {
			if t.ProjectID == nil || t.CheckoutBranch == nil {
				return nil, errors.New("tasks: a code task has no project or branch")
			}
			full, err := q.GetProjectFullName(ctx, *t.ProjectID)
			if err != nil {
				return nil, err
			}
			repo = strings.ToLower(full)
			a.InitialRef = t.CheckoutRef
			a.WorkspaceRequestID = ptr(workspaceRequestID(a.ID))
		} else {
			id := uuid.NewString()
			a.SessionID, a.BindingID = &id, &id
		}
		att, err := q.InsertTaskAttempt(ctx, a)
		if err != nil {
			return nil, err
		}
		if t.Mode == dbstate.TaskModeCode {
			gen, err := q.ClaimTaskWriter(ctx, dbq.ClaimTaskWriterParams{
				OwnerID: t.OwnerID, Repository: repo, Branch: *t.CheckoutBranch, AttemptID: &att.ID,
			})
			if errors.Is(err, pgx.ErrNoRows) {
				held = true
				return nil, errWriterHeld
			}
			if err != nil {
				return nil, err
			}
			if err := q.SetAttemptWriterGeneration(ctx, dbq.SetAttemptWriterGenerationParams{ID: att.ID, WriterGeneration: &gen}); err != nil {
				return nil, err
			}
		} else if err := s.createCoordinationSession(ctx, q, t, att, prof); err != nil {
			return nil, err
		}
		reason := dbstate.ReasonProvisioningUnavailable
		after, err := q.SetTaskAttempt(ctx, dbq.SetTaskAttemptParams{
			ID: t.ID, CurrentAttemptID: &att.ID, Status: dbstate.TaskQueued, Reason: &reason,
		})
		if err != nil {
			return nil, err
		}
		return []events.Draft{s.updated(after)}, advance(ctx, q, c, op, dbstate.TaskOpTargetCreating, nil, &att.ID)
	})
	if errors.Is(err, errWriterHeld) && held {
		return s.waitForBranch(ctx, c, op)
	}
	if err != nil {
		return reconcile.Outcome{}, err
	}
	return reconcile.Recorded(), nil
}

// waitForBranch is a create whose branch has a writer already. It tries again until the
// provisioning window ends, then fails the task.
func (s *Service) waitForBranch(ctx context.Context, c reconcile.Claim, op dbq.TaskOperation) (reconcile.Outcome, error) {
	if s.now().Sub(op.CreatedAt) < s.Limits.ProvisionWindow {
		return reconcile.RetryAfter(10 * time.Second), nil
	}
	err := s.tx(ctx, func(q *dbq.Queries) ([]events.Draft, error) {
		t, err := q.GetTaskByIDForUpdate(ctx, *op.TaskID)
		if err != nil {
			return nil, err
		}
		var drafts []events.Draft
		if !TaskFinal(t.Status) {
			after, err := q.UpdateTaskState(ctx, dbq.UpdateTaskStateParams{ID: t.ID, Version: t.Version, Status: dbstate.TaskFailed})
			if err != nil {
				return nil, err
			}
			drafts = append(drafts, s.updated(after))
		}
		return drafts, advance(ctx, q, c, op, dbstate.TaskOpBlocked, nil, nil)
	})
	if err != nil {
		return reconcile.Outcome{}, err
	}
	return reconcile.Recorded(), nil
}

// createCoordinationSession records the session and binding of a coordination attempt. The kagent
// Session itself is made by the delivery loop when the brief is delivered.
func (s *Service) createCoordinationSession(ctx context.Context, q *dbq.Queries, t dbq.Task, a dbq.TaskAttempt, prof config.ProviderProfile) error {
	id := *a.SessionID
	if _, err := q.CreateSession(ctx, dbq.CreateSessionParams{
		ID: id, UserID: t.OwnerID, ProjectID: t.ProjectID, Title: t.Title, Prompt: "",
	}); err != nil {
		return err
	}
	if _, err := q.CreateAgentBinding(ctx, dbq.CreateAgentBindingParams{
		SessionID: id, Kind: prof.ID, Role: bindingRole(attemptShape{role: a.Role}),
		MCPGrantKind: dbstate.GrantCoordination, TopicID: t.TopicID,
	}); err != nil {
		return err
	}
	_, err := q.SetSessionStatus(ctx, dbq.SetSessionStatusParams{ID: id, Status: dbstate.SessionActive})
	return err
}

// createRuntime makes the attempt's workspace (code) and waits for its session, then activates the
// attempt. A replay finds the workspace an earlier run made and never makes a second one.
func (s *Service) createRuntime(ctx context.Context, c reconcile.Claim, op dbq.TaskOperation) (reconcile.Outcome, error) {
	q := dbq.New(s.Pool)
	if op.TargetAttemptID == nil {
		return reconcile.Done(string(dbstate.TaskOpBlocked)), nil
	}
	a, err := q.GetTaskAttempt(ctx, *op.TargetAttemptID)
	if err != nil {
		return reconcile.Outcome{}, err
	}
	t, err := q.GetTaskByID(ctx, a.TaskID)
	if err != nil {
		return reconcile.Outcome{}, err
	}
	if a.State != dbstate.AttemptCreating || TaskFinal(t.Status) {
		// A cancel got there first; its own operation settles the attempt.
		return s.stepAside(ctx, c, op)
	}
	if t.Mode == dbstate.TaskModeCode {
		if a.WorkspaceID == nil {
			out, done, err := s.makeWorkspace(ctx, c, op, t, a)
			if err != nil || !done {
				return out, err
			}
		}
		a, err = q.GetTaskAttempt(ctx, a.ID)
		if err != nil {
			return reconcile.Outcome{}, err
		}
		exists, err := q.WorkspaceExists(ctx, *a.WorkspaceID)
		if err != nil {
			return reconcile.Outcome{}, err
		}
		if !exists {
			return s.abort(ctx, c, op, a, "workspace was refused")
		}
		ksid, err := q.GetBindingKagentSession(ctx, *a.BindingID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return reconcile.Outcome{}, err
		}
		if ksid == nil {
			if s.now().Sub(a.CreatedAt) > s.Limits.ProvisionWindow {
				return s.abort(ctx, c, op, a, "workspace session did not start in time")
			}
			return reconcile.RetryAfter(3 * time.Second), nil
		}
	}
	err = s.tx(ctx, func(q *dbq.Queries) ([]events.Draft, error) {
		t, err := q.GetTaskByIDForUpdate(ctx, a.TaskID)
		if err != nil {
			return nil, err
		}
		cur, err := q.GetTaskAttempt(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		if cur.State != dbstate.AttemptCreating || TaskFinal(t.Status) || t.CurrentAttemptID == nil || *t.CurrentAttemptID != a.ID {
			return nil, advance(ctx, q, c, op, dbstate.TaskOpBlocked, nil, nil)
		}
		if _, err := q.MoveAttempt(ctx, dbq.MoveAttemptParams{
			ID: a.ID, FromState: dbstate.AttemptCreating, ToState: dbstate.AttemptActive, CapacityHeld: true,
		}); err != nil {
			return nil, err
		}
		args := dbq.SetTaskAttemptParams{ID: t.ID, CurrentAttemptID: &a.ID, Status: dbstate.TaskRunning}
		if cur.Environment != nil {
			args.AcceptedEnvironment = cur.Environment
		}
		after, err := q.SetTaskAttempt(ctx, args)
		if err != nil {
			return nil, err
		}
		return []events.Draft{s.updated(after)}, advance(ctx, q, c, op, dbstate.TaskOpTargetReady, nil, &a.ID)
	})
	if err != nil {
		return reconcile.Outcome{}, err
	}
	return reconcile.Recorded(), nil
}

// stepAside ends a create operation whose attempt has been taken over by a cancel or an abort.
func (s *Service) stepAside(ctx context.Context, c reconcile.Claim, op dbq.TaskOperation) (reconcile.Outcome, error) {
	err := s.tx(ctx, func(q *dbq.Queries) ([]events.Draft, error) {
		return nil, advance(ctx, q, c, op, dbstate.TaskOpBlocked, nil, nil)
	})
	if err != nil {
		return reconcile.Outcome{}, err
	}
	return reconcile.Recorded(), nil
}

// makeWorkspace gives a code attempt its workspace. It first looks for one an interrupted run made
// for this attempt. done is false when the outcome is already decided.
func (s *Service) makeWorkspace(ctx context.Context, c reconcile.Claim, op dbq.TaskOperation, t dbq.Task, a dbq.TaskAttempt) (out reconcile.Outcome, done bool, err error) {
	q := dbq.New(s.Pool)
	if a.WorkspaceRequestID == nil {
		return reconcile.Outcome{}, false, errors.New("tasks: code attempt has no workspace request id")
	}
	w, err := q.GetWorkspaceByRequestID(ctx, dbq.GetWorkspaceByRequestIDParams{UserID: t.OwnerID, RequestID: *a.WorkspaceRequestID})
	id := w.ID
	if errors.Is(err, pgx.ErrNoRows) {
		if err := s.reserveWorkspaceCreate(ctx, c, op, a); errors.Is(err, pgx.ErrNoRows) {
			out, err = s.stepAside(ctx, c, op)
			return out, false, err
		} else if err != nil {
			return reconcile.Outcome{}, false, err
		}
		// A cancelled handler still records the return. A lost process leaves its reservation
		// unresolved, so an absent workspace is never mistaken for proof of no late commit.
		defer func() {
			finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			err = errors.Join(err, q.FinishAttemptWorkspaceCreate(finishCtx, a.ID))
		}()
		req := api.CreateWorkspaceRequest{ProjectId: t.ProjectID, Branch: t.CheckoutBranch, AgentKind: &a.ProfileID}
		if t.CheckoutRef != nil && *t.CheckoutRef != "" {
			req.Ref = t.CheckoutRef
		}
		if t.CheckoutDepth != nil {
			d := int(*t.CheckoutDepth)
			req.Depth = &d
		}
		lc, cerr := s.Workspaces.CreateWithRequestID(ctx, *a.WorkspaceRequestID, req)
		var rej *workspaces.RejectedError
		if errors.As(cerr, &rej) {
			out, err = s.abort(ctx, c, op, a, "workspace was refused: "+rej.Detail)
			return out, false, err
		}
		// A request that can never succeed ends the attempt; retrying it would only hold the slot.
		var inv *workspaces.InvalidError
		if errors.As(cerr, &inv) || errors.Is(cerr, workspaces.ErrProjectNotFound) {
			out, err = s.abort(ctx, c, op, a, "workspace request is invalid: "+cerr.Error())
			return out, false, err
		}
		var unc *workspaces.UnconfirmedError
		if cerr != nil && !errors.As(cerr, &unc) {
			// Any other failure (database, transport) is tried again with backoff, but only within
			// the provisioning window: after it the attempt is aborted rather than holding its slot.
			if s.now().Sub(a.CreatedAt) > s.Limits.ProvisionWindow {
				out, err = s.abort(ctx, c, op, a, "workspace could not be created in time: "+cerr.Error())
				return out, false, err
			}
			return reconcile.Outcome{}, false, cerr
		}
		if lc.WorkspaceId == "" {
			return reconcile.Outcome{}, false, errors.New("tasks: workspace create returned no id")
		}
		id = lc.WorkspaceId
	} else if err != nil {
		return reconcile.Outcome{}, false, err
	}
	env, err := q.GetWorkspaceEnvironment(ctx, id)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return reconcile.Outcome{}, false, err
	}
	err = s.tx(ctx, func(q *dbq.Queries) ([]events.Draft, error) {
		if _, err := q.GetTaskByIDForUpdate(ctx, a.TaskID); err != nil {
			return nil, err
		}
		if _, err := q.AttachAttemptSession(ctx, dbq.AttachAttemptSessionParams{ID: a.ID, SessionID: &id, Environment: env}); err != nil {
			return nil, err
		}
		_, err := q.SetBindingRole(ctx, dbq.SetBindingRoleParams{SessionID: id, Role: bindingRole(attemptShape{role: a.Role})})
		return nil, err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		cur, err := q.GetTaskAttempt(ctx, a.ID)
		if err != nil {
			return reconcile.Outcome{}, false, err
		}
		if cur.State == dbstate.AttemptCreating {
			out, err = s.abort(ctx, c, op, cur, "workspace disappeared before attachment")
			return out, false, err
		}
		// A cancel or another handler moved the attempt on. Cleanup re-checks that it actually
		// stopped, then deletes only its keyed workspace. If this handler dies or deletion is
		// unconfirmed, the settle sweep keeps resolving the same key.
		if err := s.cleanupWorkspace(ctx, a); err != nil {
			return reconcile.Outcome{}, false, err
		}
		out, err = s.stepAside(ctx, c, op)
		return out, false, err
	}
	if err != nil {
		return reconcile.Outcome{}, false, err
	}
	return reconcile.Outcome{}, true, nil
}

// workspaceRequestID is stable across every replay of an attempt's workspace create.
func workspaceRequestID(attemptID string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("mainloop:task-workspace:"+attemptID)).String()
}

// reserveWorkspaceCreate re-checks authority and commits a reservation before any workspace call.
// The task lock orders it against a drain, including one that races a stale create handler.
func (s *Service) reserveWorkspaceCreate(ctx context.Context, c reconcile.Claim, op dbq.TaskOperation, a dbq.TaskAttempt) error {
	return s.tx(ctx, func(q *dbq.Queries) ([]events.Draft, error) {
		t, err := q.GetTaskByIDForUpdate(ctx, a.TaskID)
		if err != nil {
			return nil, err
		}
		if TaskFinal(t.Status) || t.CurrentAttemptID == nil || *t.CurrentAttemptID != a.ID {
			return nil, pgx.ErrNoRows
		}
		_, err = q.ReserveAttemptWorkspaceCreate(ctx, dbq.ReserveAttemptWorkspaceCreateParams{
			ID: a.ID, OperationID: op.ID, LeaseOwner: &c.Owner, Attempts: int32(c.Attempts), //nolint:gosec // a small counter
		})
		return nil, err
	})
}

// briefMessageID is the ledger id of an attempt's first brief. It is derived from the attempt, so a
// replay finds the message an earlier run recorded.
func briefMessageID(attemptID string) string { return briefSendID(attemptID, 0) }

// briefSendID is the ledger id of the nth send of an attempt's brief: a brief the ledger failed is
// sent again under the next id.
func briefSendID(attemptID string, n int) string {
	key := "mainloop:task-brief:" + attemptID
	if n > 0 {
		key += ":" + strconv.Itoa(n)
	}
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(key)).String()
}

// sendBrief hands the agent its assigned work. The message is recorded in the sessions ledger
// under an id derived from the attempt; the ledger is checked first, so a replay never sends it
// twice.
func (s *Service) sendBrief(ctx context.Context, c reconcile.Claim, op dbq.TaskOperation) (reconcile.Outcome, error) {
	q := dbq.New(s.Pool)
	if op.TargetAttemptID == nil {
		return reconcile.Done(string(dbstate.TaskOpBlocked)), nil
	}
	a, err := q.GetTaskAttempt(ctx, *op.TargetAttemptID)
	if err != nil {
		return reconcile.Outcome{}, err
	}
	if a.State != dbstate.AttemptActive || a.SessionID == nil {
		return s.stepAside(ctx, c, op)
	}
	t, err := q.GetTaskByID(ctx, a.TaskID)
	if err != nil {
		return reconcile.Outcome{}, err
	}
	mid := briefMessageID(a.ID)
	if _, err := q.GetDelivery(ctx, mid); errors.Is(err, pgx.ErrNoRows) {
		_, err := s.Sessions.SendToAs(ctx, sessions.SourceMainloop, *a.SessionID, mid, DelegatedBrief(a.Role, t.Mode, t.Brief))
		switch {
		case errors.Is(err, sessions.ErrNotFound):
			return s.giveUpInitialBrief(ctx, c, op, a, mid)
		case errors.Is(err, sessions.ErrConflict):
			// A session that ended or was archived can never take the brief; a workspace that is not
			// ready yet can, so that is tried again.
			ended, oerr := s.sessionEnded(ctx, t.OwnerID, *a.SessionID)
			if oerr != nil {
				return reconcile.Outcome{}, oerr
			}
			if ended {
				return s.giveUpInitialBrief(ctx, c, op, a, mid)
			}
			return reconcile.RetryAfter(3 * time.Second), nil
		case errors.Is(err, sessions.ErrBusy):
			return reconcile.RetryAfter(3 * time.Second), nil
		case err != nil:
			return reconcile.Outcome{}, err
		}
	} else if err != nil {
		return reconcile.Outcome{}, err
	}
	err = s.tx(ctx, func(q *dbq.Queries) ([]events.Draft, error) {
		if err := q.SetAttemptBrief(ctx, dbq.SetAttemptBriefParams{ID: a.ID, BriefDeliveryID: &mid}); err != nil {
			return nil, err
		}
		return nil, advance(ctx, q, c, op, dbstate.TaskOpCompleted, nil, &a.ID)
	})
	if err != nil {
		return reconcile.Outcome{}, err
	}
	return reconcile.Recorded(), nil
}

// giveUpInitialBrief ends the create operation of an attempt whose session ended before it could take
// its first brief: the attempt gets evidence and its task is blocked for reconciliation, like a
// resent brief that cannot be delivered. The owner decides what happens to the attempt.
func (s *Service) giveUpInitialBrief(ctx context.Context, c reconcile.Claim, op dbq.TaskOperation, a dbq.TaskAttempt, mid string) (reconcile.Outcome, error) {
	if err := s.giveUpBrief(ctx, a.ID, mid); err != nil {
		return reconcile.Outcome{}, err
	}
	reason := dbstate.ReasonReconciliation
	err := s.tx(ctx, func(q *dbq.Queries) ([]events.Draft, error) {
		return nil, advance(ctx, q, c, op, dbstate.TaskOpBlocked, &reason, nil)
	})
	if err != nil {
		return reconcile.Outcome{}, err
	}
	return reconcile.Recorded(), nil
}

// abort ends a provisioning that cannot finish: the attempt goes creating → draining and the task
// fails in one transaction, then the runtime is stopped, the attempt is fenced and it fails.
func (s *Service) abort(ctx context.Context, c reconcile.Claim, op dbq.TaskOperation, a dbq.TaskAttempt, why string) (reconcile.Outcome, error) {
	reason := dbstate.ReasonProvisioningUnavailable
	taken := false
	err := s.tx(ctx, func(q *dbq.Queries) ([]events.Draft, error) {
		t, err := q.GetTaskByIDForUpdate(ctx, a.TaskID)
		if err != nil {
			return nil, err
		}
		cur, err := q.GetTaskAttempt(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		if cur.State != dbstate.AttemptCreating {
			// A cancel got to the attempt first and owns it: the task ends cancelled, not failed.
			taken = true
			return nil, advance(ctx, q, c, op, dbstate.TaskOpBlocked, nil, nil)
		}
		var drafts []events.Draft
		if _, err := q.MoveAttempt(ctx, dbq.MoveAttemptParams{
			ID: a.ID, FromState: dbstate.AttemptCreating, ToState: dbstate.AttemptDraining, CapacityHeld: true,
			EvidenceRef: ptr("provision-aborted: " + why),
		}); err != nil {
			return nil, err
		}
		if !TaskFinal(t.Status) {
			after, err := q.UpdateTaskState(ctx, dbq.UpdateTaskStateParams{ID: t.ID, Version: t.Version, Status: dbstate.TaskFailed})
			if err != nil {
				return nil, err
			}
			drafts = append(drafts, s.updated(after))
		}
		return drafts, advance(ctx, q, c, op, dbstate.TaskOpBlocked, &reason, nil)
	})
	if err != nil {
		return reconcile.Outcome{}, err
	}
	if taken {
		return reconcile.Recorded(), nil
	}
	// The operation is done; the settle sweep finishes the attempt if this is cut short.
	if err := s.settle(ctx, a.ID); err != nil {
		s.log().WarnContext(ctx, "settle after abort", "attempt", a.ID, "error", err)
	}
	return reconcile.Recorded(), nil
}
