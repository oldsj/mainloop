package workspaces

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	"github.com/oldsj/mainloop/server/internal/providers"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/testdb"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

const owner = "owner-1"

// deliveries is a scriptable delivery ledger.
type deliveries struct {
	mu     sync.Mutex
	open   map[string]bool
	active map[string]time.Time
}

func (d *deliveries) HasOpen(_ context.Context, id string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.open[id], nil
}

func (d *deliveries) LastActivity(_ context.Context, id string) (time.Time, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.active[id], nil
}

func (d *deliveries) setOpen(id string, v bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.open[id] = v
}

type harness struct {
	t      *testing.T
	svc    *Service
	fake   *kagenttest.Server
	client *kagent.Client
	cfg    *config.Config
	dq     *dbq.Queries
	dl     *deliveries
	// deleteState is what a kagent delete reports; real kagent says DELETED.
	deleteState atomic.Int32
	// getState, when non-zero, is the state GetSession reports.
	getState atomic.Int32
}

func newHarness(t *testing.T, extra map[string]string) *harness {
	t.Helper()
	env := map[string]string{
		"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": "mainloop.test", "MAINLOOP_API_HOSTS": "api.test",
		"MAINLOOP_OWNER_ID":                      owner,
		"WORKSPACE_CREATE_RETRY_INITIAL_SECONDS": "0.05",
		"WORKSPACE_CREATE_RETRY_MAX_SECONDS":     "0.2",
		"WORKSPACE_OPERATION_WAIT_SECONDS":       "1",
		"SUBSTRATE_PREVIEW_BASE_URL":             "http://localhost:8001",
	}
	for k, v := range extra {
		env[k] = v
	}
	cfg, err := config.Parse(env)
	if err != nil {
		t.Fatal(err)
	}
	fake := kagenttest.New()
	t.Cleanup(fake.Close)
	h := &harness{t: t, fake: fake, cfg: cfg, dl: &deliveries{open: map[string]bool{}, active: map[string]time.Time{}}}
	h.deleteState.Store(int32(kagentapi.RuntimeState_RUNTIME_STATE_DELETED))
	fake.TamperSession = func(proc string, s *kagentapi.Session) {
		switch proc {
		case kagent.ProcDeleteSession:
			s.State = kagentapi.RuntimeState(h.deleteState.Load())
		case kagent.ProcGetSession:
			if v := h.getState.Load(); v != 0 {
				s.State = kagentapi.RuntimeState(v)
			}
		}
	}
	for _, p := range mustRegistry(t, cfg).Profiles() {
		for _, ref := range p.Agents {
			fake.AddAgent(ref.Namespace, ref.Name)
		}
	}
	client, err := kagent.New(kagent.Config{GatewayURL: fake.URL, UserID: cfg.Kagent.UserID})
	if err != nil {
		t.Fatal(err)
	}
	h.client = client
	pool := testdb.New(t).Pool
	h.dq = dbq.New(pool)
	h.svc = &Service{
		Pool: pool, Kagent: client, Providers: mustRegistry(t, cfg), Config: cfg,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), OwnerID: owner, Deliveries: h.dl,
		PollEvery: 5 * time.Millisecond,
	}
	return h
}

func mustRegistry(t *testing.T, cfg *config.Config) *providers.Registry {
	t.Helper()
	r, err := providers.NewRegistry(cfg.Kagent)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// run starts a runner for the lifecycle loop and stops it with the returned func. Each call is a
// new runner with its own lease owner, so a second call models a restarted process.
func (h *harness) run() (stop func()) {
	h.t.Helper()
	r := reconcile.NewRunner(h.svc.Pool, h.svc.Log, nil)
	loop := h.svc.Loop()
	loop.Interval = 10 * time.Millisecond
	r.Add(loop)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); <-done }) }
	h.t.Cleanup(stop)
	return stop
}

func ptr[T any](v T) *T { return &v }

// create makes a workspace through the service.
func (h *harness) create(body api.CreateWorkspaceRequest) (api.WorkspaceLifecycle, error) {
	h.t.Helper()
	if body.Repo == nil && body.ProjectId == nil {
		body.Repo = ptr("oldsj/mainloop")
	}
	return h.svc.Create(context.Background(), body)
}

// running creates a workspace with a runner and returns it once kagent has the Session.
func (h *harness) running() api.WorkspaceLifecycle {
	h.t.Helper()
	stop := h.run()
	defer stop()
	w, err := h.create(api.CreateWorkspaceRequest{})
	if err != nil {
		h.t.Fatal(err)
	}
	if w.ObservedState != api.WorkspaceObservedStateRunning {
		h.t.Fatalf("not running: %+v", w)
	}
	return w
}

func (h *harness) row(id string) wsRow {
	h.t.Helper()
	r, err := h.dq.GetWorkspaceForOp(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return wsRow(r)
}

func (h *harness) sessions() []*kagentapi.Session {
	h.t.Helper()
	s, err := h.client.ListSessions(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

func (h *harness) workspaceCount() int {
	h.t.Helper()
	var n int
	if err := h.svc.Pool.QueryRow(context.Background(), `SELECT count(*) FROM workspaces`).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h *harness) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.svc.Pool.Exec(context.Background(), sql, args...); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) events() int {
	h.t.Helper()
	var n int
	if err := h.svc.Pool.QueryRow(context.Background(), `SELECT count(*) FROM events WHERE type = 'workspace:updated'`).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h *harness) op(id, kind string) dbq.WorkspaceOp {
	h.t.Helper()
	var op dbq.WorkspaceOp
	err := h.svc.Pool.QueryRow(context.Background(),
		`SELECT id, workspace_id, kind, idempotency_key, state, attempts, next_attempt_at, lease_owner, lease_until,
		        failures, first_failed_at, last_error, quiet_since, created_at
		 FROM workspace_ops WHERE workspace_id = $1 AND kind = $2 ORDER BY created_at DESC, id DESC LIMIT 1`, id, kind).
		Scan(&op.ID, &op.WorkspaceID, &op.Kind, &op.IdempotencyKey, &op.State, &op.Attempts, &op.NextAttemptAt,
			&op.LeaseOwner, &op.LeaseUntil, &op.Failures, &op.FirstFailedAt, &op.LastError, &op.QuietSince, &op.CreatedAt)
	if err != nil {
		h.t.Fatal(err)
	}
	return op
}

// eventually polls cond until it holds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func newID() string { return uuid.NewString() }
