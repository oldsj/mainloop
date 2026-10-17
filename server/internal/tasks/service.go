package tasks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/gitref"
	"github.com/oldsj/mainloop/server/internal/providers"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// Resolver finds a provider profile; [providers.Registry] is the implementation.
type Resolver interface {
	Resolve(identifier, role string, selecting bool) (config.ProviderProfile, error)
}

// Service records tasks, operations and reports, and runs the reconcile loops that pick them up:
// provisioning, settlement, delivery of reports and projection. Every change commits together
// with its task:updated event, and no transaction here reaches the network.
type Service struct {
	Pool *pgxpool.Pool
	// Providers resolves profile identifiers.
	Providers Resolver
	// DefaultProfile is the installation default provider profile.
	DefaultProfile string
	// MergeTools is whether the merge tools are on; see [Service.Auth].
	MergeTools bool
	// Now is the clock for event timestamps; time.Now when nil.
	Now func() time.Time

	// Workspaces and Sessions are the runtimes the reconcile loops drive. Without them the loops
	// are not built, and rows written here wait.
	Workspaces WorkspaceRuntime
	Sessions   SessionRuntime
	// Observer reads a task's pull request and CI from GitHub; without one nothing is projected.
	Observer Observer
	// StopObserver resolves a stop kagent did not confirm. Without one the service has no kagent
	// client, and an unconfirmed stop is taken as stopped.
	StopObserver StopObserver
	// Liveness reads whether an active attempt's kagent session is still alive. Without one the
	// liveness sweep does nothing.
	Liveness LivenessObserver
	// Limits are the capacity and timing settings of the loops.
	Limits Limits
	Log    *slog.Logger
}

// Limits are the capacity and timing settings, from configuration.
type Limits struct {
	// MaxChildrenPerParent and MaxChildrenGlobal cap the child tasks that hold a slot: an attempt
	// that still holds capacity, or a task not yet given an attempt.
	MaxChildrenPerParent int
	MaxChildrenGlobal    int
	// ProvisionWindow is how long a new attempt may wait before it is aborted.
	ProvisionWindow time.Duration
	// Interval is the poll of the operation and delivery loops.
	Interval time.Duration
	// ProjectionEvery is the period of the projection sweep.
	ProjectionEvery time.Duration
	// SettleIdle is how long an attempt must have been unchanged before the settle sweep takes it;
	// the default is 30 seconds.
	SettleIdle time.Duration
	// ResolveBackoff paces the looks at kagent for a stop it did not confirm; ResolveBound is how many
	// unsuccessful looks come before the task is blocked for reconciliation. Zero values take defaults.
	ResolveBackoff reconcile.Backoff
	ResolveBound   int
	// LivenessEvery is the period of the liveness sweep; StallTimeout and MaxRuntime are its no-progress
	// and overall bounds. Zero values take the defaults in internal/tasks/liveness.go.
	LivenessEvery time.Duration
	StallTimeout  time.Duration
	MaxRuntime    time.Duration
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// digest is the idempotency digest of a request: the kind, the task it targets and the payload,
// as canonical JSON (encoding/json sorts map keys).
func digest(kind dbstate.TaskOpKind, taskID string, payload map[string]any) (string, json.RawMessage, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", nil, err
	}
	canon, err := json.Marshal(map[string]any{"kind": kind, "task_id": taskID, "payload": payload})
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), raw, nil
}

// updated is the task:updated draft for a task row as it stands after a change.
func (s *Service) updated(t dbq.Task) events.Draft {
	return events.Draft{UserID: t.OwnerID, Type: events.TypeTaskUpdated, Payload: api.TaskUpdated{
		AttemptId:    t.CurrentAttemptID,
		EventId:      uuid.NewString(),
		OccurredAt:   s.now(),
		ParentTaskId: t.ParentTaskID,
		RootTaskId:   t.RootTaskID,
		TaskId:       t.ID,
		Type:         "task:updated",
		Version:      int(t.Version),
	}}
}

