package app_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/app"
	"github.com/oldsj/mainloop/server/internal/app/apicheck"
	"github.com/oldsj/mainloop/server/internal/app/apptest"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	"github.com/oldsj/mainloop/server/internal/providers"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/sessions"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

// checkpointsServer builds the HTTP handler over a real kagenttest fake and a running delivery
// loop, so a created session's prompt actually lands on a native Session and completes: the
// precondition for a checkpoint. It returns the handler and the service, so a test can wait for
// its own session's turn through the HTTP conversation route.
func checkpointsServer(t *testing.T) (http.Handler, *sessions.Service) {
	t.Helper()
	cfg, err := config.Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": "mainloop.test", "MAINLOOP_API_HOSTS": "api.test"})
	if err != nil {
		t.Fatal(err)
	}
	srv := kagenttest.New()
	t.Cleanup(srv.Close)
	reg, err := providers.NewRegistry(cfg.Kagent)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range reg.Profiles() {
		for _, a := range p.Agents {
			srv.AddAgent(a.Namespace, a.Name)
		}
	}
	k, err := kagent.New(kagent.Config{GatewayURL: srv.URL, UserID: "mainloop"})
	if err != nil {
		t.Fatal(err)
	}
	pool := testdb.New(t).Pool
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	d := apptest.Deps(t, cfg, log, pool, apptest.WithKagent(k))
	d.DB = up()

	r := reconcile.NewRunner(pool, log, nil)
	l := d.Sessions.DeliveryLoop()
	l.Interval = 20 * time.Millisecond
	r.Add(l)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { _ = r.Run(ctx) })
	t.Cleanup(func() { cancel(); wg.Wait() })

	h, err := app.New(d).Handler()
	if err != nil {
		t.Fatal(err)
	}
	return apicheck.Wrap(t, h), d.Sessions
}

// waitCompletedConversation polls the session's conversation until its prompt has a reply, the
// same signal TestChatAndMainThreadOverHTTP uses, over HTTP rather than a direct DB read.
func waitCompletedConversation(t *testing.T, h http.Handler, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		r := send(h, "GET", "/sessions/"+sessionID+"/conversation", "", nil)
		if r.status != 200 {
			t.Fatalf("conversation: %d %s", r.status, r.body)
		}
		var conv api.SessionConversationResponse
		strict(t, r.body, &conv)
		if len(conv.Messages) >= 2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("session %s never completed its first turn", sessionID)
}

// TestCheckpointAndForkOverHTTP exercises the whole checkpoint and fork surface through the real
// HTTP routes (so wire shapes are validated against api/openapi.yaml by apicheck.Wrap, not just
// the Go-level calls internal/sessions' own tests make): create a session, let its prompt
// complete, checkpoint it, read it back by both GET routes, fork it, and delete it.
func TestCheckpointAndForkOverHTTP(t *testing.T) {
	t.Parallel()
	h, _ := checkpointsServer(t)

	r := send(h, "POST", "/sessions", `{"title":"Fix the thing","description":"d","prompt":"hello"}`, nil)
	if r.status != 200 {
		t.Fatalf("create session: %d %s", r.status, r.body)
	}
	var created api.Session
	strict(t, r.body, &created)
	waitCompletedConversation(t, h, created.Id)

	rid := "11111111-1111-1111-1111-111111111111"
	r = send(h, "POST", "/sessions/"+created.Id+"/checkpoints", `{"request_id":"`+rid+`"}`, nil)
	if r.status != 200 {
		t.Fatalf("create checkpoint: %d %s", r.status, r.body)
	}
	var cp api.Checkpoint
	strict(t, r.body, &cp)
	if cp.Id == "" || cp.SessionId == "" || cp.State != api.CheckpointStateReady {
		t.Fatalf("checkpoint: %s", r.body)
	}

	r = send(h, "GET", "/checkpoints/"+cp.Id, "", nil)
	if r.status != 200 {
		t.Fatalf("get checkpoint: %d %s", r.status, r.body)
	}
	var got api.Checkpoint
	strict(t, r.body, &got)
	if got.Id != cp.Id {
		t.Fatalf("get checkpoint: %s", r.body)
	}

	r = send(h, "GET", "/sessions/"+created.Id+"/checkpoints", "", nil)
	if r.status != 200 {
		t.Fatalf("list checkpoints: %d %s", r.status, r.body)
	}
	var list []api.Checkpoint
	strict(t, r.body, &list)
	if len(list) != 1 || list[0].Id != cp.Id {
		t.Fatalf("list checkpoints: %s", r.body)
	}

	forkRid := "22222222-2222-2222-2222-222222222222"
	r = send(h, "POST", "/checkpoints/"+cp.Id+"/fork", `{"request_id":"`+forkRid+`"}`, nil)
	if r.status != 200 {
		t.Fatalf("fork: %d %s", r.status, r.body)
	}
	var fork api.CheckpointForkResponse
	strict(t, r.body, &fork)
	if fork.NewSessionId == "" || fork.AcceptedAt == nil {
		t.Fatalf("fork: %s", r.body)
	}

	r = send(h, "GET", "/sessions/"+fork.NewSessionId, "", nil)
	if r.status != 200 {
		t.Fatalf("get forked session: %d %s", r.status, r.body)
	}
	var forked api.Session
	strict(t, r.body, &forked)
	if forked.ParentSessionId == nil || *forked.ParentSessionId != created.Id {
		t.Fatalf("forked session's parent: %s", r.body)
	}

	r = send(h, "DELETE", "/checkpoints/"+cp.Id, "", nil)
	if r.status != 200 {
		t.Fatalf("delete checkpoint: %d %s", r.status, r.body)
	}
	// Deleting it again is not an error.
	r = send(h, "DELETE", "/checkpoints/"+cp.Id, "", nil)
	if r.status != 200 {
		t.Fatalf("repeat delete checkpoint: %d %s", r.status, r.body)
	}
}
