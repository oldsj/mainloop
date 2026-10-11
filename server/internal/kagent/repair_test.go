package kagent_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	api "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// ---- redirects -------------------------------------------------------------------------------

// leakCheck is a second host that records every request it gets.
func leakCheck(t *testing.T) (*httptest.Server, *atomic.Int32, *atomic.Value) {
	t.Helper()
	var hits atomic.Int32
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		auth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits, &auth
}

func bearerClient(t *testing.T, url string) *kagent.Client {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	writeToken(t, path, "secret-xyz", time.Now())
	clk := newClock()
	c, err := kagent.New(kagent.Config{GatewayURL: url, ControlTokenFile: path, Sleep: clk.Sleep, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRedirectsAreNotFollowedAndNeverCarryTheToken(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			other, hits, auth := leakCheck(t)
			gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, other.URL+r.URL.Path, status)
			}))
			defer gw.Close()
			c := bearerClient(t, gw.URL)

			_, err := c.GetAgent(ctx, agent)
			if err == nil {
				t.Fatal("GetAgent followed a redirect")
			}
			mustBe[*kagent.OutcomeUnknown](t, err) // a 3xx says nothing about whether the call ran

			_, err = c.GetTask(ctx, agent, "t")
			mustBe[*kagent.ProtocolError](t, err)

			_, err = send(c, "m1")
			mustBe[*kagent.ProtocolError](t, err)

			if n := hits.Load(); n != 0 {
				t.Fatalf("the other host got %d requests, last Authorization %q", n, auth.Load())
			}
		})
	}
}

func TestSameHostRedirectsAreNotFollowedEither(t *testing.T) {
	var hits atomic.Int32
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			http.Redirect(w, r, r.URL.Path+"/again", http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer gw.Close()
	c := bearerClient(t, gw.URL)
	if _, err := send(c, "m1"); err == nil {
		t.Fatal("send followed a redirect")
	}
	if hits.Load() != 1 {
		t.Fatalf("%d requests, want 1", hits.Load())
	}
}

// ---- single-use sequences --------------------------------------------------------------------

func TestSendSequenceIsSingleUse(t *testing.T) {
	c, srv, _ := setup(t)
	seq := c.SendMessage(ctx, agent, kagent.SendParams{Text: "t", MessageID: "m", ContextID: "c"})
	n := 0
	for _, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 3 {
		t.Fatalf("%d events", n)
	}
	var second error
	for _, err := range seq {
		second = err
	}
	if second == nil {
		t.Fatal("second range did not report an error")
	}
	if srv.Count("SendStreamingMessage") != 1 {
		t.Fatalf("sent %d times", srv.Count("SendStreamingMessage"))
	}
}

func TestSubscribeSequenceIsSingleUse(t *testing.T) {
	c, srv, _ := setup(t)
	if _, err := send(c, "m1"); err != nil {
		t.Fatal(err)
	}
	seq := c.SubscribeToTask(ctx, agent, "task-1")
	for _, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
	}
	var second error
	for _, err := range seq {
		second = err
	}
	if second == nil || srv.Count("SubscribeToTask") != 1 {
		t.Fatalf("second %v, subscribed %d times", second, srv.Count("SubscribeToTask"))
	}
}

// ---- identity echo ---------------------------------------------------------------------------

func TestRepliesForAnotherSessionAreOutcomeUnknown(t *testing.T) {
	calls := map[string]func(c *kagent.Client, id string) error{
		kagent.ProcGetSession:     func(c *kagent.Client, id string) error { _, err := c.GetSession(ctx, id); return err },
		kagent.ProcSuspendSession: func(c *kagent.Client, id string) error { _, err := c.SuspendSession(ctx, id); return err },
		kagent.ProcResumeSession:  func(c *kagent.Client, id string) error { _, err := c.ResumeSession(ctx, id); return err },
		kagent.ProcDeleteSession:  func(c *kagent.Client, id string) error { _, err := c.DeleteSession(ctx, id); return err },
	}
	tampers := map[string]func(*api.Session){
		"other id":         func(s *api.Session) { s.Id = "someone-else" },
		"other context id": func(s *api.Session) { s.ContextId = "someone-else" },
		"no session":       nil,
	}
	for proc, call := range calls {
		for name, tamper := range tampers {
			t.Run(proc+"/"+name, func(t *testing.T) {
				c, srv, _ := setup(t)
				created, err := c.CreateSession(ctx, &api.CreateSessionRequest{RequestId: "r"})
				if err != nil {
					t.Fatal(err)
				}
				if err := call(c, created.GetId()); err != nil {
					t.Fatalf("honest reply failed: %v", err)
				}
				// Delete removed it; make a fresh one for the tampered call.
				created, err = c.CreateSession(ctx, &api.CreateSessionRequest{RequestId: "r2"})
				if err != nil {
					t.Fatal(err)
				}
				srv.TamperSession = func(p string, s *api.Session) {
					if p != proc {
						return
					}
					if tamper == nil {
						*s = api.Session{}
						return
					}
					tamper(s)
				}
				mustBe[*kagent.OutcomeUnknown](t, call(c, created.GetId()))
			})
		}
	}
}