// request is the idempotency check every operation starts with. It takes the request-key lock,
// so concurrent copies of one request run one after the other, then looks for the stored
// operation: the same digest replays it, a different one is refused.
func request(ctx context.Context, q *dbq.Queries, p Principal, requestID, digest string) (dbq.TaskOperation, bool, error) {
	if err := q.LockTaskRequest(ctx, p.OwnerID+"\x1f"+p.Key()+"\x1f"+requestID); err != nil {
		return dbq.TaskOperation{}, false, err
	}
	op, err := q.GetTaskOperationByKey(ctx, dbq.GetTaskOperationByKeyParams{
		OwnerID: p.OwnerID, PrincipalKey: p.Key(), RequestID: requestID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return dbq.TaskOperation{}, false, nil
	}
	if err != nil {
		return dbq.TaskOperation{}, false, err
	}
	if op.RequestDigest != digest {
		return dbq.TaskOperation{}, false, refusal(409, ReasonPayloadConflict)
	}
	return op, true, nil
}

// CreateInput is a create request. Empty optional strings mean not given.
type CreateInput struct {
	RequestID         string
	Title             string
	Brief             string
	Mode              dbstate.TaskMode
	ProjectID         string
	TopicID           string
	ProviderProfileID string
	Checkout          *Checkout
}

// Checkout is the repository position a code task starts from.
type Checkout struct {
	Branch string
	Ref    string
	Depth  int
}

func (in CreateInput) payload() map[string]any {
	m := map[string]any{
		"title": in.Title, "brief": in.Brief, "mode": in.Mode,
		"project_id": nilIfEmpty(in.ProjectID), "topic_id": nilIfEmpty(in.TopicID),
		"provider_profile_id": nilIfEmpty(in.ProviderProfileID), "checkout": nil,
	}
	if in.Checkout != nil {
		m["checkout"] = map[string]any{"branch": in.Checkout.Branch, "ref": in.Checkout.Ref, "depth": in.Checkout.Depth}
	}
	return m
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func ptr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// selection is the provider a new task is assigned.
type selection struct {
	profile    config.ProviderProfile
	source     dbstate.TaskSelection
	constraint *string
}

// selectProfile resolves a task's provider: the parent's constraint wins, then an explicit
// choice, then the project's default, then the installation default. A root task gets a
// supervisor Agent and a child task a child Agent.
func (s *Service) selectProfile(ctx context.Context, q *dbq.Queries, in CreateInput, project *string, parent *dbq.Task) (selection, error) {
	role := providers.RoleSupervisor
	if parent != nil {
		role = roleChild
	}
	resolve := func(id string) (config.ProviderProfile, error) {
		prof, err := s.Providers.Resolve(id, role, true)
		switch {
		case err == nil:
			return prof, nil
		case errors.Is(err, providers.ErrUnknown):
			return prof, refusal(422, ReasonProviderUnknown)
		case errors.Is(err, providers.ErrNoRole):
			return prof, refusal(422, ReasonProviderRole)
		case errors.Is(err, providers.ErrDisabled):
			return prof, refusal(422, ReasonProviderDisabled)
		}
		return prof, err
	}
	if parent != nil && parent.ProviderConstraint != nil {
		forced, err := resolve(*parent.ProviderConstraint)
		if err != nil {
			return selection{}, err
		}
		if in.ProviderProfileID != "" {
			asked, err := resolve(in.ProviderProfileID)
			if err != nil {
				return selection{}, err
			}
			if asked.ID != forced.ID {
				return selection{}, refusal(403, ReasonInheritedProvider)
			}
		}
		return selection{profile: forced, source: dbstate.SelectionInherited, constraint: parent.ProviderConstraint}, nil
	}
	if in.ProviderProfileID != "" {
		prof, err := resolve(in.ProviderProfileID)
		if err != nil {
			return selection{}, err
		}
		sel := selection{profile: prof, source: dbstate.SelectionExplicit}
		if parent == nil {
			sel.constraint = &prof.ID
		}
		return sel, nil
	}
	if project != nil {
		pref, err := q.GetProviderPreference(ctx, *project)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return selection{}, err
		}
		if err == nil && pref.ProfileID != nil {
			prof, err := resolve(*pref.ProfileID)
			if err != nil {
				return selection{}, err
			}
			return selection{profile: prof, source: dbstate.SelectionProjectDefault}, nil
		}
	}
	prof, err := resolve(s.DefaultProfile)
	if err != nil {
		return selection{}, err
	}
	return selection{profile: prof, source: dbstate.SelectionInstallationDefault}, nil
}

// Create records a task and its create operation, both with no reason: nothing has gone wrong
// yet, and provisioning has not been attempted. It leaves the operation requested: the task has
// no attempt until provisioning picks the operation up. A repeat of the same request returns the
// same operation. Reason provisioning_unavailable is reserved for an operation the reconcile loop
// has actually blocked, never for one merely awaiting its first look.
func (s *Service) Create(ctx context.Context, p Principal, in CreateInput) (api.TaskOperation, error) {
	if !p.canManage() {
		return api.TaskOperation{}, refusal(403, ReasonManagementDenied)
	}
	if err := validateCreate(in); err != nil {
		return api.TaskOperation{}, err
	}
	dig, payload, err := digest(dbstate.TaskOpCreate, "", in.payload())
	if err != nil {
		return api.TaskOperation{}, err
	}
	var op dbq.TaskOperation
	err = events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		q := dbq.New(tx)
		stored, replay, err := request(ctx, q, p, in.RequestID, dig)
		if err != nil {
			return nil, err
		}
		if replay {
			op = stored
			return nil, nil
		}
		t, err := s.insertTask(ctx, q, p, in)
		if err != nil {
			return nil, err
		}
		op, err = q.InsertTaskOperation(ctx, dbq.InsertTaskOperationParams{
			ID: uuid.NewString(), OwnerID: p.OwnerID, PrincipalKey: p.Key(), RequestID: in.RequestID,
			RequestDigest: dig, RequestPayload: payload, Kind: dbstate.TaskOpCreate, TaskID: &t.ID,
			State: dbstate.TaskOpRequested, LastConfirmedStep: dbstate.TaskOpRequested,
		})
		if err != nil {
			return nil, err
		}
		if err := s.notify(ctx, q, OpChannel, op.ID); err != nil {
			return nil, err
		}
		return []events.Draft{s.updated(t)}, nil
	})
	if err != nil {
		return api.TaskOperation{}, err
	}
	return toOperation(op)
}

