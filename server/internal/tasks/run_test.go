package tasks_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
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
	// crash makes the next creates write their rows and then fail, as a process dying before it
	// could record the answer would.
	crash int
	// reject makes creates fail the way kagent refusing a request does.
	reject string
	// unready leaves new workspaces without a kagent Session.
	unready bool
}

func (f *fakeWorkspaces) Create(ctx context.Context, body api.CreateWorkspaceRequest) (api.WorkspaceLifecycle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reject != "" {
		return api.WorkspaceLifecycle{}, &workspaces.RejectedError{Detail: f.reject}
	}
	id := uuid.NewString()
	q := dbq.New(f.pool)
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
	     VALUES ($1, $2, $3, 'https://example.test/o/p1', $4, $5, 'kagent', 'agent', $1)`, id, owner, *body.ProjectId, *body.Branch, *body.AgentKind); err != nil {
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
	f.removed = append(f.removed, id)
	_, err := f.pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, id)
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
	// cancels counts Cancel calls.
	cancels int
}

func (f *fakeSessions) Cancel(ctx context.Context, id string) (sessions.CancelResult, error) {
	res, err := f.Service.Cancel(ctx, id)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels++
	if f.agent != "" && err == nil {
		res.Agent = f.agent
	}
	return res, err
}

func (f *fakeSessions) cancelCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cancels
}

// fakeObserver answers the projection sweep.
type fakeObserver struct {
	mu    sync.Mutex
	obs   tasks.Observation
	err   error
	calls int
	// onObserve runs inside Observe, before it answers.
	onObserve func()
}

func (o *fakeObserver) Observe(context.Context, string, string) (tasks.Observation, error) {
	o.mu.Lock()
	hook := o.onObserve
	o.mu.Unlock()
	if hook != nil {
		hook()
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls++
	return o.obs, o.err
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
}

func newRig(t *testing.T) *rig {
	t.Helper()
	e := newEnv(t)
	r := &rig{env: e}
	r.ws = &fakeWorkspaces{pool: e.pool}
	r.ses = &fakeSessions{Service: sessions.NewService(mustConfig(t), e.pool, nil, slog.Default())}
	r.ses.OwnerID = owner
	r.obs = &fakeObserver{}
	e.svc.Workspaces, e.svc.Sessions, e.svc.Observer = r.ws, r.ses, r.obs
	e.svc.Limits = tasks.Limits{
		MaxChildrenPerParent: 3, MaxChildrenGlobal: 6, ProvisionWindow: 30 * time.Second,
		Interval: 10 * time.Millisecond, ProjectionEvery: time.Second, SettleIdle: time.Nanosecond,
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
