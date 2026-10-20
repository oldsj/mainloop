package tasks_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/sessions"
	"github.com/oldsj/mainloop/server/internal/tasks"
	"github.com/oldsj/mainloop/server/internal/workspaces"
)

// fakeWorkspaces stands in for the workspace service: it writes the rows a created workspace has
// (session, binding with its kagent Session, workspace) and records what was asked of it.
type fakeWorkspaces struct {
	pool *pgxpool.Pool
	mu   sync.Mutex
	// made and removed are the workspace ids created and deleted.
	made, removed []string
	requests      []string
	// crash makes the next creates write their rows and then fail, as a process dying before it
	// could record the answer would.
	crash int
	// reject makes creates fail the way kagent refusing a request does.
	reject string
	// unready leaves new workspaces without a kagent Session.
	unready bool
	// fail makes creates return this error, as a request that can never succeed would.
	fail error
	// failTimes makes the next failTimes creates return a transient error, then succeed, as a
	// capacity hiccup the reconcile loop's own retry resolves would.
	failTimes int
	deleteErr error
	// gate holds the next create until it is closed, then returns gateErr if set, otherwise commits
	// the workspace. entered is told when a create is waiting.
	gate    chan struct{}
	gateErr error
	entered chan struct{}
}

func (f *fakeWorkspaces) Create(ctx context.Context, body api.CreateWorkspaceRequest) (api.WorkspaceLifecycle, error) {
	return f.CreateWithRequestID(ctx, uuid.NewString(), body)
}