// Limits the database also enforces; checked here so a request over them is refused, not a 500.
const (
	maxBriefBytes    = 16384
	maxCheckoutDepth = 1000
)

func validateCreate(in CreateInput) error {
	switch in.Mode {
	case dbstate.TaskModeCode:
		if in.Checkout == nil {
			return refusal(422, ReasonCodeNeedsCheckout)
		}
		c := in.Checkout
		if !gitref.ValidCheckoutRef(c.Branch) || c.Ref != "" && !gitref.ValidCheckoutRef(c.Ref) {
			return refusal(422, ReasonInvalidCheckout)
		}
		if c.Depth < 0 || c.Depth > maxCheckoutDepth {
			return refusal(422, ReasonInvalidCheckout)
		}
	case dbstate.TaskModeCoordination:
		if in.Checkout != nil {
			return refusal(422, ReasonCoordinationCheckout)
		}
	default:
		return refusal(422, ReasonInvalidRequest)
	}
	if len(in.Brief) > maxBriefBytes {
		return refusal(422, ReasonBriefTooLong)
	}
	return nil
}

func (s *Service) insertTask(ctx context.Context, q *dbq.Queries, p Principal, in CreateInput) (dbq.Task, error) {
	var parent *dbq.Task
	if p.Role == dbstate.RoleSupervisor {
		t, err := q.GetTaskForUpdate(ctx, dbq.GetTaskForUpdateParams{ID: p.TaskID, OwnerID: p.OwnerID})
		if err != nil {
			return dbq.Task{}, notFoundOr(err, ReasonTaskNotFound)
		}
		if err := ensureActive(ctx, q, p); err != nil {
			return dbq.Task{}, err
		}
		if TaskFinal(t.Status) {
			return dbq.Task{}, refusal(409, ReasonParentNotActive)
		}
		parent = &t
		if err := s.checkCapacity(ctx, q, p.OwnerID, t.ID); err != nil {
			return dbq.Task{}, err
		}
	}
	project, topic := ptr(in.ProjectID), ptr(in.TopicID)
	if parent != nil {
		if project != nil && (parent.ProjectID == nil || *project != *parent.ProjectID) {
			return dbq.Task{}, refusal(403, ReasonInheritedProject)
		}
		if project == nil {
			project = parent.ProjectID
		}
		if topic == nil {
			topic = parent.TopicID
		}
	}
	if in.Mode == dbstate.TaskModeCode && project == nil {
		return dbq.Task{}, refusal(422, ReasonCodeNeedsProject)
	}
	if project != nil {
		owner, err := q.GetProjectOwner(ctx, *project)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return dbq.Task{}, err
		}
		if err != nil || owner != p.OwnerID {
			return dbq.Task{}, refusal(404, ReasonProjectNotFound)
		}
		if in.Mode == dbstate.TaskModeCode {
			// A code task pushes its branch, and the push gate attests only a workspace pinned to a
			// development environment. Refuse now, with an action the owner can take, rather than start a
			// workspace that can never push.
			if _, err := q.GetWorkspaceEnvironmentVersion(ctx, *project); errors.Is(err, pgx.ErrNoRows) {
				return dbq.Task{}, refusal(409, ReasonEnvironmentRequired)
			} else if err != nil {
				return dbq.Task{}, err
			}
		}
	}
	if topic != nil {
		tp, err := q.GetTopic(ctx, *topic)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return dbq.Task{}, err
		}
		if err != nil || tp.UserID != p.OwnerID {
			return dbq.Task{}, refusal(404, ReasonTopicNotFound)
		}
	}
	sel, err := s.selectProfile(ctx, q, in, project, parent)
	if err != nil {
		return dbq.Task{}, err
	}
	id := uuid.NewString()
	root := id
	var parentID *string
	if parent != nil {
		root, parentID = parent.RootTaskID, &parent.ID
	}
	args := dbq.InsertTaskParams{
		ID: id, OwnerID: p.OwnerID, ProjectID: project, TopicID: topic, ParentTaskID: parentID, RootTaskID: root,
		CreatorBindingID: ptr(p.BindingID), Title: in.Title, Brief: in.Brief, Mode: in.Mode,
		AssignedProfileID: sel.profile.ID, SelectionSource: sel.source, ProviderConstraint: sel.constraint,
	}
	if c := in.Checkout; c != nil {
		depth := int32(c.Depth) //nolint:gosec // validate bounds it to 0..1000
		args.CheckoutBranch, args.CheckoutRef, args.CheckoutDepth = &c.Branch, &c.Ref, &depth
	}
	return q.InsertTask(ctx, args)
}

