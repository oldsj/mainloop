package kagent_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	api "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

var (
	agent = kagent.AgentRef{Namespace: "kagent", Name: "mainloop-main"}
	ctx   = context.Background()
)

// clock is a fake clock whose Sleep advances it, so retry timing is exact and instant.
type clock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	return nil
}

func newClock() *clock { return &clock{now: time.Unix(1_000_000, 0)} }

func setup(t *testing.T, edit ...func(*kagent.Config)) (*kagent.Client, *kagenttest.Server, *clock) {
	t.Helper()
	srv := kagenttest.New()
	t.Cleanup(srv.Close)
	clk := newClock()
	cfg := kagent.Config{GatewayURL: srv.URL, UserID: "mainloop", Sleep: clk.Sleep, Now: clk.Now}
	for _, e := range edit {
		e(&cfg)
	}
	c, err := kagent.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c, srv, clk
}

func send(c *kagent.Client, id string) ([]a2a.Event, error) {
	var events []a2a.Event
	for ev, err := range c.SendMessage(ctx, agent, kagent.SendParams{
		Text: "hello", MessageID: id, ContextID: "ctx-1",
	}) {
		if err != nil {
			return events, err
		}
		events = append(events, ev)
	}
	return events, nil
}

func asType[T error](t *testing.T, err error) T {
	t.Helper()
	var target T
	if !errors.As(err, &target) {
		t.Fatalf("got %T (%v), want %T", err, err, target)
	}
	return target
}

func notType[T error](t *testing.T, err error) {
	t.Helper()
	var target T
	if errors.As(err, &target) {
		t.Fatalf("got %T (%v), which must not be %T", err, err, target)
	}
}

// ---- unary calls -----------------------------------------------------------------------------

func TestProcedureNamesMatchTheForkDefinitions(t *testing.T) {
	want := map[string]string{
		kagent.ProcCreateSession:              "/kagent.api.v1alpha1.SessionService/CreateSession",
		kagent.ProcPrepareSessionWorkspace:    "/kagent.api.v1alpha1.SessionService/PrepareSessionWorkspace",
		kagent.ProcGetSession:                 "/kagent.api.v1alpha1.SessionService/GetSession",
		kagent.ProcListSessions:               "/kagent.api.v1alpha1.SessionService/ListSessions",
		kagent.ProcSuspendSession:             "/kagent.api.v1alpha1.SessionService/SuspendSession",
		kagent.ProcResumeSession:              "/kagent.api.v1alpha1.SessionService/ResumeSession",
		kagent.ProcDeleteSession:              "/kagent.api.v1alpha1.SessionService/DeleteSession",
		kagent.ProcGetAgent:                   "/kagent.api.v1alpha1.AgentService/GetAgent",
		kagent.ProcCreateScheduledRun:         "/kagent.api.v1alpha1.ScheduledRunService/CreateScheduledRun",
		kagent.ProcGetScheduledRun:            "/kagent.api.v1alpha1.ScheduledRunService/GetScheduledRun",
		kagent.ProcListScheduledRuns:          "/kagent.api.v1alpha1.ScheduledRunService/ListScheduledRuns",
		kagent.ProcUpdateScheduledRun:         "/kagent.api.v1alpha1.ScheduledRunService/UpdateScheduledRun",
		kagent.ProcDeleteScheduledRun:         "/kagent.api.v1alpha1.ScheduledRunService/DeleteScheduledRun",
		kagent.ProcTriggerScheduledRun:        "/kagent.api.v1alpha1.ScheduledRunService/TriggerScheduledRun",
		kagent.ProcGetScheduledRunExecution:   "/kagent.api.v1alpha1.ScheduledRunService/GetScheduledRunExecution",
		kagent.ProcListScheduledRunExecutions: "/kagent.api.v1alpha1.ScheduledRunService/ListScheduledRunExecutions",
	}
	for got, exact := range want {
		if got != exact {
			t.Errorf("got %q, want %q", got, exact)
		}
	}
}

