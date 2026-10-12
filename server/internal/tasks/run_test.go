package tasks_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
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
	// agent overrides what Cancel reports about the agent (stopped, not_running, unknown).
	agent string
	// cancels counts Cancel calls; cancelled lists the session ids they named.
	cancels   int
	cancelled []string
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
	e.svc.Workspaces, e.svc.Sessions, e.svc.Observer = r.ws, r.ses, r.obs
	e.svc.Limits = tasks.Limits{
		MaxChildrenPerParent: 3, MaxChildrenGlobal: 6, ProvisionWindow: 30 * time.Second,
		Interval: 10 * time.Millisecond, ProjectionEvery: time.Second, SettleIdle: time.Nanosecond,
		ResolveBackoff: reconcile.Backoff{Base: 20 * time.Millisecond, Max: 20 * time.Millisecond},
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
	deadline := time.Now().Add(20 * time.Second)
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

func (r *rig) attemptState(id string) string {
	return r.str(`SELECT a.state FROM task_attempts a WHERE a.task_id = $1 ORDER BY a.number DESC LIMIT 1`, id)
}

func (r *rig) opState(id string) string {
	return r.str(`SELECT state FROM task_operations WHERE task_id = $1 AND kind = 'create'`, id)
}

// settleTurns marks every open delivery of the ledger completed, as a finished agent turn would.
func (r *rig) finishTurns() {
	r.t.Helper()
	r.exec(`UPDATE deliveries SET state = 'completed' WHERE state IN ('recorded', 'sending', 'delivered')`)
}

// attemptOf returns the current attempt id and binding of a task.
func (r *rig) attemptOf(task string) (attempt, binding string) {
	r.t.Helper()
	attempt = r.str(`SELECT current_attempt_id FROM tasks WHERE id = $1`, task)
	binding = r.str(`SELECT binding_id FROM task_attempts WHERE id = $1`, attempt)
	return attempt, binding
}