// checkCapacity refuses a new child when its parent or the owner already has as many children as
// the limits allow. A child counts from the moment its task is recorded until its attempt has
// stopped, so the count is taken under an owner-wide lock that serializes concurrent creates.
func (s *Service) checkCapacity(ctx context.Context, q *dbq.Queries, owner, parent string) error {
	if err := q.LockTaskRequest(ctx, "capacity\x1f"+owner); err != nil {
		return err
	}
	if lim := s.Limits.MaxChildrenPerParent; lim > 0 {
		n, err := q.CountParentCapacity(ctx, dbq.CountParentCapacityParams{OwnerID: owner, ParentTaskID: &parent})
		if err != nil {
			return err
		}
		if n >= int64(lim) {
			return refusal(409, ReasonChildCapacity)
		}
	}
	if lim := s.Limits.MaxChildrenGlobal; lim > 0 {
		n, err := q.CountGlobalCapacity(ctx, owner)
		if err != nil {
			return err
		}
		if n >= int64(lim) {
			return refusal(409, ReasonGlobalCapacity)
		}
	}
	return nil
}

func notFoundOr(err error, reason string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return refusal(404, reason)
	}
	return err
}

// ActionInput is a cancel, retry or reassign request. The expected version and attempt are the
// caller's view of the task; a task that has moved on is refused.
type ActionInput struct {
	RequestID         string
	TaskID            string
	ExpectedVersion   int
	ExpectedAttemptID string
	// TargetProfileID is the new provider of a reassign.
	TargetProfileID string
}

func (in ActionInput) payload(kind dbstate.TaskOpKind) map[string]any {
	m := map[string]any{
		"expected_version": in.ExpectedVersion, "expected_attempt_id": nilIfEmpty(in.ExpectedAttemptID),
	}
	if kind == dbstate.TaskOpReassign {
		m["target_profile_id"] = in.TargetProfileID
	}
	return m
}

// lockManaged loads and locks a task the principal may manage, and checks the caller's view of it.
func lockManaged(ctx context.Context, q *dbq.Queries, p Principal, in ActionInput) (dbq.Task, error) {
	if !p.canManage() {
		return dbq.Task{}, refusal(403, ReasonManagementDenied)
	}
	t, err := q.GetTaskForUpdate(ctx, dbq.GetTaskForUpdateParams{ID: in.TaskID, OwnerID: p.OwnerID})
	if err != nil {
		return t, notFoundOr(err, ReasonTaskNotFound)
	}
	if err := ensureActive(ctx, q, p); err != nil {
		return t, err
	}
	read, manage := p.access(t)
	if !read {
		return t, refusal(404, ReasonTaskNotFound)
	}
	if !manage {
		return t, refusal(403, ReasonManagementDenied)
	}
	current := ""
	if t.CurrentAttemptID != nil {
		current = *t.CurrentAttemptID
	}
	if current != in.ExpectedAttemptID {
		return t, refusal(409, ReasonStaleAttempt)
	}
	if int(t.Version) != in.ExpectedVersion {
		return t, refusal(409, ReasonStaleVersion)
	}
	return t, nil
}

// Cancel cancels a task. With no attempt there is nothing to stop, so the task is cancelled and
// the operation completes at once. With an attempt the operation waits in draining until the
// attempt has been stopped; the task keeps its status until then. Cancelling a task that is
// already final completes an operation and changes nothing.
func (s *Service) Cancel(ctx context.Context, p Principal, in ActionInput) (api.TaskOperation, error) {
	return s.act(ctx, p, dbstate.TaskOpCancel, in)
}

// Retry records a retry that waits for handoff, which is not available yet: the operation is
// blocked with reason handoff_unavailable.
func (s *Service) Retry(ctx context.Context, p Principal, in ActionInput) (api.TaskOperation, error) {
	return s.act(ctx, p, dbstate.TaskOpRetry, in)
}

// Reassign is like [Service.Retry] but names the provider the task would move to.
func (s *Service) Reassign(ctx context.Context, p Principal, in ActionInput) (api.TaskOperation, error) {
	return s.act(ctx, p, dbstate.TaskOpReassign, in)
}