func TestSessionLifecycle(t *testing.T) {
	c, srv, _ := setup(t)
	srv.AddAgent("kagent", "claude-workspace")
	ref := &api.ResourceReference{Namespace: "kagent", Name: "claude-workspace"}

	created, err := c.CreateSession(ctx, &api.CreateSessionRequest{Agent: ref, RequestId: "req-1", Name: "work", Credentials: gitProxyCredentials()})
	if err != nil {
		t.Fatal(err)
	}
	if created.GetId() == "" || created.GetContextId() != created.GetId() || created.GetName() != "work" {
		t.Fatalf("created %v", created)
	}

	again, err := c.CreateSession(ctx, &api.CreateSessionRequest{Agent: ref, RequestId: "req-1", Name: "work", Credentials: gitProxyCredentials()})
	if err != nil || again.GetId() != created.GetId() {
		t.Fatalf("same request id must return the same session: %v, %v", again, err)
	}

	got, err := c.GetSession(ctx, created.GetId())
	if err != nil || got.GetId() != created.GetId() {
		t.Fatalf("get: %v, %v", got, err)
	}
	if s, err := c.SuspendSession(ctx, created.GetId()); err != nil || s.GetState() != api.RuntimeState_RUNTIME_STATE_SUSPENDED {
		t.Fatalf("suspend: %v, %v", s, err)
	}
	if s, err := c.ResumeSession(ctx, created.GetId()); err != nil || s.GetState() != api.RuntimeState_RUNTIME_STATE_READY {
		t.Fatalf("resume: %v, %v", s, err)
	}
	if r, err := c.PrepareSessionWorkspace(ctx, &api.PrepareSessionWorkspaceRequest{SessionId: created.GetId()}); err != nil || r == nil {
		t.Fatalf("prepare: %v, %v", r, err)
	}
	if _, err := c.DeleteSession(ctx, created.GetId()); err != nil {
		t.Fatal(err)
	}
	if _, ok := srv.Session(created.GetId()); ok {
		t.Fatal("session still stored after delete")
	}

	for _, key := range []string{
		"/kagent.api.v1alpha1.SessionService/CreateSession",
		"/kagent.api.v1alpha1.SessionService/GetSession",
		"/kagent.api.v1alpha1.SessionService/SuspendSession",
		"/kagent.api.v1alpha1.SessionService/ResumeSession",
		"/kagent.api.v1alpha1.SessionService/PrepareSessionWorkspace",
		"/kagent.api.v1alpha1.SessionService/DeleteSession",
	} {
		if srv.Count(key) == 0 {
			t.Errorf("no request reached %s", key)
		}
	}
}

func TestMissingSessionIsASessionError(t *testing.T) {
	c, _, _ := setup(t)
	_, err := c.GetSession(ctx, "nope")
	se := asType[*kagent.SessionError](t, err)
	if se.Code != connect.CodeNotFound {
		t.Fatalf("code %v", se.Code)
	}
}

func TestListSessionsPages(t *testing.T) {
	c, _, _ := setup(t)
	for i := range 5 {
		_, err := c.CreateSession(ctx, &api.CreateSessionRequest{RequestId: string(rune('a' + i))})
		if err != nil {
			t.Fatal(err)
		}
	}
	page, next, err := c.ListSessionsPage(ctx, "", 2)
	if err != nil || len(page) != 2 || next == "" {
		t.Fatalf("page %d, next %q, err %v", len(page), next, err)
	}
	all, err := c.ListSessions(ctx)
	if err != nil || len(all) != 5 {
		t.Fatalf("all %d, err %v", len(all), err)
	}
}

func TestGetAgent(t *testing.T) {
	c, srv, _ := setup(t)
	srv.AddAgent("kagent", "mainloop-main")
	got, err := c.GetAgent(ctx, agent)
	if err != nil || got.GetRef().GetName() != "mainloop-main" {
		t.Fatalf("got %v, %v", got, err)
	}
	_, err = c.GetAgent(ctx, kagent.AgentRef{Namespace: "kagent", Name: "missing"})
	mustBe[*kagent.SessionError](t, err)
}

func TestCreateSessionRejectsAReplyThatChangesTheContract(t *testing.T) {
	c, srv, _ := setup(t)
	srv.TamperSession = func(_ string, s *api.Session) { s.ContextId = "other" }
	_, err := c.CreateSession(ctx, &api.CreateSessionRequest{RequestId: "r"})
	mustBe[*kagent.OutcomeUnknown](t, err)
}

// ---- auth ------------------------------------------------------------------------------------