func TestPrepareReceiptMustBelongToTheRequest(t *testing.T) {
	c, srv, _ := setup(t)
	created, err := c.CreateSession(ctx, &api.CreateSessionRequest{RequestId: "r"})
	if err != nil {
		t.Fatal(err)
	}
	req := func() *api.PrepareSessionWorkspaceRequest {
		return &api.PrepareSessionWorkspaceRequest{SessionId: created.GetId(), ActionId: "action-1"}
	}
	receipt, err := c.PrepareSessionWorkspace(ctx, req())
	if err != nil || receipt.GetOriginal().GetActionId() != "action-1" || receipt.GetContextId() != created.GetId() {
		t.Fatalf("honest reply: %v, %v", receipt, err)
	}
	cases := map[string]func(*api.WorkspacePreparationReceipt){
		"other action":  func(r *api.WorkspacePreparationReceipt) { r.Original.ActionId = "action-2" },
		"other session": func(r *api.WorkspacePreparationReceipt) { r.ContextId = "someone-else" },
		"no original":   func(r *api.WorkspacePreparationReceipt) { r.Original = nil },
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			srv.TamperReceipt = tamper
			_, err := c.PrepareSessionWorkspace(ctx, req())
			mustBe[*kagent.OutcomeUnknown](t, err)
		})
	}
	t.Run("no receipt", func(t *testing.T) {
		srv.TamperReceipt = func(r *api.WorkspacePreparationReceipt) { r.Reset() }
		_, err := c.PrepareSessionWorkspace(ctx, req())
		mustBe[*kagent.OutcomeUnknown](t, err)
	})
}

func TestGetAgentMustBeTheAgentAskedFor(t *testing.T) {
	c, srv, _ := setup(t)
	srv.AddAgent("kagent", "mainloop-main")
	for name, tamper := range map[string]func(*api.Agent){
		"other name":      func(a *api.Agent) { a.Ref.Name = "other" },
		"other namespace": func(a *api.Agent) { a.Ref.Namespace = "other" },
		"no ref":          func(a *api.Agent) { a.Ref = nil },
	} {
		t.Run(name, func(t *testing.T) {
			srv.TamperAgent = tamper
			_, err := c.GetAgent(ctx, agent)
			mustBe[*kagent.SessionError](t, err)
			notType[*kagent.OutcomeUnknown](t, err)
		})
	}
}

func TestCreateEchoIgnoresFieldsTheClientDoesNotKnow(t *testing.T) {
	c, srv, _ := setup(t)
	req := &api.CreateSessionRequest{
		RequestId: "r", Agent: &api.ResourceReference{Namespace: "kagent", Name: "a"},
		Workspace:              &api.Workspace{Repo: "https://example.test/o/r", Ref: "main", Branch: "b", Depth: 1},
		DevelopmentEnvironment: &api.DevelopmentEnvironment{Image: "img", Platform: "linux/amd64", PolicyIdentity: "p"},
	}
	srv.TamperSession = func(_ string, s *api.Session) {
		s.Workspace.ProtoReflect().SetUnknown([]byte{0xf8, 0x01, 0x01}) // field 31, varint 1
	}
	if _, err := c.CreateSession(ctx, req); err != nil {
		t.Fatalf("an unknown field in the echo was rejected: %v", err)
	}
	for name, tamper := range map[string]func(*api.Session){
		"repo":     func(s *api.Session) { s.Workspace.Repo = "https://example.test/o/other" },
		"depth":    func(s *api.Session) { s.Workspace.Depth = 2 },
		"image":    func(s *api.Session) { s.DevelopmentEnvironment.Image = "other" },
		"policy":   func(s *api.Session) { s.DevelopmentEnvironment.PolicyIdentity = "other" },
		"agent":    func(s *api.Session) { s.Agent.Name = "other" },
		"platform": func(s *api.Session) { s.DevelopmentEnvironment.Platform = "linux/arm64" },
	} {
		t.Run(name, func(t *testing.T) {
			srv.TamperSession = func(_ string, s *api.Session) { tamper(s) }
			_, err := c.CreateSession(ctx, req)
			mustBe[*kagent.OutcomeUnknown](t, err)
		})
	}
}