func (s *Service) act(ctx context.Context, p Principal, kind dbstate.TaskOpKind, in ActionInput) (api.TaskOperation, error) {
	dig, payload, err := digest(kind, in.TaskID, in.payload(kind))
	if err != nil {
		return api.TaskOperation{}, err
	}
	var op dbq.TaskOperation
	err = events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		q := dbq.New(tx)
		stored, replay, err := request(ctx, q, p, in.RequestID, dig)
		if err != nil {
			return nil, err
		}
		if replay {
			op = stored
			return nil, nil
		}
		t, err := lockManaged(ctx, q, p, in)
		if err != nil {
			return nil, err
		}
		newOp := dbq.InsertTaskOperationParams{
			ID: uuid.NewString(), OwnerID: p.OwnerID, PrincipalKey: p.Key(), RequestID: in.RequestID,
			RequestDigest: dig, RequestPayload: payload, Kind: kind, TaskID: &t.ID,
			State: dbstate.TaskOpRequested, LastConfirmedStep: dbstate.TaskOpRequested, AttemptID: t.CurrentAttemptID,
		}
		var after dbq.Task
		switch kind {
		case dbstate.TaskOpCancel:
			after, err = s.cancel(ctx, q, t, &newOp)
		default:
			after, err = s.handoff(q, ctx, t, kind, in, &newOp)
		}
		if err != nil {
			return nil, err
		}
		if op, err = q.InsertTaskOperation(ctx, newOp); err != nil {
			return nil, err
		}
		if err := s.notify(ctx, q, OpChannel, op.ID); err != nil {
			return nil, err
		}
		if after.Version == t.Version {
			return nil, nil
		}
		return []events.Draft{s.updated(after)}, nil
	})
	if err != nil {
		return api.TaskOperation{}, err
	}
	return toOperation(op)
}

// cancel shapes the cancel operation and moves the task when it can be moved at once.
func (s *Service) cancel(ctx context.Context, q *dbq.Queries, t dbq.Task, op *dbq.InsertTaskOperationParams) (dbq.Task, error) {
	switch {
	case TaskFinal(t.Status):
		op.State, op.LastConfirmedStep = dbstate.TaskOpCompleted, dbstate.TaskOpCompleted
		return t, nil
	case t.CurrentAttemptID != nil:
		if err := TransitionOperation(dbstate.TaskOpCancel, dbstate.TaskOpRequested, dbstate.TaskOpRequested, dbstate.TaskOpDraining); err != nil {
			return t, err
		}
		op.State, op.LastConfirmedStep, op.SourceAttemptID = dbstate.TaskOpDraining, dbstate.TaskOpDraining, t.CurrentAttemptID
		return q.TouchTask(ctx, dbq.TouchTaskParams{ID: t.ID, Version: t.Version})
	}
	from := TaskState{Status: t.Status}
	if t.Reason != nil {
		from.Reason = *t.Reason
	}
	if err := TransitionTask(from, TaskState{Status: dbstate.TaskCancelled}); err != nil {
		return t, err
	}
	after, err := q.UpdateTaskState(ctx, dbq.UpdateTaskStateParams{ID: t.ID, Version: t.Version, Status: dbstate.TaskCancelled})
	if err != nil {
		return t, err
	}
	if err := q.BlockRequestedCreateOps(ctx, &t.ID); err != nil {
		return t, err
	}
	op.State, op.LastConfirmedStep = dbstate.TaskOpCompleted, dbstate.TaskOpCompleted
	return after, nil
}

// handoff shapes a retry or reassign operation. Handoff is never installed, so every request on
// an open task, of either mode, ends in the same blocked operation with reason
// handoff_unavailable; only a task that has ended and a reassign target that could never be used
// are refused.
func (s *Service) handoff(q *dbq.Queries, ctx context.Context, t dbq.Task, kind dbstate.TaskOpKind, in ActionInput, op *dbq.InsertTaskOperationParams) (dbq.Task, error) {
	if TaskFinal(t.Status) {
		return t, refusal(409, ReasonTaskTerminal)
	}
	if kind == dbstate.TaskOpReassign {
		role := providers.RoleSupervisor
		if t.ParentTaskID != nil {
			role = roleChild
		}
		if _, err := s.Providers.Resolve(in.TargetProfileID, role, true); err != nil {
			switch {
			case errors.Is(err, providers.ErrUnknown):
				return t, refusal(422, ReasonProviderUnknown)
			case errors.Is(err, providers.ErrNoRole):
				return t, refusal(422, ReasonProviderRole)
			case errors.Is(err, providers.ErrDisabled):
				return t, refusal(422, ReasonProviderDisabled)
			}
			return t, err
		}
	}
	reason := dbstate.ReasonHandoffUnavailable
	op.State, op.Reason, op.SourceAttemptID = dbstate.TaskOpBlocked, &reason, t.CurrentAttemptID
	return q.TouchTask(ctx, dbq.TouchTaskParams{ID: t.ID, Version: t.Version})
}

// ReportInput is a report from the attempt that does the work.
type ReportInput struct {
	TaskID       string
	AttemptID    string
	RequestID    string
	Summary      string
	Outcome      dbstate.ReportOutcome
	OwnerAction  string
	EvidenceRefs []string
}

// Reported is the answer to a report.
type Reported struct {
	Report  api.TaskReport
	Version int
	Status  api.TaskStatus
	// Replayed is set when the request had been recorded before.
	Replayed bool
}