func TestUserIDAuth(t *testing.T) {
	c, srv, _ := setup(t)
	srv.AddAgent("kagent", "mainloop-main")
	if _, err := c.GetAgent(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if _, err := send(c, "m1"); err != nil {
		t.Fatal(err)
	}
	reqs := srv.Requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests", len(reqs))
	}
	for _, r := range reqs {
		if got := r.Header.Get("X-User-Id"); got != "mainloop" {
			t.Errorf("%s: x-user-id %q", r.Key, got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("%s: unexpected Authorization", r.Key)
		}
		if got := r.Header.Get("A2A-Extensions"); got != "https://kagent.dev/extensions/hitl/v1" {
			t.Errorf("%s: A2A-Extensions %q", r.Key, got)
		}
	}
}

func writeToken(t *testing.T, path, token string, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestBearerAuthRereadsTheTokenFileWhenItChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	base := time.Now().Add(-time.Hour)
	writeToken(t, path, "token-one\n", base)
	c, srv, _ := setup(t, func(cfg *kagent.Config) { cfg.ControlTokenFile = path })
	srv.AddAgent("kagent", "mainloop-main")

	call := func() {
		t.Helper()
		if _, err := c.GetAgent(ctx, agent); err != nil {
			t.Fatal(err)
		}
	}
	call()
	call()
	// Same size, new mtime: still re-read.
	writeToken(t, path, "token-two", base.Add(time.Minute))
	call()

	var bearers []string
	for _, r := range srv.Requests() {
		if r.Header.Get("X-User-Id") != "" {
			t.Error("x-user-id sent in bearer mode")
		}
		bearers = append(bearers, r.Header.Get("Authorization"))
	}
	want := []string{"Bearer token-one", "Bearer token-one", "Bearer token-two"}
	if strings.Join(bearers, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q", bearers)
	}
}

func TestBearerAuthAlsoAppliesToA2A(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	writeToken(t, path, "secret-token", time.Now())
	c, srv, _ := setup(t, func(cfg *kagent.Config) { cfg.ControlTokenFile = path })
	if _, err := send(c, "m1"); err != nil {
		t.Fatal(err)
	}
	r := srv.Requests()[0]
	if r.Header.Get("Authorization") != "Bearer secret-token" ||
		r.Header.Get("A2A-Extensions") != "https://kagent.dev/extensions/hitl/v1" {
		t.Fatalf("headers %v", r.Header)
	}
}

func TestBadTokenFilesAreConfigErrorsThatSendNothingAndLeakNothing(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"empty":      "",
		"whitespace": "abc def",
		"non-ascii":  "tökén",
		"too long":   strings.Repeat("a", 4097),
		"newline":    "abc\ndef",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
			writeToken(t, path, content, time.Now())
			c, srv, _ := setup(t, func(cfg *kagent.Config) { cfg.ControlTokenFile = path })
			_, err := c.GetAgent(ctx, agent)
			mustBe[*kagent.ConfigError](t, err)
			if content != "" && strings.Contains(err.Error(), content) {
				t.Fatal("error repeats the token file")
			}
			if _, err := send(c, "m1"); err == nil {
				t.Fatal("send succeeded with a bad token")
			} else {
				mustBe[*kagent.ConfigError](t, err)
			}
			if n := len(srv.Requests()); n != 0 {
				t.Fatalf("%d requests were sent", n)
			}
		})
	}
	t.Run("missing", func(t *testing.T) {
		c, _, _ := setup(t, func(cfg *kagent.Config) { cfg.ControlTokenFile = filepath.Join(dir, "absent") })
		_, err := c.GetAgent(ctx, agent)
		mustBe[*kagent.ConfigError](t, err)
	})
}

func TestNewValidatesConfig(t *testing.T) {
	_, err := kagent.New(kagent.Config{UserID: "mainloop"})
	mustBe[*kagent.ConfigError](t, err)
	_, err = kagent.New(kagent.Config{GatewayURL: "http://x"})
	mustBe[*kagent.ConfigError](t, err)
}

// ---- error classification -------------------------------------------------------------------

func deadServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	return url
}