func TestFakeRejectsARetriedRequestIDWithAnotherContract(t *testing.T) {
	c, _, _ := setup(t)
	mk := func(repo string) *api.CreateSessionRequest {
		return &api.CreateSessionRequest{RequestId: "r", Workspace: &api.Workspace{Repo: repo}}
	}
	if _, err := c.CreateSession(ctx, mk("https://example.test/a")); err != nil {
		t.Fatal(err)
	}
	_, err := c.CreateSession(ctx, mk("https://example.test/b"))
	se := asType[*kagent.SessionError](t, err)
	if se.Code != connect.CodeAlreadyExists {
		t.Fatalf("code %v", se.Code)
	}
}

// ---- refusal on the wire ---------------------------------------------------------------------

// kagent answers 200 text/event-stream and reports a refusal as the first SSE frame.
func TestRefusalAsTheFirstSSEFrameIsRetried(t *testing.T) {
	c, srv, clk := setup(t)
	srv.Script(kagenttest.Fail{Err: kagenttest.NotAccepted(10)})
	srv.Script(kagenttest.Fail{Err: kagenttest.NotAccepted(10)})
	events, err := send(c, "msg-1")
	if err != nil || len(events) != 3 {
		t.Fatalf("%d events, err %v", len(events), err)
	}
	if srv.Count("SendStreamingMessage") != 3 || len(clk.sleeps) != 2 {
		t.Fatalf("sent %d times, slept %v", srv.Count("SendStreamingMessage"), clk.sleeps)
	}
	for _, r := range srv.Requests() {
		if r.MessageID != "msg-1" {
			t.Fatalf("message id %q", r.MessageID)
		}
	}
}

func TestRefusalAsTheFirstSSEFrameStopsAtTheBudget(t *testing.T) {
	c, srv, _ := setup(t, func(cfg *kagent.Config) { cfg.SendRetryBudget = 250 * time.Millisecond })
	for range 5 {
		srv.Script(kagenttest.Fail{Err: kagenttest.NotAccepted(10)})
	}
	events, err := send(c, "m")
	mustBe[*kagent.SendNotAccepted](t, err)
	if len(events) != 0 || srv.Count("SendStreamingMessage") != 2 {
		t.Fatalf("%d events, sent %d times", len(events), srv.Count("SendStreamingMessage"))
	}
}

func TestBackoffDoesNotOverflowWithAHugeBudget(t *testing.T) {
	c, srv, clk := setup(t, func(cfg *kagent.Config) { cfg.SendRetryBudget = 24 * time.Hour })
	for range 60 {
		srv.Script(kagenttest.Fail{Err: kagenttest.NotAccepted(1)})
	}
	if _, err := send(c, "m"); err != nil {
		t.Fatal(err)
	}
	if len(clk.sleeps) != 60 {
		t.Fatalf("%d sleeps", len(clk.sleeps))
	}
	for i, d := range clk.sleeps {
		if d < 100*time.Millisecond || d > 2*time.Second {
			t.Fatalf("sleep %d is %v", i, d)
		}
	}
}

// ---- stream timing ---------------------------------------------------------------------------

func TestStreamWaitsForHeadersLongerThanTheRequestTimeout(t *testing.T) {
	c, srv, _ := setup(t, func(cfg *kagent.Config) {
		cfg.RequestTimeout = 50 * time.Millisecond
		cfg.StreamIdleTimeout = 5 * time.Second
	})
	srv.FailNext("SendStreamingMessage", kagenttest.Fault{Delay: 300 * time.Millisecond, Status: 0})
	events, err := send(c, "m1")
	if err != nil || len(events) != 3 {
		t.Fatalf("%d events, err %v", len(events), err)
	}
}