func (f *fakeWorkspaces) CreateWithRequestID(ctx context.Context, requestID string, body api.CreateWorkspaceRequest) (api.WorkspaceLifecycle, error) {
	f.mu.Lock()
	f.requests = append(f.requests, requestID)
	gate, gateErr, entered := f.gate, f.gateErr, f.entered
	f.gate = nil
	f.mu.Unlock()
	if gate != nil {
		entered <- struct{}{}
		<-gate
		if gateErr != nil {
			return api.WorkspaceLifecycle{}, gateErr
		}
		// Model a commit that was already in flight when the caller lost its lease or cancelled.
		ctx = context.WithoutCancel(ctx)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	q := dbq.New(f.pool)
	if w, err := q.GetWorkspaceByRequestID(ctx, dbq.GetWorkspaceByRequestIDParams{UserID: owner, RequestID: requestID}); err == nil {
		return api.WorkspaceLifecycle{WorkspaceId: w.ID, SessionId: w.ID}, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return api.WorkspaceLifecycle{}, err
	}
	if f.failTimes > 0 {
		f.failTimes--
		return api.WorkspaceLifecycle{}, errors.New("tasks: transient capacity hiccup")
	}
	if f.fail != nil {
		return api.WorkspaceLifecycle{}, f.fail
	}
	if f.reject != "" {
		return api.WorkspaceLifecycle{}, &workspaces.RejectedError{Detail: f.reject}
	}
	id := uuid.NewString()
	if _, err := q.CreateSession(ctx, dbq.CreateSessionParams{
		ID: id, UserID: owner, ProjectID: body.ProjectId, Title: "ws", RepoURL: ptr("https://example.test/o/p1"), BranchName: body.Branch,
	}); err != nil {
		return api.WorkspaceLifecycle{}, err
	}
	if _, err := q.CreateAgentBinding(ctx, dbq.CreateAgentBindingParams{
		SessionID: id, Kind: *body.AgentKind, Role: dbstate.RoleAgent, MCPGrantKind: dbstate.GrantWorkspace,
	}); err != nil {
		return api.WorkspaceLifecycle{}, err
	}
	if !f.unready {
		if _, err := f.pool.Exec(ctx, `UPDATE agent_bindings SET kagent_session_id = 'ks-' || session_id WHERE session_id = $1`, id); err != nil {
			return api.WorkspaceLifecycle{}, err
		}
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO workspaces (id, user_id, project_id, repo_url, branch, agent_kind, agent_namespace, agent_name, request_id)
	     VALUES ($1, $2, $3, 'https://example.test/o/p1', $4, $5, 'kagent', 'agent', $6)`, id, owner, *body.ProjectId, *body.Branch, *body.AgentKind, requestID); err != nil {
		return api.WorkspaceLifecycle{}, err
	}
	f.made = append(f.made, id)
	if f.crash > 0 {
		f.crash--
		return api.WorkspaceLifecycle{}, errors.New("connection lost")
	}
	return api.WorkspaceLifecycle{WorkspaceId: id, SessionId: id}, nil
}

func (f *fakeWorkspaces) Delete(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	tag, err := f.pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, id)
	if err == nil && tag.RowsAffected() > 0 {
		f.removed = append(f.removed, id)
	}
	return err
}

func (f *fakeWorkspaces) madeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.made)
}

func ptr[T any](v T) *T { return &v }

// fakeSessions is the real sessions service over the test database with no kagent client, so sends
// are recorded in the ledger and never delivered, plus switches for the cancel answer.
type fakeSessions struct {
	*sessions.Service
	mu sync.Mutex
	// agent overrides what Cancel, Complete and Fail report about the agent (stopped, not_running,
	// unknown).
	agent string
	// cancels counts Cancel calls; cancelled lists the session ids they named.
	cancels   int
	cancelled []string
	// completed lists the session ids Complete named.
	completed []string
	// failed lists the session ids Fail named.
	failed []string
}

func (f *fakeSessions) Cancel(ctx context.Context, id string) (sessions.CancelResult, error) {
	res, err := f.Service.Cancel(ctx, id)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels++
	f.cancelled = append(f.cancelled, id)
	if f.agent != "" && err == nil {
		res.Agent = f.agent
	}
	return res, err
}

func (f *fakeSessions) Complete(ctx context.Context, id string) (sessions.CancelResult, error) {
	res, err := f.Service.Complete(ctx, id)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completed = append(f.completed, id)
	if f.agent != "" && err == nil {
		res.Agent = f.agent
	}
	return res, err
}

func (f *fakeSessions) Fail(ctx context.Context, id string) (sessions.CancelResult, error) {
	res, err := f.Service.Fail(ctx, id)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed = append(f.failed, id)
	if f.agent != "" && err == nil {
		res.Agent = f.agent
	}
	return res, err
}

// cancelCountOf counts the Cancel calls made for one session.
func (f *fakeSessions) cancelCountOf(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.cancelled {
		if c == id {
			n++
		}
	}
	return n
}

func (f *fakeSessions) cancelCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancels
}

// completeCountOf counts the Complete calls made for one session.
func (f *fakeSessions) completeCountOf(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.completed {
		if c == id {
			n++
		}
	}
	return n
}

// failCountOf counts the Fail calls made for one session.
func (f *fakeSessions) failCountOf(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.failed {
		if c == id {
			n++
		}
	}
	return n
}

// fakeStops answers the resolver of an unconfirmed stop: it reports the agent as still running
// until released.
type fakeStops struct {
	mu      sync.Mutex
	stopped bool
	calls   int
}

func (f *fakeStops) Stopped(context.Context, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.stopped, nil
}

func (f *fakeStops) set(stopped bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = stopped
}

func (f *fakeStops) lookCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeKagentLiveness is the kagent client for the real KagentLiveness adapter under test:
// ObserveLiveness and ListTasksLatest, both keyed by the kagent session/context id KagentLiveness
// resolves from a binding, never Mainloop's own session id. A context not set answers present and
// not terminal with no tasks, so a test only states what it means to change.
type fakeKagentLiveness struct {
	mu    sync.Mutex
	live  map[string]kagent.SessionLiveness
	tasks map[string][]*a2a.Task
	calls int
	// artifactReads are the states ListTasksInStateLatest was asked for.
	artifactReads []a2a.TaskState
	// onObserve runs inside ObserveLiveness, before it answers, so a test can race a concurrent
	// write against the write the sweep makes afterward.
	onObserve func()
}

func (k *fakeKagentLiveness) ObserveLiveness(_ context.Context, id string) (kagent.SessionLiveness, error) {
	k.mu.Lock()
	hook := k.onObserve
	k.mu.Unlock()
	if hook != nil {
		hook()
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.calls++
	if live, ok := k.live[id]; ok {
		return live, nil
	}
	return kagent.SessionLiveness{Exists: true}, nil
}

func (k *fakeKagentLiveness) artifactStatesRead() []a2a.TaskState {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.artifactReads)
}

func (k *fakeKagentLiveness) resetArtifactReads() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.artifactReads = nil
}

// ListTasksLatest answers like kagent's plain list: every task, none with its artifacts.
func (k *fakeKagentLiveness) ListTasksLatest(_ context.Context, _ kagent.AgentRef, contextID string) ([]*a2a.Task, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []*a2a.Task
	for _, t := range k.tasks[contextID] {
		cp := *t
		cp.Artifacts = nil
		out = append(out, &cp)
	}
	return out, nil
}

// ListTasksInStateLatest answers like kagent's list of one state: those tasks, with artifacts. The
// states it was asked for are kept, so a test can see that finished tasks' artifacts were never read.
func (k *fakeKagentLiveness) ListTasksInStateLatest(_ context.Context, _ kagent.AgentRef, contextID string, state a2a.TaskState) ([]*a2a.Task, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.artifactReads = append(k.artifactReads, state)
	var out []*a2a.Task
	for _, t := range k.tasks[contextID] {
		if t.Status.State == state {
			out = append(out, t)
		}
	}
	return out, nil
}

func (k *fakeKagentLiveness) set(kagentSessionID string, live kagent.SessionLiveness) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.live == nil {
		k.live = map[string]kagent.SessionLiveness{}
	}
	k.live[kagentSessionID] = live
}

func (k *fakeKagentLiveness) setTasks(contextID string, listed []*a2a.Task) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.tasks == nil {
		k.tasks = map[string][]*a2a.Task{}
	}
	k.tasks[contextID] = listed
}

func (k *fakeKagentLiveness) lookCount() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.calls
}

// fakeActivity is the sessions delivery ledger KagentLiveness falls back to when kagent shows no
// A2A task at all. A session not set answers the zero time.
type fakeActivity struct {
	mu sync.Mutex
	at map[string]time.Time
}

func (f *fakeActivity) LastActivity(_ context.Context, sessionID string) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.at[sessionID], nil
}

func (f *fakeActivity) set(sessionID string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.at == nil {
		f.at = map[string]time.Time{}
	}
	f.at[sessionID] = at
}

// fakeObserver answers the projection sweep.
type fakeObserver struct {
	mu    sync.Mutex
	obs   tasks.Observation
	err   error
	calls int
	// expected is the PR number each call was asked for.
	expected []int
	// branch, when set, models the branch's PRs as GitHub shows them: a positive expected PR picks
	// only that one (none when it is absent), and zero picks the open PR, else the highest number.
	branch []tasks.Observation
	// onObserve runs inside Observe, before it answers.
	onObserve func()
}

func (o *fakeObserver) Observe(_ context.Context, _, _ string, expected int) (tasks.Observation, error) {
	o.mu.Lock()
	hook := o.onObserve
	o.mu.Unlock()
	if hook != nil {
		hook()
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls++
	o.expected = append(o.expected, expected)
	if o.branch != nil {
		return o.pick(expected), o.err
	}
	return o.obs, o.err
}

// pick is the branch observation for the expected PR; see fakeObserver.branch.
func (o *fakeObserver) pick(expected int) tasks.Observation {
	var chosen tasks.Observation
	for _, p := range o.branch {
		switch {
		case expected > 0:
			if p.PRNumber == expected {
				chosen = p
			}
		case chosen.PRNumber == 0 || (p.PRState == "open" && chosen.PRState != "open") ||
			((p.PRState == "open") == (chosen.PRState == "open") && p.PRNumber > chosen.PRNumber):
			chosen = p
		}
	}
	if chosen.PRNumber == 0 {
		return tasks.Observation{ObservedAt: time.Now()}
	}
	return chosen
}

func (o *fakeObserver) set(obs tasks.Observation, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.obs, o.err = obs, err
}

// rig is an env with the runtimes attached.
type rig struct {
	*env
	ws  *fakeWorkspaces
	ses *fakeSessions
	obs *fakeObserver
	// stops is installed as the StopObserver by the tests that resolve unconfirmed stops.
	stops *fakeStops
	// live is the fake kagent client of the KagentLiveness adapter installed as svc.Liveness.
	live *fakeKagentLiveness
	// activity is the fake delivery ledger that adapter falls back to.
	activity *fakeActivity
}

func newRig(t *testing.T) *rig {
	t.Helper()
	e := newEnv(t)
	r := &rig{env: e}
	r.ws = &fakeWorkspaces{pool: e.pool}
	r.ses = &fakeSessions{Service: sessions.NewService(mustConfig(t), e.pool, nil, nil, nil, nil, nil, nil, slog.Default())}
	r.ses.OwnerID = owner
	r.obs = &fakeObserver{}
	r.stops = &fakeStops{}
	r.live = &fakeKagentLiveness{}
	r.activity = &fakeActivity{}
	e.svc.Workspaces, e.svc.Sessions, e.svc.Observer = r.ws, r.ses, r.obs
	e.svc.Liveness = &tasks.KagentLiveness{Pool: e.pool, OwnerID: owner, Sessions: r.ses.Service, Kagent: r.live, Activity: r.activity}
	e.svc.Limits = tasks.Limits{
		MaxChildrenPerParent: 3, MaxChildrenGlobal: 6, ProvisionWindow: 30 * time.Second,
		Interval: 10 * time.Millisecond, ProjectionEvery: time.Second, SettleIdle: time.Nanosecond,
		ResolveBackoff: reconcile.Backoff{Base: 20 * time.Millisecond, Max: 20 * time.Millisecond},
		StallTimeout:   45 * time.Minute, MaxRuntime: 6 * time.Hour,
	}
	e.svc.Log = slog.Default()
	return r
}

// run starts a runner with the operation and delivery loops and stops it with the returned func.
// Each call is a new runner with its own lease owner, so a second call models a restarted process.
func (r *rig) run() (stop func()) {
	r.t.Helper()
	rn := reconcile.NewRunner(r.pool, slog.Default(), nil)
	for _, l := range []reconcile.Loop{r.svc.OpLoop(), r.svc.DeliveryLoop()} {
		l.Interval = 10 * time.Millisecond
		// A short lease, so a runner stopped between claim and handler does not leave a row leased
		// past the wait of the test that restarts it.
		l.Lease = time.Second
		l.Backoff = reconcile.Backoff{Base: 10 * time.Millisecond, Max: 50 * time.Millisecond}
		rn.Add(l)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = rn.Run(ctx) }()
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); <-done }) }
	r.t.Cleanup(stop)
	return stop
}

// sweep runs one sweep once.
func (r *rig) sweep(s reconcile.Sweep) {
	r.t.Helper()
	rn := reconcile.NewRunner(r.pool, slog.Default(), nil)
	if _, err := rn.RunSweep(context.Background(), s); err != nil {
		r.t.Fatal(err)
	}
}

// eventually polls until cond holds.
func (r *rig) eventually(what string, cond func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.t.Fatalf("timed out waiting for %s", what)
}

func (r *rig) str(sql string, args ...any) string {
	r.t.Helper()
	var s *string
	if err := r.pool.QueryRow(context.Background(), sql, args...).Scan(&s); err != nil {
		r.t.Fatal(err)
	}
	if s == nil {
		return ""
	}
	return *s
}

func (r *rig) taskStatus(id string) string {
	return r.str(`SELECT status FROM tasks WHERE id = $1`, id)
}

func (r *rig) taskReason(id string) string {
	return r.str(`SELECT reason FROM tasks WHERE id = $1`, id)
}

func (r *rig) attemptState(id string) string {
	return r.str(`SELECT a.state FROM task_attempts a WHERE a.task_id = $1 ORDER BY a.number DESC LIMIT 1`, id)
}

func (r *rig) opState(id string) string {
	return r.str(`SELECT state FROM task_operations WHERE task_id = $1 AND kind = 'create'`, id)
}

// settleTurns marks every open delivery of the ledger completed, as a finished agent turn would,
// then admits the oldest message still queued behind each session's turn, as the sessions ledger's
// own reconciler does in the same transaction that ends a turn (deliver.go's settle, control.go's
// endDelivery). These raw-SQL helpers bypass that Go path, so it is replayed here.
func (r *rig) finishTurns() {
	r.t.Helper()
	r.exec(`UPDATE deliveries SET state = 'completed' WHERE state IN ('recorded', 'sending', 'delivered')`)
	r.promoteQueued()
}

// promoteQueued admits the oldest queued delivery of every session that currently has no open one.
func (r *rig) promoteQueued() {
	r.t.Helper()
	r.exec(`
		UPDATE deliveries d SET state = 'recorded', updated_at = now()
		WHERE d.id IN (
			SELECT DISTINCT ON (q.session_id) q.id FROM deliveries q
			WHERE q.state = 'queued'
			  AND NOT EXISTS (
			      SELECT 1 FROM deliveries o
			      WHERE o.session_id = q.session_id AND o.state IN ('recorded', 'sending', 'delivered')
			  )
			ORDER BY q.session_id, q.created_at, q.id
		)`)
}

// attemptOf returns the current attempt id and binding of a task.
func (r *rig) attemptOf(task string) (attempt, binding string) {
	r.t.Helper()
	attempt = r.str(`SELECT current_attempt_id FROM tasks WHERE id = $1`, task)
	binding = r.str(`SELECT binding_id FROM task_attempts WHERE id = $1`, attempt)
	return attempt, binding
}