func TestUnreachableMeansNotSent(t *testing.T) {
	clk := newClock()
	c, err := kagent.New(kagent.Config{GatewayURL: deadServer(t), UserID: "u", Sleep: clk.Sleep, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.GetAgent(ctx, agent)
	mustBe[*kagent.Unreachable](t, err)
	if !kagent.NotSent(err) {
		t.Fatal("NotSent must be true for Unreachable")
	}
	_, err = send(c, "m1")
	mustBe[*kagent.Unreachable](t, err)
	if len(clk.sleeps) != 0 {
		t.Fatalf("an unreachable gateway must not be retried, slept %v", clk.sleeps)
	}
}

func TestUnaryFailuresThatMayHaveRun(t *testing.T) {
	cases := map[string]kagenttest.Fault{
		"connection dropped": {Drop: true},
		"HTTP 502":           {Status: 502},
		"HTTP 500":           {Status: 500},
		"grpc aborted":       {GRPC: codePtr(connect.CodeAborted)},
		"grpc unavailable":   {GRPC: codePtr(connect.CodeUnavailable)},
		"grpc internal":      {GRPC: codePtr(connect.CodeInternal)},
		"grpc deadline":      {GRPC: codePtr(connect.CodeDeadlineExceeded)},
		"timeout":            {Delay: 2 * time.Second},
	}
	for name, fault := range cases {
		t.Run(name, func(t *testing.T) {
			c, srv, clk := setup(t, func(cfg *kagent.Config) { cfg.RequestTimeout = 100 * time.Millisecond })
			srv.FailNext(kagent.ProcCreateSession, fault)
			_, err := c.CreateSession(ctx, &api.CreateSessionRequest{RequestId: "r"})
			mustBe[*kagent.OutcomeUnknown](t, err)
			if kagent.NotSent(err) {
				t.Fatal("an unknown outcome must not look like not-sent")
			}
			if srv.Count(kagent.ProcCreateSession) != 1 || len(clk.sleeps) != 0 {
				t.Fatalf("sent %d times, slept %v", srv.Count(kagent.ProcCreateSession), clk.sleeps)
			}
		})
	}
}

func codePtr(c connect.Code) *connect.Code { return &c }

func TestAuthRefusalsAreConfigErrors(t *testing.T) {
	// credentials is whether the refusal is of our own bearer, which every call would meet.
	cases := map[string]struct {
		fault       kagenttest.Fault
		credentials bool
	}{
		"HTTP 401":             {kagenttest.Fault{Status: 401}, true},
		"HTTP 403":             {kagenttest.Fault{Status: 403}, false},
		"grpc unauthenticated": {kagenttest.Fault{GRPC: codePtr(connect.CodeUnauthenticated)}, true},
		"grpc permission":      {kagenttest.Fault{GRPC: codePtr(connect.CodePermissionDenied)}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, srv, _ := setup(t)
			srv.FailNext(kagent.ProcGetAgent, tc.fault)
			_, err := c.GetAgent(ctx, agent)
			if got := asType[*kagent.ConfigError](t, err).Credentials; got != tc.credentials {
				t.Fatalf("Credentials = %v, want %v", got, tc.credentials)
			}
		})
	}
	t.Run("A2A HTTP 401", func(t *testing.T) {
		c, srv, _ := setup(t)
		srv.FailNext("SendStreamingMessage", kagenttest.Fault{Status: 401})
		_, err := send(c, "m1")
		if !asType[*kagent.ConfigError](t, err).Credentials {
			t.Fatal("HTTP 401 refuses our bearer")
		}
		if srv.Count("SendStreamingMessage") != 1 {
			t.Fatal("an auth refusal must not be retried")
		}
	})
	t.Run("A2A UNAUTHORIZED", func(t *testing.T) {
		c, srv, _ := setup(t)
		srv.FailNext("GetTask", kagenttest.Fault{RPC: kagenttest.Unauthorized()})
		_, err := c.GetTask(ctx, agent, "t1")
		if asType[*kagent.ConfigError](t, err).Credentials {
			t.Fatal("UNAUTHORIZED is about one conversation, not our bearer")
		}
	})
	t.Run("A2A UNAUTHENTICATED", func(t *testing.T) {
		c, srv, _ := setup(t)
		srv.FailNext("GetTask", kagenttest.Fault{RPC: kagenttest.Unauthenticated()})
		_, err := c.GetTask(ctx, agent, "t1")
		if !asType[*kagent.ConfigError](t, err).Credentials {
			t.Fatal("UNAUTHENTICATED refuses our bearer")
		}
	})
}

func TestAnUnreadySessionRefusalIsNotSent(t *testing.T) {
	c, srv, clk := setup(t, func(cfg *kagent.Config) { cfg.SendRetryBudget = 250 * time.Millisecond })
	for range 5 {
		srv.FailNext("SendStreamingMessage", kagenttest.Fault{RPC: kagenttest.NotReady()})
	}
	_, err := send(c, "m1")
	mustBe[*kagent.SendNotAccepted](t, err)
	if !kagent.NotSent(err) || len(clk.sleeps) == 0 {
		t.Fatalf("not sent = %v, slept %v", kagent.NotSent(err), clk.sleeps)
	}
}

func TestOtherGRPCStatusesAreSessionErrors(t *testing.T) {
	c, srv, _ := setup(t)
	srv.FailNext(kagent.ProcGetSession, kagenttest.Fault{GRPC: codePtr(connect.CodeFailedPrecondition)})
	_, err := c.GetSession(ctx, "s")
	se := asType[*kagent.SessionError](t, err)
	if se.Code != connect.CodeFailedPrecondition {
		t.Fatalf("code %v", se.Code)
	}
}

func TestCallerCancelAfterSendIsOutcomeUnknown(t *testing.T) {
	c, srv, _ := setup(t)
	srv.FailNext(kagent.ProcGetSession, kagenttest.Fault{Delay: 5 * time.Second})
	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	_, err := c.GetSession(cctx, "s")
	mustBe[*kagent.OutcomeUnknown](t, err)
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "outcome unknown") {
		t.Fatalf("got %v", err)
	}
}

