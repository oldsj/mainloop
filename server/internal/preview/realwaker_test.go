package preview

import (
	"context"
	"io"
	"log/slog"
	"maps"
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
	vars := map[string]string{
		"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": "mainloop.test", "MAINLOOP_API_HOSTS": "api.test",
		"MAINLOOP_OWNER_ID": owner, "SUBSTRATE_PREVIEW_BASE_URL": origin,
		"WORKSPACE_OPERATION_WAIT_SECONDS": "1",
	}
	maps.Copy(vars, e.router.vars())
	cfg, err := config.Parse(vars)
	if err != nil {
		t.Fatal(err)
	}
	fake = kagenttest.New()
	t.Cleanup(fake.Close)
	fake.Actors = true
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
	e.proxy.Sessions = client

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

// kagent parks an idle workspace's Actor itself and has no resume call. A preview of a sleeping
// workspace is tunnelled to its Actor at once: the connection through the router is what wakes it,
// and no workspace operation is started.
func TestPreviewOfASleepingWorkspaceTunnelsToItsActor(t *testing.T) {
	e := newEnv(t)
	svc, fake, id, host := realWorkspace(t, e)
	var ksid string
	if err := e.pool.QueryRow(context.Background(), `SELECT kagent_session_id FROM agent_bindings WHERE session_id = $1`, id).Scan(&ksid); err != nil {
		t.Fatal(err)
	}
	fake.Sleep(ksid)
	if w, err := svc.Get(context.Background(), id); err != nil || w.ObservedState != api.WorkspaceObservedStateSuspended {
		t.Fatalf("a sleeping workspace shows as %+v %v", w.ObservedState, err)
	}

	rec := e.do("GET", host, "/", nil, nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "dev" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	want := "ate/" + kagent.ActorName(ksid) + " actor-upstream:3000 CONNECT"
	if seen := e.router.seen(); len(seen) != 1 || seen[0] != want {
		t.Fatalf("router saw %v, want [%s]", seen, want)
	}
	var ops int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM workspace_ops WHERE workspace_id = $1 AND kind <> 'create'`, id).Scan(&ops); err != nil || ops != 0 {
		t.Fatalf("%d workspace operations for a preview: %v", ops, err)
	}
}