func TestStreamHeaderWaitEndsAtTheIdleTimeout(t *testing.T) {
	c, srv, _ := setup(t, func(cfg *kagent.Config) { cfg.StreamIdleTimeout = 100 * time.Millisecond })
	srv.FailNext("SendStreamingMessage", kagenttest.Fault{Delay: 2 * time.Second})
	_, err := send(c, "m1")
	mustBe[*kagent.OutcomeUnknown](t, err)
}

func TestSSECommentsKeepAStreamAlive(t *testing.T) {
	c, srv, _ := setup(t, func(cfg *kagent.Config) { cfg.StreamIdleTimeout = 250 * time.Millisecond })
	task := &a2a.Task{ID: "t1", ContextID: "ctx-1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}
	done := &a2a.Task{ID: "t1", ContextID: "ctx-1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}
	var steps []kagenttest.Step
	steps = append(steps, kagenttest.Emit{Event: task})
	for range 4 { // 4 x 100ms is longer than the idle timeout, but never silent for 250ms
		steps = append(steps, kagenttest.Pause{D: 100 * time.Millisecond}, kagenttest.Comment{})
	}
	steps = append(steps, kagenttest.Emit{Event: done})
	srv.Script(steps...)
	events, err := send(c, "m1")
	if err != nil || len(events) != 2 {
		t.Fatalf("%d events, err %v", len(events), err)
	}
}

func TestPlainBodyReadIsBoundedByTheIdleTimeout(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0",`))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer gw.Close()
	c, err := kagent.New(kagent.Config{GatewayURL: gw.URL, UserID: "u", StreamIdleTimeout: 150 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = send(c, "m1")
	mustBe[*kagent.OutcomeUnknown](t, err)
	if time.Since(start) > 3*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
}

// ---- observing -------------------------------------------------------------------------------

func TestFindTaskForMessageReportsFoundWhenTheRereadFails(t *testing.T) {
	c, srv, _ := setup(t)
	srv.AddTask(&a2a.Task{
		ID: "t1", ContextID: "ctx-1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
		History: []*a2a.Message{{ID: "msg-1", Role: a2a.MessageRoleUser}},
	})
	srv.FailNext("GetTask", kagenttest.Fault{Status: 502})
	task, found, err := c.FindTaskForMessage(ctx, agent, "ctx-1", "msg-1")
	mustBe[*kagent.OutcomeUnknown](t, err)
	if !found || task != nil {
		t.Fatalf("found %v, task %v: a delivered message must not look missing", found, task)
	}
}

func TestListingsFailOnARepeatedPageToken(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body struct {
			ID     string `json:"id"`
			Params struct {
				PageToken string `json:"pageToken"`
			} `json:"params"`
		}
		_ = readJSON(r, &body)
		next := map[string]string{"": "A", "A": "B", "B": "A"}[body.Params.PageToken]
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"` + body.ID + `","result":{"tasks":[],"nextPageToken":"` + next + `","pageSize":1,"totalSize":9}}`))
	}))
	defer gw.Close()
	c, err := kagent.New(kagent.Config{GatewayURL: gw.URL, UserID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ListTasks(ctx, agent, "ctx-1")
	oe := asType[*kagent.OutcomeUnknown](t, err)
	if !strings.Contains(oe.Detail, "repeated") {
		t.Fatalf("detail %q", oe.Detail)
	}
}

// ---- token cache -----------------------------------------------------------------------------

func TestReplacedTokenFileIsReadEvenWithTheSameSizeAndMtime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	mtime := time.Now().Add(-time.Hour).Truncate(time.Second)
	writeToken(t, path, "token-one", mtime)
	c, srv, _ := setup(t, func(cfg *kagent.Config) { cfg.ControlTokenFile = path })
	srv.AddAgent("kagent", "mainloop-main")
	if _, err := c.GetAgent(ctx, agent); err != nil {
		t.Fatal(err)
	}
	// A new file (new inode) of the same size, with the old mtime put back, as a secret
	// rotation that preserves timestamps would leave it.
	next := filepath.Join(dir, "token.next")
	writeToken(t, next, "token-two", mtime)
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetAgent(ctx, agent); err != nil {
		t.Fatal(err)
	}
	reqs := srv.Requests()
	if got := reqs[len(reqs)-1].Header.Get("Authorization"); got != "Bearer token-two" {
		t.Fatalf("Authorization %q", got)
	}
}