// Report records what an attempt reports and moves the task accordingly. It is idempotent by
// attempt and request id. Deliveries to the parent task and the main thread are recorded as
// pending rows in the same transaction; sending them is the reconcile loop's work.
func (s *Service) Report(ctx context.Context, p Principal, in ReportInput) (Reported, error) {
	if !p.isTaskAgent() || p.TaskID != in.TaskID || p.AttemptID != in.AttemptID {
		return Reported{}, refusal(403, ReasonReportScope)
	}
	if in.OwnerAction != "" && in.Outcome != dbstate.OutcomeBlocked {
		return Reported{}, refusal(422, ReasonOwnerActionOutcome)
	}
	refs := in.EvidenceRefs
	if refs == nil {
		refs = []string{}
	}
	dig, _, err := digest("report", in.TaskID, map[string]any{
		"attempt_id": in.AttemptID, "summary": in.Summary, "outcome": in.Outcome,
		"owner_action": nilIfEmpty(in.OwnerAction), "evidence_refs": refs,
	})
	if err != nil {
		return Reported{}, err
	}
	var out Reported
	err = events.InTxPublish(ctx, s.Pool, func(tx pgx.Tx) ([]events.Draft, error) {
		q := dbq.New(tx)
		if err := q.LockTaskRequest(ctx, "report\x1f"+in.AttemptID+"\x1f"+in.RequestID); err != nil {
			return nil, err
		}
		prior, err := q.GetTaskReportByRequest(ctx, dbq.GetTaskReportByRequestParams{AttemptID: in.AttemptID, RequestID: in.RequestID})
		if err == nil {
			if prior.RequestDigest != dig {
				return nil, refusal(409, ReasonReportConflict)
			}
			t, err := q.GetTask(ctx, dbq.GetTaskParams{ID: in.TaskID, OwnerID: p.OwnerID})
			if err != nil {
				return nil, err
			}
			out = Reported{Report: toReport(prior), Version: int(t.Version), Status: api.TaskStatus(t.Status), Replayed: true}
			return nil, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		t, err := q.GetTaskForUpdate(ctx, dbq.GetTaskForUpdateParams{ID: in.TaskID, OwnerID: p.OwnerID})
		if err != nil {
			return nil, notFoundOr(err, ReasonTaskNotFound)
		}
		if t.CurrentAttemptID == nil || *t.CurrentAttemptID != in.AttemptID {
			return nil, refusal(403, ReasonInactivePrincipal)
		}
		if err := ensureActive(ctx, q, p); err != nil {
			return nil, err
		}
		after, report, err := s.applyReport(ctx, q, t, in, dig, refs)
		if err != nil {
			return nil, err
		}
		out = Reported{Report: toReport(report), Version: int(after.Version), Status: api.TaskStatus(after.Status)}
		return []events.Draft{s.updated(after)}, nil
	})
	return out, err
}

func (s *Service) applyReport(ctx context.Context, q *dbq.Queries, t dbq.Task, in ReportInput, dig string, refs []string) (dbq.Task, dbq.TaskReport, error) {
	alreadyDone := t.Status == dbstate.TaskCompleted
	if alreadyDone {
		if in.Outcome != dbstate.OutcomeCompleted || t.Mode != dbstate.TaskModeCode {
			return t, dbq.TaskReport{}, refusal(403, ReasonCompletedOutcome)
		}
		done, err := q.HasCompletedReport(ctx, t.ID)
		if err != nil {
			return t, dbq.TaskReport{}, err
		}
		if done {
			return t, dbq.TaskReport{}, refusal(409, ReasonCompletedRecorded)
		}
	} else if TaskFinal(t.Status) {
		return t, dbq.TaskReport{}, refusal(409, ReasonTaskTerminal)
	}
	if in.Outcome == dbstate.OutcomeCompleted && t.Mode == dbstate.TaskModeCoordination {
		live, err := q.CountParentCapacity(ctx, dbq.CountParentCapacityParams{OwnerID: t.OwnerID, ParentTaskID: &t.ID})
		if err != nil {
			return t, dbq.TaskReport{}, err
		}
		if live > 0 {
			return t, dbq.TaskReport{}, refusal(409, ReasonLiveChildren)
		}
	}

	reportID := uuid.NewString()
	report, err := q.InsertTaskReport(ctx, dbq.InsertTaskReportParams{
		ID: reportID, TaskID: t.ID, AttemptID: in.AttemptID, RequestID: in.RequestID, RequestDigest: dig,
		Summary: in.Summary, Outcome: in.Outcome, OwnerAction: ptr(in.OwnerAction), EvidenceRefs: refs,
		TaskVersion: t.Version + 1,
	})
	if err != nil {
		return t, report, err
	}

	from := TaskState{Status: t.Status}
	if t.Reason != nil {
		from.Reason = *t.Reason
	}
	to := reportedState(t, in)
	var after dbq.Task
	// A new owner question replaces the one on the task even when status and reason stay the same.
	if alreadyDone || to == from && in.OwnerAction == "" {
		after, err = q.TouchTask(ctx, dbq.TouchTaskParams{ID: t.ID, Version: t.Version})
	} else {
		if err = TransitionTask(from, to); err != nil {
			return t, report, err
		}
		args := dbq.UpdateTaskStateParams{ID: t.ID, Version: t.Version, Status: to.Status}
		if to.Reason != "" {
			args.Reason = &to.Reason
		}
		if in.OwnerAction != "" {
			args.DecisionAttemptID, args.DecisionReportID, args.DecisionAction = &in.AttemptID, &reportID, &in.OwnerAction
		}
		after, err = q.UpdateTaskState(ctx, args)
	}
	if err != nil {
		return t, report, err
	}
	if in.Outcome != dbstate.OutcomeProgress && !alreadyDone {
		ref := "report:" + reportID
		if err := q.SetAttemptResult(ctx, dbq.SetAttemptResultParams{ID: in.AttemptID, ResultRef: &ref}); err != nil {
			return t, report, err
		}
	}
	var recipients []string
	if t.ParentTaskID != nil {
		recipients = append(recipients, "parent-task:"+*t.ParentTaskID)
	}
	if in.Outcome == dbstate.OutcomeBlocked || in.Outcome == dbstate.OutcomeFailed {
		recipients = append(recipients, "main")
	}
	for _, r := range recipients {
		id := uuid.NewString()
		if err := q.InsertTaskEventDelivery(ctx, dbq.InsertTaskEventDeliveryParams{ID: id, ReportID: reportID, RecipientKey: r}); err != nil {
			return t, report, err
		}
		if err := s.notify(ctx, q, EventChannel, id); err != nil {
			return t, report, err
		}
	}
	return after, report, nil
}

// reportedState is the state a report moves its task to.
func reportedState(t dbq.Task, in ReportInput) TaskState {
	cur := TaskState{Status: t.Status}
	if t.Reason != nil {
		cur.Reason = *t.Reason
	}
	switch in.Outcome {
	case dbstate.OutcomeCompleted:
		if t.Mode == dbstate.TaskModeCode {
			return TaskState{Status: dbstate.TaskWaiting, Reason: dbstate.ReasonPublication}
		}
		return TaskState{Status: dbstate.TaskWaiting, Reason: dbstate.ReasonReconciliation}
	case dbstate.OutcomeBlocked:
		// Only a blocked report may carry an owner action; Report refuses it on any other outcome.
		if in.OwnerAction != "" {
			return TaskState{Status: dbstate.TaskBlocked, Reason: dbstate.ReasonOwnerDecision}
		}
		return TaskState{Status: dbstate.TaskBlocked, Reason: dbstate.ReasonReconciliation}
	case dbstate.OutcomeFailed:
		return TaskState{Status: dbstate.TaskBlocked, Reason: dbstate.ReasonReconciliation}
	}
	// A progress report proves the attempt is still working, which recovers owner_decision (the
	// owner answered) and the liveness sweep's own session_lost/stalled (it was wrong). It does not
	// recover runtime_limit: that bound fires regardless of activity, so a report alone must not
	// clear it; only the owner's own action on the blocked task does.
	if cur.Status == dbstate.TaskBlocked {
		switch cur.Reason {
		case dbstate.ReasonOwnerDecision, dbstate.ReasonSessionLost, dbstate.ReasonStalled:
			return TaskState{Status: dbstate.TaskRunning}
		}
	}
	return cur
}

// Get returns one task the principal may read, with its attempts, operations, artifacts and
// reports. History is the same view.
func (s *Service) Get(ctx context.Context, p Principal, id string) (api.TaskView, error) {
	q := dbq.New(s.Pool)
	t, err := q.GetTask(ctx, dbq.GetTaskParams{ID: id, OwnerID: p.OwnerID})
	if err != nil {
		return api.TaskView{}, notFoundOr(err, ReasonTaskNotFound)
	}
	if read, _ := p.access(t); !read {
		return api.TaskView{}, refusal(404, ReasonTaskNotFound)
	}
	views, err := s.load(ctx, q, []dbq.Task{t})
	if err != nil {
		return api.TaskView{}, err
	}
	if p.Role != "" {
		hideOwnerFields(&views[0])
	}
	return views[0], nil
}

// List returns the tasks the principal may read, oldest first, optionally narrowed to a project
// or a parent task.
func (s *Service) List(ctx context.Context, p Principal, projectID, parentID string) ([]api.TaskView, error) {
	q := dbq.New(s.Pool)
	all, err := q.ListTasks(ctx, dbq.ListTasksParams{OwnerID: p.OwnerID, ProjectID: ptr(projectID), ParentTaskID: ptr(parentID)})
	if err != nil {
		return nil, err
	}
	visible := all[:0:0]
	for _, t := range all {
		if read, _ := p.access(t); read {
			visible = append(visible, t)
		}
	}
	views, err := s.load(ctx, q, visible)
	if err != nil {
		return nil, err
	}
	if p.Role != "" {
		for i := range views {
			hideOwnerFields(&views[i])
		}
	}
	return views, nil
}

func (s *Service) load(ctx context.Context, q *dbq.Queries, tasks []dbq.Task) ([]api.TaskView, error) {
	if len(tasks) == 0 {
		return []api.TaskView{}, nil
	}
	ids := make([]string, len(tasks))
	for i, t := range tasks {
		ids[i] = t.ID
	}
	attempts, err := q.ListTaskAttempts(ctx, ids)
	if err != nil {
		return nil, err
	}
	ops, err := q.ListTaskOperations(ctx, ids)
	if err != nil {
		return nil, err
	}
	arts, err := q.ListTaskArtifacts(ctx, ids)
	if err != nil {
		return nil, err
	}
	reports, err := q.ListTaskReports(ctx, ids)
	if err != nil {
		return nil, err
	}
	return buildViews(tasks, attempts, ops, arts, reports)
}

// Operation returns an operation the principal may see: one on a task it can read, or one it
// made itself.
func (s *Service) Operation(ctx context.Context, p Principal, id string) (api.TaskOperation, error) {
	q := dbq.New(s.Pool)
	op, err := q.GetTaskOperation(ctx, dbq.GetTaskOperationParams{ID: id, OwnerID: p.OwnerID})
	if err != nil {
		return api.TaskOperation{}, notFoundOr(err, ReasonOperationNotFound)
	}
	visible := op.PrincipalKey == p.Key()
	if !visible && op.TaskID != nil {
		t, err := q.GetTask(ctx, dbq.GetTaskParams{ID: *op.TaskID, OwnerID: p.OwnerID})
		if err != nil {
			return api.TaskOperation{}, err
		}
		visible, _ = p.access(t)
	}
	if !visible {
		return api.TaskOperation{}, refusal(404, ReasonOperationNotFound)
	}
	out, err := toOperation(op)
	if err == nil && p.Role != "" {
		out.PrincipalKey, out.RequestDigest, out.RequestPayload = "", "", map[string]any{}
	}
	return out, err
}

// roleChild is the provider Agent role of a child task's attempt. The registry names the roles
// of a profile's Agents; this one is not exported there.
const roleChild = "child"

// Option sets a collaborator only some processes have.
type Option func(*Service)

// WithRuntimes sets the workspace and session runtimes the reconcile loops drive. Without them the
// loops are not built, and rows written here wait (the MCP process only writes rows). Pass the
// concrete services, never a nil pointer in an interface.
func WithRuntimes(ws WorkspaceRuntime, sess SessionRuntime) Option {
	return func(s *Service) { s.Workspaces, s.Sessions = ws, sess }
}

// WithObserver sets the reader of a task's pull request and CI. Without one nothing is projected.
func WithObserver(o Observer) Option { return func(s *Service) { s.Observer = o } }

// WithStopObserver sets the confirmation of a stop kagent did not confirm. Without one the service has
// no kagent client, and an unconfirmed stop is taken as stopped.
func WithStopObserver(o StopObserver) Option { return func(s *Service) { s.StopObserver = o } }

// WithLivenessObserver sets the reader of an active attempt's kagent session. Without one the
// liveness sweep does nothing.
func WithLivenessObserver(o LivenessObserver) Option { return func(s *Service) { s.Liveness = o } }

// NewService builds the task service from configuration. reg resolves provider profiles; build it
// once with [providers.NewRegistry] so a malformed profile fails at startup. The options carry what
// only some processes have.
func NewService(cfg *config.Config, pool *pgxpool.Pool, reg Resolver, log *slog.Logger, opts ...Option) *Service {
	s := &Service{
		Pool: pool, Providers: reg, Log: log, DefaultProfile: cfg.Tasks.DefaultProviderProfileID,
		MergeTools: bool(cfg.MergeToolsEnabled),
		Limits: Limits{
			MaxChildrenPerParent: cfg.Tasks.MaxChildrenPerParent, MaxChildrenGlobal: cfg.Tasks.MaxChildrenGlobal,
			ProvisionWindow: cfg.Tasks.ProvisionWindow.Duration(), Interval: cfg.Tasks.ReconcileInterval.Duration(),
			ProjectionEvery: cfg.Tasks.ProjectionEvery.Duration(),
			StallTimeout:    cfg.Tasks.StallTimeout.Duration(), MaxRuntime: cfg.Tasks.MaxRuntime.Duration(),
		},
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// AuthDepth is the [agentauth.DepthFunc] of the MCP listener: a supervisor or child binding gets
// its depth from its task attempt, and is refused unless that attempt is the live current one.
func (s *Service) AuthDepth(ctx context.Context, row dbq.GetMCPActorRow) (depth int, mergeStatusOnly bool, err error) {
	switch row.Role {
	case dbstate.RoleSupervisor, dbstate.RoleChild:
		st, err := s.Auth(ctx, row.Role, row.MCPGrantKind, row.BindingID)
		if err != nil {
			return 0, false, err
		}
		return st.Depth, st.MergeStatusOnly, nil
	}
	return 0, false, nil
}
