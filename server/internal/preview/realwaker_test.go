package preview

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	"github.com/oldsj/mainloop/server/internal/projects"
	"github.com/oldsj/mainloop/server/internal/providers"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/workspaces"
)

// noDeliveries is a delivery ledger with no turns.
type noDeliveries struct{}

func (noDeliveries) HasOpen(context.Context, string) (bool, error)           { return false, nil }
func (noDeliveries) LastActivity(context.Context, string) (time.Time, error) { return time.Time{}, nil }

// realWorkspace puts a real workspaces.Service (with its lifecycle loop) behind the proxy of e, with
// a running workspace that declares port 3000. It returns the service, the fake kagent, the
// workspace id and the host to request.
func realWorkspace(t *testing.T, e *env) (svc *workspaces.Service, fake *kagenttest.Server, id, host string) {
	t.Helper()
	cfg, err := config.Parse(map[string]string{
		"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": "mainloop.test", "MAINLOOP_API_HOSTS": "api.test",
		"MAINLOOP_OWNER_ID": owner, "SUBSTRATE_PREVIEW_BASE_URL": origin,
		"SUBSTRATE_ROUTER_ADDRESS":         e.router.addr(),
		"WORKSPACE_OPERATION_WAIT_SECONDS": "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	fake = kagenttest.New()
	t.Cleanup(fake.Close)
	reg, err := providers.NewRegistry(cfg.Kagent)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range reg.Profiles() {
		for _, ref := range p.Agents {
			fake.AddAgent(ref.Namespace, ref.Name)
		}
	}
	client, err := kagent.New(kagent.Config{GatewayURL: fake.URL, UserID: cfg.Kagent.UserID})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc = workspaces.New(cfg, e.pool, client, reg, nil, noDeliveries{}, projects.NewService(cfg, e.pool, nil, nil, nil, log), log)
	svc.PollEvery = 5 * time.Millisecond
	e.proxy.Waker = svc

	r := reconcile.NewRunner(e.pool, svc.Log, nil)
	loop := svc.Loop()
	loop.Interval = 10 * time.Millisecond
	r.Add(loop)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	repo := "oldsj/mainloop"
	w, err := svc.Create(context.Background(), api.CreateWorkspaceRequest{Repo: &repo})
	if err != nil {
		t.Fatal(err)
	}
	if w.ObservedState != api.WorkspaceObservedStateRunning {
		t.Fatalf("not running: %+v", w)
	}
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE workspaces SET ports = '[{"name":"web","number":3000}]'::jsonb WHERE id = $1`, w.WorkspaceId); err != nil {
		t.Fatal(err)
	}
	return svc, fake, w.WorkspaceId, "3000--" + w.WorkspaceId + "--preview." + baseHost
}

// A suspend that is open must not hold the preview request: the proxy answers its waking page at
// once and the browser retries, and nothing is resumed under the suspend.
func TestPreviewDuringAnOpenSuspendShowsTheWakingPageAtOnce(t *testing.T) {
	e := newEnv(t)
	svc, fake, id, host := realWorkspace(t, e)

	fake.FailNext(kagent.ProcSuspendSession, kagenttest.Fault{Delay: 3 * time.Second}) // the suspend stays in flight
	suspended := make(chan error, 1)
	go func() { _, err := svc.Suspend(context.Background(), id); suspended <- err }()
	deadline := time.Now().Add(10 * time.Second)
	for fake.Count(kagent.ProcSuspendSession) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the suspend never reached kagent")
		}
		time.Sleep(5 * time.Millisecond)
	}

	start := time.Now()
	rec := e.do("GET", host, "/", nil, nil)
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the preview request waited %s for the suspend", took)
	}
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("got %d %v", rec.Code, rec.Header())
	}
	if fake.Count(kagent.ProcResumeSession) != 0 {
		t.Fatal("resumed under an open suspend")
	}
	if len(e.router.seen()) != 0 {
		t.Fatal("traffic reached the workspace while it was being suspended")
	}
	<-suspended
}

// A suspended workspace is resumed once by the preview request, which then goes through.
func TestPreviewOfASuspendedWorkspaceResumesItOnce(t *testing.T) {
	e := newEnv(t)
	svc, fake, id, host := realWorkspace(t, e)
	if _, err := svc.Suspend(context.Background(), id); err != nil {
		t.Fatal(err)
	}

	rec := e.do("GET", host, "/", nil, nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "dev" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	if n := fake.Count(kagent.ProcResumeSession); n != 1 {
		t.Fatalf("%d resumes", n)
	}
	// A second request shares the confirmed wake.
	if rec := e.do("GET", host, "/", nil, nil); rec.Code != http.StatusOK {
		t.Fatalf("second request: %d", rec.Code)
	}
	if n := fake.Count(kagent.ProcResumeSession); n != 1 {
		t.Fatalf("%d resumes after two requests", n)
	}
}