// fixtureServer answers every request with a file from testdata.
func fixtureServer(t *testing.T, status int, contentType, name string, vars ...string) *httptest.Server {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.NewReplacer(vars...).Replace(string(body))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(text))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func clientFor(t *testing.T, url string) (*kagent.Client, *clock) {
	t.Helper()
	clk := newClock()
	c, err := kagent.New(kagent.Config{GatewayURL: url, UserID: "u", Sleep: clk.Sleep, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	return c, clk
}

func TestWireFixtures(t *testing.T) {
	t.Run("send not accepted", func(t *testing.T) {
		c, _ := clientFor(t, fixtureServer(t, 200, "application/json", "send-not-accepted.json").URL)
		_, err := c.GetTask(ctx, agent, "t")
		refused := asType[*kagent.SendNotAccepted](t, err)
		if refused.RetryAfter() != 100*time.Millisecond || !kagent.NotSent(err) {
			t.Fatalf("retry after %v", refused.RetryAfter())
		}
		a := asType[*kagent.A2AError](t, err)
		if a.Code != -32004 || a.Domain != "a2a-protocol.org" {
			t.Fatalf("%+v", a)
		}
	})
	t.Run("task not found", func(t *testing.T) {
		c, _ := clientFor(t, fixtureServer(t, 200, "application/json", "task-not-found.json").URL)
		_, err := c.GetTask(ctx, agent, "t")
		mustBe[*kagent.TaskNotFound](t, err)
		if kagent.NotSent(err) {
			t.Fatal("task not found says nothing about delivery")
		}
	})
	t.Run("turn stream", func(t *testing.T) {
		srv := fixtureServer(t, 200, "text/event-stream", "turn-stream.sse",
			"{message_id}", "msg-1", "{text}", "hello")
		c, _ := clientFor(t, srv.URL)
		events, err := send(c, "msg-1")
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 4 {
			t.Fatalf("%d events", len(events))
		}
		first := events[0].(*a2a.Task)
		if first.ID != "task-fixture-1" || first.Status.State != a2a.TaskStateSubmitted || first.History[0].ID != "msg-1" {
			t.Fatalf("first %+v", first)
		}
		if u := events[1].(*a2a.TaskStatusUpdateEvent); u.Status.State != a2a.TaskStateWorking {
			t.Fatalf("second %+v", u)
		}
		if _, ok := events[2].(*a2a.TaskArtifactUpdateEvent); !ok {
			t.Fatalf("third %T", events[2])
		}
		last := events[3].(*a2a.Task)
		if last.Status.State != a2a.TaskStateCompleted || len(last.Artifacts) != 1 {
			t.Fatalf("last %+v", last)
		}
	})
}

func TestA2AErrorsWithoutTheSendNotAcceptedMarker(t *testing.T) {
	cases := map[string]*kagenttest.RPCError{
		"no ErrorInfo": {Code: -32603, Message: "boom"},
		"wrong domain": {
			Code: -32004, Message: "x", Reason: "UNSUPPORTED_OPERATION", Domain: "other.example",
			Metadata: map[string]string{"reason": "KAGENT_SEND_NOT_ACCEPTED"},
		},
		"marker in the top-level reason": {
			Code: -32004, Message: "x", Reason: "KAGENT_SEND_NOT_ACCEPTED", Domain: "a2a-protocol.org",
		},
	}
	for name, rpc := range cases {
		t.Run(name, func(t *testing.T) {
			c, srv, clk := setup(t)
			srv.FailNext("SendStreamingMessage", kagenttest.Fault{RPC: rpc})
			_, err := send(c, "m1")
			mustBe[*kagent.A2AError](t, err)
			notType[*kagent.SendNotAccepted](t, err)
			if srv.Count("SendStreamingMessage") != 1 || len(clk.sleeps) != 0 {
				t.Fatalf("sent %d times, slept %v", srv.Count("SendStreamingMessage"), clk.sleeps)
			}
		})
	}
}

func TestA2AGatewayErrorsWithoutAnEnvelope(t *testing.T) {
	t.Run("HTTP 502 is unknown", func(t *testing.T) {
		c, srv, _ := setup(t)
		srv.FailNext("GetTask", kagenttest.Fault{Status: 502})
		_, err := c.GetTask(ctx, agent, "t")
		mustBe[*kagent.OutcomeUnknown](t, err)
	})
	t.Run("HTTP 404 is a protocol error", func(t *testing.T) {
		c, srv, _ := setup(t)
		srv.FailNext("GetTask", kagenttest.Fault{Status: 404})
		_, err := c.GetTask(ctx, agent, "t")
		pe := asType[*kagent.ProtocolError](t, err)
		if pe.Status != 404 {
			t.Fatalf("status %d", pe.Status)
		}
	})
}

// ---- send: retry rule ------------------------------------------------------------------------

func TestSendRetriesOnlyARefusalAndResendsTheSameMessage(t *testing.T) {
	c, srv, clk := setup(t)
	srv.FailNext("SendStreamingMessage", kagenttest.Fault{RPC: kagenttest.NotAccepted(10)})
	srv.FailNext("SendStreamingMessage", kagenttest.Fault{RPC: kagenttest.NotAccepted(10)})
	events, err := send(c, "msg-1")
	if err != nil || len(events) != 3 {
		t.Fatalf("%d events, err %v", len(events), err)
	}
	var ids []string
	for _, r := range srv.Requests() {
		ids = append(ids, r.MessageID)
	}
	if strings.Join(ids, ",") != "msg-1,msg-1,msg-1" {
		t.Fatalf("message ids %q", ids)
	}
	// kagent's 10ms hint is raised to the 100ms then 200ms backoff.
	if len(clk.sleeps) != 2 || clk.sleeps[0] != 100*time.Millisecond || clk.sleeps[1] != 200*time.Millisecond {
		t.Fatalf("sleeps %v", clk.sleeps)
	}
}

func TestSendHonoursALongerRetryHint(t *testing.T) {
	c, srv, clk := setup(t)
	srv.FailNext("SendStreamingMessage", kagenttest.Fault{RPC: kagenttest.NotAccepted(1500)})
	if _, err := send(c, "m"); err != nil {
		t.Fatal(err)
	}
	if len(clk.sleeps) != 1 || clk.sleeps[0] != 1500*time.Millisecond {
		t.Fatalf("sleeps %v", clk.sleeps)
	}
}

func TestSendGivesUpWhenTheRetryBudgetRunsOut(t *testing.T) {
	c, srv, clk := setup(t, func(cfg *kagent.Config) { cfg.SendRetryBudget = 250 * time.Millisecond })
	for range 5 {
		srv.FailNext("SendStreamingMessage", kagenttest.Fault{RPC: kagenttest.NotAccepted(10)})
	}
	_, err := send(c, "m")
	mustBe[*kagent.SendNotAccepted](t, err)
	// 100ms fits in the budget; the next 200ms would pass it.
	if srv.Count("SendStreamingMessage") != 2 || len(clk.sleeps) != 1 {
		t.Fatalf("sent %d times, slept %v", srv.Count("SendStreamingMessage"), clk.sleeps)
	}
}

func TestSendNeverRetriesWhatMayHaveBeenAccepted(t *testing.T) {
	cases := map[string]kagenttest.Fault{
		"connection dropped": {Drop: true},
		"HTTP 503":           {Status: 503},
		"timeout":            {Delay: 2 * time.Second},
	}
	for name, fault := range cases {
		t.Run(name, func(t *testing.T) {
			c, srv, clk := setup(t, func(cfg *kagent.Config) {
				cfg.RequestTimeout = 100 * time.Millisecond
				cfg.StreamIdleTimeout = 100 * time.Millisecond
			})
			srv.FailNext("SendStreamingMessage", fault)
			_, err := send(c, "m1")
			mustBe[*kagent.OutcomeUnknown](t, err)
			if srv.Count("SendStreamingMessage") != 1 || len(clk.sleeps) != 0 {
				t.Fatalf("sent %d times, slept %v", srv.Count("SendStreamingMessage"), clk.sleeps)
			}
		})
	}
}

func TestSendDoesNotRetryARefusalAfterTheFirstEvent(t *testing.T) {
	c, srv, clk := setup(t)
	first := &a2a.Task{ID: "t1", ContextID: "ctx-1", Status: a2a.TaskStatus{State: a2a.TaskStateSubmitted}}
	srv.Script(kagenttest.Emit{Event: first}, kagenttest.Fail{Err: kagenttest.NotAccepted(10)})
	events, err := send(c, "m1")
	mustBe[*kagent.SendNotAccepted](t, err)
	if len(events) != 1 || srv.Count("SendStreamingMessage") != 1 || len(clk.sleeps) != 0 {
		t.Fatalf("%d events, sent %d times, slept %v", len(events), srv.Count("SendStreamingMessage"), clk.sleeps)
	}
}

func TestSendValidatesBeforeSending(t *testing.T) {
	c, srv, _ := setup(t)
	for _, p := range []kagent.SendParams{
		{MessageID: "m", ContextID: "c"},
		{Text: "t", ContextID: "c"},
		{Text: "t", MessageID: "m"},
	} {
		var err error
		for _, e := range c.SendMessage(ctx, agent, p) {
			err = e
		}
		if err == nil {
			t.Fatalf("%+v was accepted", p)
		}
	}
	if len(srv.Requests()) != 0 {
		t.Fatal("an invalid message was sent")
	}
}

// ---- streaming -------------------------------------------------------------------------------

func TestStreamingHappyPath(t *testing.T) {
	c, srv, _ := setup(t)
	events, err := send(c, "msg-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("%d events", len(events))
	}
	submitted := events[0].(*a2a.Task)
	if submitted.Status.State != a2a.TaskStateSubmitted || submitted.History[0].ID != "msg-1" ||
		submitted.History[0].Parts[0].Text() != "hello" || submitted.ContextID != "ctx-1" {
		t.Fatalf("submitted %+v", submitted)
	}
	if u := events[1].(*a2a.TaskStatusUpdateEvent); u.Status.State != a2a.TaskStateWorking {
		t.Fatalf("working %+v", u)
	}
	done := events[2].(*a2a.Task)
	if done.Status.State != a2a.TaskStateCompleted || len(done.History) != 2 {
		t.Fatalf("done %+v", done)
	}
	req := srv.Requests()[0]
	if req.AgentPath != kagenttest.AgentPath("kagent", "mainloop-main") || req.MessageID != "msg-1" {
		t.Fatalf("request %+v", req)
	}
}

func TestSendCarriesMetadataAndExtensionsAndTheHITLHeader(t *testing.T) {
	c, srv, _ := setup(t)
	meta := map[string]any{kagent.HITLExtension: map[string]any{"type": "ask_user_response", "id": "q1"}}
	for _, err := range c.SendMessage(ctx, agent, kagent.SendParams{
		Text: "HITL response", MessageID: "msg-h", ContextID: "ctx-1", TaskID: "t1",
		Metadata: meta, Extensions: []string{kagent.HITLExtension},
	}) {
		if err != nil {
			t.Fatal(err)
		}
	}
	req := srv.Requests()[0]
	if req.MessageID != "msg-h" || !slices.Equal(req.Extensions, []string{kagent.HITLExtension}) {
		t.Fatalf("request %+v", req)
	}
	got, ok := req.Metadata[kagent.HITLExtension].(map[string]any)
	if !ok || got["type"] != "ask_user_response" || got["id"] != "q1" {
		t.Fatalf("metadata %v", req.Metadata)
	}
	if h := req.Header.Get("A2A-Extensions"); h != kagent.HITLExtension {
		t.Fatalf("A2A-Extensions %q", h)
	}
	// A plain message carries neither.
	if _, err := send(c, "msg-plain"); err != nil {
		t.Fatal(err)
	}
	plain := srv.Requests()[1]
	if len(plain.Metadata) != 0 || len(plain.Extensions) != 0 {
		t.Fatalf("plain request %+v", plain)
	}
}

func TestConsumerCanStopTheStream(t *testing.T) {
	c, _, _ := setup(t)
	n := 0
	for _, err := range c.SendMessage(ctx, agent, kagent.SendParams{Text: "t", MessageID: "m", ContextID: "c"}) {
		if err != nil {
			t.Fatal(err)
		}
		n++
		break
	}
	if n != 1 {
		t.Fatalf("%d events", n)
	}
}

func TestMidStreamDropIsOutcomeUnknownAndTheMessageIsFoundNotResent(t *testing.T) {
	c, srv, clk := setup(t)
	task := &a2a.Task{
		ID: "t1", ContextID: "ctx-1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking},
		History: []*a2a.Message{{ID: "msg-1", ContextID: "ctx-1", Role: a2a.MessageRoleUser, TaskID: "t1"}},
	}
	srv.AddTask(task)
	srv.Script(kagenttest.Emit{Event: task}, kagenttest.Drop{})

	events, err := send(c, "msg-1")
	oe := asType[*kagent.OutcomeUnknown](t, err)
	if kagent.NotSent(err) || !strings.Contains(oe.Detail, "stream broke") {
		t.Fatalf("detail %q", oe.Detail)
	}
	if len(events) != 1 {
		t.Fatalf("%d events before the drop", len(events))
	}
	if srv.Count("SendStreamingMessage") != 1 || len(clk.sleeps) != 0 {
		t.Fatalf("resent: sent %d times, slept %v", srv.Count("SendStreamingMessage"), clk.sleeps)
	}

	found, ok, err := c.FindTaskForMessage(ctx, agent, "ctx-1", "msg-1")
	if err != nil || !ok || found.ID != "t1" {
		t.Fatalf("found %v, %v, %v", found, ok, err)
	}
	if srv.Count("SendStreamingMessage") != 1 {
		t.Fatal("observing must not send")
	}
}

func TestSilentStreamTimesOutAsOutcomeUnknown(t *testing.T) {
	c, srv, _ := setup(t, func(cfg *kagent.Config) { cfg.StreamIdleTimeout = 150 * time.Millisecond })
	task := &a2a.Task{ID: "t1", ContextID: "ctx-1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}
	srv.Script(kagenttest.Emit{Event: task}, kagenttest.Hang{})
	events, err := send(c, "m1")
	oe := asType[*kagent.OutcomeUnknown](t, err)
	if len(events) != 1 || !strings.Contains(oe.Detail, "stream broke") {
		t.Fatalf("%d events, detail %q", len(events), oe.Detail)
	}
}

func TestSubscribeToTask(t *testing.T) {
	c, srv, _ := setup(t)
	if _, err := send(c, "msg-1"); err != nil {
		t.Fatal(err)
	}
	var got []a2a.Event
	for ev, err := range c.SubscribeToTask(ctx, agent, "task-1") {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, ev)
	}
	if len(got) != 1 || got[0].(*a2a.Task).Status.State != a2a.TaskStateCompleted {
		t.Fatalf("got %v", got)
	}
	for _, err := range c.SubscribeToTask(ctx, agent, "missing") {
		mustBe[*kagent.TaskNotFound](t, err)
	}
	if srv.Count("SubscribeToTask") != 2 {
		t.Fatal("subscribe not recorded")
	}
}

// ---- observing -------------------------------------------------------------------------------

func TestGetTaskAndListTasks(t *testing.T) {
	c, srv, _ := setup(t)
	for _, id := range []a2a.TaskID{"t1", "t2", "t3", "t4", "t5"} {
		ctxID := "ctx-1"
		if id == "t5" {
			ctxID = "other"
		}
		srv.AddTask(&a2a.Task{
			ID: id, ContextID: ctxID, Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
			Artifacts: []*a2a.Artifact{{ID: "a", Parts: a2a.ContentParts{a2a.NewTextPart("x")}}},
		})
	}
	got, err := c.GetTask(ctx, agent, "t2")
	if err != nil || got.ID != "t2" || len(got.Artifacts) != 1 {
		t.Fatalf("get: %+v, %v", got, err)
	}
	_, err = c.GetTask(ctx, agent, "nope")
	mustBe[*kagent.TaskNotFound](t, err)

	page, next, err := c.ListTasksPage(ctx, agent, "ctx-1", "", 3)
	if err != nil || len(page) != 3 || next == "" {
		t.Fatalf("page %d, next %q, err %v", len(page), next, err)
	}
	if len(page[0].Artifacts) != 0 {
		t.Fatal("a listed task has no artifacts")
	}
	all, err := c.ListTasks(ctx, agent, "ctx-1")
	if err != nil || len(all) != 4 {
		t.Fatalf("all %d, err %v", len(all), err)
	}
}

// The plain latest-task list is what a liveness sweep reads for every task of a conversation, so it
// must stay small however long the conversation is: no artifacts, and only the newest message.
func TestListTasksLatestCarriesTheNewestMessageAndNoArtifacts(t *testing.T) {
	c, srv, _ := setup(t)
	srv.AddTask(&a2a.Task{
		ID: "t1", ContextID: "ctx-1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking},
		History:   []*a2a.Message{{ID: "m1", Role: a2a.MessageRoleUser}, {ID: "m2", Role: a2a.MessageRoleUser}},
		Artifacts: []*a2a.Artifact{{ID: "a", Parts: a2a.ContentParts{a2a.NewTextPart("x")}}},
	})
	got, err := c.ListTasksLatest(ctx, agent, "ctx-1")
	if err != nil || len(got) != 1 {
		t.Fatalf("got %d tasks, err %v", len(got), err)
	}
	if len(got[0].Artifacts) != 0 || len(got[0].History) != 1 || got[0].History[0].ID != "m2" {
		t.Fatalf("artifacts %d, history %+v; want no artifacts and only the newest message", len(got[0].Artifacts), got[0].History)
	}
}

// A busy harness turn records its text and tool calls as artifacts only, so the liveness read asks
// for them, for the tasks in one state and no others.
func TestListTasksInStateLatestCarriesArtifactsForThatStateOnly(t *testing.T) {
	c, srv, _ := setup(t)
	art := []*a2a.Artifact{{ID: "a", Parts: a2a.ContentParts{a2a.NewTextPart("x")}}}
	srv.AddTask(&a2a.Task{
		ID: "t1", ContextID: "ctx-1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking},
		History:   []*a2a.Message{{ID: "m1", Role: a2a.MessageRoleUser}, {ID: "m2", Role: a2a.MessageRoleUser}},
		Artifacts: art,
	})
	srv.AddTask(&a2a.Task{ID: "t2", ContextID: "ctx-1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}, Artifacts: art})
	srv.AddTask(&a2a.Task{ID: "t3", ContextID: "ctx-2", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}, Artifacts: art})
	got, err := c.ListTasksInStateLatest(ctx, agent, "ctx-1", a2a.TaskStateWorking)
	if err != nil || len(got) != 1 || got[0].ID != "t1" {
		t.Fatalf("got %+v, err %v; want only the working task of the conversation", got, err)
	}
	if len(got[0].Artifacts) != 1 || len(got[0].History) != 1 || got[0].History[0].ID != "m2" {
		t.Fatalf("artifacts %d, history %+v; want the artifact and only the newest message", len(got[0].Artifacts), got[0].History)
	}
}

func TestListWaitingTasksPageFiltersByStatusAndTrimsHistory(t *testing.T) {
	c, srv, _ := setup(t)
	msgs := func(ids ...string) []*a2a.Message {
		var out []*a2a.Message
		for _, id := range ids {
			out = append(out, &a2a.Message{ID: id, Role: a2a.MessageRoleUser})
		}
		return out
	}
	srv.AddTask(&a2a.Task{ID: "t1", ContextID: "ctx-1", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}, History: msgs("a", "b", "c")})
	srv.AddTask(&a2a.Task{ID: "t2", ContextID: "ctx-1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}, History: msgs("d")})
	srv.AddTask(&a2a.Task{ID: "t3", ContextID: "ctx-2", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}})
	page, next, err := c.ListWaitingTasksPage(ctx, agent, "ctx-1", "", 10)
	if err != nil || next != "" || len(page) != 1 || page[0].ID != "t1" {
		t.Fatalf("page %+v, next %q, err %v", page, next, err)
	}
	if len(page[0].History) != 1 || page[0].History[0].ID != "c" {
		t.Fatalf("history = %+v, want only the latest message", page[0].History)
	}
}

func TestFindTaskForMessageRereadsTheTaskForItsArtifactsAndReportsAMiss(t *testing.T) {
	c, srv, _ := setup(t)
	srv.AddTask(&a2a.Task{
		ID: "t1", ContextID: "ctx-1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
		History:   []*a2a.Message{{ID: "msg-1", Role: a2a.MessageRoleUser}},
		Artifacts: []*a2a.Artifact{{ID: "a", Parts: a2a.ContentParts{a2a.NewTextPart("x")}}},
	})
	task, ok, err := c.FindTaskForMessage(ctx, agent, "ctx-1", "msg-1")
	if err != nil || !ok || len(task.Artifacts) != 1 {
		t.Fatalf("found %+v, %v, %v", task, ok, err)
	}
	_, ok, err = c.FindTaskForMessage(ctx, agent, "ctx-1", "msg-2")
	if err != nil || ok {
		t.Fatalf("a message nobody has must be a clean miss: %v, %v", ok, err)
	}
	if srv.Count("SendStreamingMessage") != 0 {
		t.Fatal("finding must not send")
	}
}

// mustBe is asType for callers that only need the check.
func mustBe[T error](t *testing.T, err error) {
	t.Helper()
	_ = asType[T](t, err)
}

func readJSON(r *http.Request, v any) error { return json.NewDecoder(r.Body).Decode(v) }

// gitProxyCredentials are the two references kagent's preparation check counts, one for each Git
// proxy; a Session without both cannot be prepared.
func gitProxyCredentials() []*api.SessionCredential {
	ref := func(service string) *api.SessionCredential {
		return &api.SessionCredential{
			Origin: "http://mainloop-git-" + service + ".mainloop.svc.cluster.local", Header: "authorization",
			SecretRef: &api.SecretKeyReference{Name: "git-" + service, Key: "authorization"},
		}
	}
	return []*api.SessionCredential{ref("read"), ref("push")}
}
