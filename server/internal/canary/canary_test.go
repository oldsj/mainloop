package canary_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/auth"
	"github.com/oldsj/mainloop/server/internal/auth/authtest"
	"github.com/oldsj/mainloop/server/internal/canary"
)

// serve puts h behind the owner API's authentication in enforce mode, allowing the canary
// ServiceAccount the canary scope, and returns the URL and the canary's token. Every run in these
// tests goes through it, so a call outside the canary scope fails the test.
func serve(t *testing.T, h http.Handler) (string, auth.TokenSource) {
	t.Helper()
	gate := authtest.NewGate(t, map[string]auth.Scope{"mainloop/mainloop-canary": auth.ScopeCanary})
	tok := gate.Token(t, "mainloop", "mainloop-canary")
	srv := httptest.NewServer(gate.Wrap(h))
	t.Cleanup(srv.Close)
	return srv.URL, func() (string, error) { return tok, nil }
}

var today = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

// fakeAPI is the slice of the owner API the canary uses.
type fakeAPI struct {
	mu       sync.Mutex
	sessions []api.Session
	messages map[string][]api.Message
	calls    []string
	// reply makes the agent answer each user message.
	reply        bool
	listStatus   int
	createStatus int
	model        string
	ended        map[string]bool
	archived     []string
	stopped      []string
	hidden       bool
	sendStatus   string
	// deliveries is what GET /sessions/{id}/native lists; nil answers 404.
	deliveries []api.NativeDeliveryInfo
	// restartable is the native strip's flag; restarts counts POST /restart, which answers
	// restartStatus when set and otherwise makes the agent answer from then on when
	// healOnRestart is set.
	restartable   bool
	restartStatus int
	restarts      int
	healOnRestart bool
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	userMsg := func(id, sid string) {
		f.messages[sid] = append(f.messages[sid], api.Message{Id: id, Role: api.MessageRoleUser, Content: canary.Prompt})
		if f.reply {
			f.messages[sid] = append(f.messages[sid], api.Message{Id: id + "-r", Role: api.MessageRoleAssistant, Content: "pong"})
		}
	}
	switch {
	case r.Method == http.MethodGet && len(parts) == 1:
		if r.URL.Query().Get("hidden") != "true" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if f.listStatus != 0 {
			w.WriteHeader(f.listStatus)
			return
		}
		write(f.sessions)
	case r.Method == http.MethodPost && len(parts) == 1:
		if f.createStatus != 0 {
			w.WriteHeader(f.createStatus)
			return
		}
		f.model, _ = body["model"].(string)
		f.hidden, _ = body["hidden"].(bool)
		s := api.Session{Id: "s-new", Title: body["title"].(string), Status: api.SessionStatusActive}
		f.sessions = append(f.sessions, s)
		userMsg("first", s.Id)
		write(s)
	case len(parts) == 3 && parts[2] == "message":
		if f.sendStatus == "ok" {
			userMsg(body["message_id"].(string), parts[1])
		}
		write(api.SessionMessageResponse{Status: api.SessionMessageResponseStatus(f.sendStatus), MessageId: "m"})
	case len(parts) == 3 && parts[2] == "stop-turn":
		f.stopped = append(f.stopped, parts[1])
		write(map[string]any{"status": "stopped"})
	case len(parts) == 3 && parts[2] == "cancel":
		if f.ended[parts[1]] {
			w.WriteHeader(http.StatusConflict)
			return
		}
		f.ended[parts[1]] = true
		write(map[string]any{})
	case len(parts) == 3 && parts[2] == "conversation":
		write(api.SessionConversationResponse{Session: api.Session{Id: parts[1], Status: api.SessionStatusActive}, Messages: f.messages[parts[1]]})
	case len(parts) == 3 && parts[2] == "native":
		if f.deliveries == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		write(api.NativeSessionInfo{Deliveries: f.deliveries, Restartable: &f.restartable})
	case len(parts) == 3 && parts[2] == "restart":
		f.restarts++
		if f.restartStatus != 0 {
			w.WriteHeader(f.restartStatus)
			return
		}
		if _, ok := body["request_id"].(string); !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.reply = f.reply || f.healOnRestart
		write(api.SessionRestartResponse{Status: "restarted", FailedMessageIds: []string{}})
	case len(parts) == 3 && parts[2] == "archive":
		if !f.ended[parts[1]] {
			w.WriteHeader(http.StatusConflict)
			return
		}
		f.archived = append(f.archived, parts[1])
		write(map[string]any{})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func run(t *testing.T, f *fakeAPI) canary.Result {
	t.Helper()
	if f.messages == nil {
		f.messages = map[string][]api.Message{}
	}
	if f.ended == nil {
		f.ended = map[string]bool{}
	}
	url, token := serve(t, f)
	return canary.Run(context.Background(), canary.Options{
		Name: "prod", API: url, Token: token, Model: "claude-haiku-5-5", Slow: time.Hour, Deadline: 300 * time.Millisecond,
		Poll: 20 * time.Millisecond, Request: time.Second, Now: func() time.Time { return today },
	})
}

func TestFirstRunCreatesAHiddenSessionOnTheSmallModel(t *testing.T) {
	f := &fakeAPI{reply: true}
	res := run(t, f)
	if !res.OK() || res.Session != "s-new" || !res.Created {
		t.Fatalf("result: %v", res)
	}
	if f.model != "claude-haiku-5-5" || !f.hidden {
		t.Fatalf("create must set the model and hide the session: model %q hidden %v", f.model, f.hidden)
	}
	if len(f.archived) != 0 {
		t.Fatalf("the new session stays for the next run: archived %v", f.archived)
	}
}

func TestLaterRunsWakeTheSameSession(t *testing.T) {
	f := &fakeAPI{reply: true, sendStatus: "ok", sessions: []api.Session{
		{Id: "s-live", Title: canary.TitlePrefix + "2026-11-12 09:00", Status: api.SessionStatusIdle},
		{Id: "s-dead", Title: canary.TitlePrefix + "2026-11-11 09:00", Status: api.SessionStatusFailed},
	}, ended: map[string]bool{"s-dead": true}}
	res := run(t, f)
	if !res.OK() || res.Session != "s-live" || res.Created {
		t.Fatalf("result: %v", res)
	}
	if f.model != "" {
		t.Fatal("must not create a session while one is usable")
	}
	if !slices.Equal(f.archived, []string{"s-dead"}) {
		t.Fatalf("only the broken session is retired: %v", f.archived)
	}
}

func TestABrokenSessionIsReplaced(t *testing.T) {
	f := &fakeAPI{reply: true, sessions: []api.Session{
		{Id: "s-dead", Title: canary.TitlePrefix + "2026-11-11 09:00", Status: api.SessionStatusFailed},
	}, ended: map[string]bool{"s-dead": true}}
	res := run(t, f)
	if !res.OK() || res.Session != "s-new" || !slices.Equal(f.archived, []string{"s-dead"}) {
		t.Fatalf("result %v archived %v", res, f.archived)
	}
}

func TestReportsTheFailedStepAndStopsTheTurn(t *testing.T) {
	live := func() []api.Session {
		return []api.Session{{Id: "s", Title: canary.TitlePrefix + "x", Status: api.SessionStatusIdle}}
	}
	cases := []struct {
		name    string
		f       *fakeAPI
		step    string
		stopped []string
	}{
		{"api down", &fakeAPI{listStatus: http.StatusBadGateway}, canary.StepList, nil},
		{"create refused", &fakeAPI{createStatus: http.StatusUnprocessableEntity}, canary.StepCreate, nil},
		{"turn still open", &fakeAPI{sendStatus: "queued", sessions: live()}, canary.StepSend, []string{"s"}},
		{"no reply", &fakeAPI{sendStatus: "ok", sessions: live()}, canary.StepReply, []string{"s"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := run(t, c.f)
			if res.Err == nil || res.Step != c.step {
				t.Fatalf("got %v, want step %s", res, c.step)
			}
			if !slices.Equal(c.f.stopped, c.stopped) {
				t.Fatalf("stopped %v, want %v", c.f.stopped, c.stopped)
			}
		})
	}
}

func TestAlert(t *testing.T) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		got = append(got, m)
	}))
	defer srv.Close()
	n := canary.Ntfy{URL: srv.URL, Topic: "mainloop"}
	ctx := context.Background()
	if err := n.Alert(ctx, []canary.Result{{Target: "prod"}}); err != nil || len(got) != 0 {
		t.Fatalf("a passing run must not alert: %v %v", err, got)
	}
	err := n.Alert(ctx, []canary.Result{
		{Target: "prod", Step: canary.StepReply, Err: context.DeadlineExceeded},
		{Target: "dev", Slow: true, Elapsed: 2 * time.Minute},
	})
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %v", err, got)
	}
	m := got[0]
	if m["title"] != "Mainloop canary failed" || m["priority"].(float64) != 4 ||
		!strings.Contains(m["message"].(string), "prod: await-reply failed") || !strings.Contains(m["message"].(string), "dev: reply took 2m0s") {
		t.Fatalf("alert: %+v", m)
	}
}

func TestADeliveryKagentRefusedFailsTheRunWithItsReason(t *testing.T) {
	refused := "send rejected: A2A error -32004: dispatch busy"
	scenarios := []struct {
		name     string
		existing bool
	}{
		{name: "Given a new canary session, when its first message is refused, assert that the run fails with the refusal", existing: false},
		{name: "Given an idle canary session, when the ping is refused, assert that the run fails with the refusal", existing: true},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			f := &fakeAPI{sendStatus: "ok"}
			if sc.existing {
				f.sessions = []api.Session{{Id: "s-live", Title: canary.TitlePrefix + "2026-11-12 09:00", Status: api.SessionStatusIdle}}
			}
			f.deliveries = []api.NativeDeliveryInfo{
				{MessageId: "an-older-one", State: "failed", Detail: ptr("not this one")},
			}
			f.messages = map[string][]api.Message{}
			f.ended = map[string]bool{}
			url, token := serve(t, &refusing{f: f, detail: refused})

			res := canary.Run(context.Background(), canary.Options{
				Name: "prod", API: url, Token: token, Slow: time.Hour, Deadline: 10 * time.Second,
				Poll: 20 * time.Millisecond, Request: time.Second, Now: func() time.Time { return today },
			})

			if res.Step != canary.StepReply || res.Err == nil || !strings.Contains(res.Err.Error(), "delivery failed: "+refused) {
				t.Fatalf("result: %v, want await-reply failing with the refusal", res)
			}
			if f.restarts != 0 {
				t.Fatalf("restarts = %d, want none for a session that is not restartable", f.restarts)
			}
		})
	}

	t.Run("Given the delivery ledger cannot be read, when no reply comes, assert that the run still waits out its deadline", func(t *testing.T) {
		f := &fakeAPI{sendStatus: "ok", sessions: []api.Session{
			{Id: "s-live", Title: canary.TitlePrefix + "2026-11-12 09:00", Status: api.SessionStatusIdle},
		}}

		res := run(t, f)

		if res.Step != canary.StepReply || res.Err == nil || !strings.Contains(res.Err.Error(), "no reply within") {
			t.Fatalf("result: %v, want the reply deadline", res)
		}
	})
}

// refusing records each sent message that got no reply as a failed delivery, as the backend
// does once kagent refuses it.
type refusing struct {
	f      *fakeAPI
	detail string
}

func (r *refusing) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.f.ServeHTTP(w, req)
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	for _, msgs := range r.f.messages {
		for i, m := range msgs {
			answered := i+1 < len(msgs) && msgs[i+1].Role == api.MessageRoleAssistant
			if m.Role == api.MessageRoleUser && !answered && !slices.ContainsFunc(r.f.deliveries, func(d api.NativeDeliveryInfo) bool { return d.MessageId == m.Id }) {
				r.f.deliveries = append(r.f.deliveries, api.NativeDeliveryInfo{MessageId: m.Id, State: "failed", Detail: ptr(r.detail)})
			}
		}
	}
}

func ptr(s string) *string { return &s }

func TestARefusedPingRestartsARestartableSession(t *testing.T) {
	const refused = "not sent: A2A error -32004: input was not accepted; retry after the session becomes available"
	idle := []api.Session{{Id: "s-live", Title: canary.TitlePrefix + "2026-11-12 09:00", Status: api.SessionStatusIdle}}
	runRefused := func(t *testing.T, f *fakeAPI, detail string) canary.Result {
		t.Helper()
		f.sendStatus, f.sessions, f.deliveries = "ok", idle, []api.NativeDeliveryInfo{}
		f.messages, f.ended = map[string][]api.Message{}, map[string]bool{}
		url, token := serve(t, &refusing{f: f, detail: detail})
		return canary.Run(context.Background(), canary.Options{
			Name: "prod", API: url, Token: token, Slow: time.Hour, Deadline: 10 * time.Second,
			Poll: 20 * time.Millisecond, Request: time.Second, Now: func() time.Time { return today },
		})
	}

	t.Run("Given a restartable canary session, when kagent refuses the ping and the restart heals it, assert that the run restarts once and reports it", func(t *testing.T) {
		f := &fakeAPI{restartable: true, healOnRestart: true}

		res := runRefused(t, f, refused)

		if res.Err != nil || res.Restarted != refused || res.OK() || f.restarts != 1 {
			t.Fatalf("result %v, %d restarts; want one restart reported with the refusal", res, f.restarts)
		}
		if !strings.HasPrefix(res.String(), "restarted prod session=s-live after kagent refused the ping") {
			t.Fatalf("report %q", res.String())
		}
	})

	t.Run("Given a restartable canary session, when kagent still refuses the ping after the restart, assert that the run fails after one restart", func(t *testing.T) {
		f := &fakeAPI{restartable: true}

		res := runRefused(t, f, refused)

		if res.Step != canary.StepReply || res.Err == nil || !strings.Contains(res.Err.Error(), "after restarting the session: delivery failed") || f.restarts != 1 {
			t.Fatalf("result %v, %d restarts; want await-reply failing after one restart", res, f.restarts)
		}
	})

	t.Run("Given a restartable canary session, when the restart is refused, assert that the run fails naming both", func(t *testing.T) {
		f := &fakeAPI{restartable: true, restartStatus: http.StatusConflict}

		res := runRefused(t, f, refused)

		if res.Err == nil || !strings.Contains(res.Err.Error(), "delivery failed: "+refused) ||
			!strings.Contains(res.Err.Error(), "restarting the session failed") || f.restarts != 1 {
			t.Fatalf("result %v, %d restarts", res, f.restarts)
		}
	})

	t.Run("Given a restartable canary session, when the ping was not sent because kagent did not answer, assert that the run does not restart", func(t *testing.T) {
		f := &fakeAPI{restartable: true, healOnRestart: true}

		res := runRefused(t, f, "not sent: kagent unreachable: connection refused")

		if res.Err == nil || f.restarts != 0 {
			t.Fatalf("result %v, %d restarts; want a failure and no restart", res, f.restarts)
		}
	})
}

func TestAlertReportsARestartOnce(t *testing.T) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		got = append(got, m)
	}))
	defer srv.Close()

	err := canary.Ntfy{URL: srv.URL, Topic: "mainloop"}.Alert(context.Background(), []canary.Result{
		{Target: "prod", Restarted: "not sent: A2A error -32004: input was not accepted", Elapsed: 9 * time.Second},
	})

	if err != nil || len(got) != 1 {
		t.Fatalf("%v %v", err, got)
	}
	m := got[0]
	if m["title"] != "Mainloop canary restarted its session" || m["priority"].(float64) != 3 ||
		!strings.Contains(m["message"].(string), "prod: kagent refused the ping (not sent: A2A error -32004") {
		t.Fatalf("alert: %+v", m)
	}
}

func TestTheCanaryAuthenticatesAsItsServiceAccount(t *testing.T) {
	var (
		mu  sync.Mutex
		got http.Header
	)
	f := &fakeAPI{reply: true, messages: map[string][]api.Message{}, ended: map[string]bool{}}
	url, token := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Header.Clone()
		mu.Unlock()
		f.ServeHTTP(w, r)
	}))
	opts := canary.Options{Name: "prod", API: url, Model: "claude-haiku-5-5", Slow: time.Hour, Deadline: 300 * time.Millisecond,
		Poll: 20 * time.Millisecond, Request: time.Second, Now: func() time.Time { return today }}

	t.Run("without a token the first step is refused", func(t *testing.T) {
		res := canary.Run(context.Background(), opts)
		if res.Step != canary.StepList || res.Err == nil || !strings.Contains(res.Err.Error(), "status 401") {
			t.Fatalf("result: %v", res)
		}
	})
	t.Run("an unreadable token fails the step without a request", func(t *testing.T) {
		o := opts
		o.Token = auth.TokenFile(t.TempDir() + "/missing")
		res := canary.Run(context.Background(), o)
		if res.Step != canary.StepList || res.Err == nil || !strings.Contains(res.Err.Error(), "token file") {
			t.Fatalf("result: %v", res)
		}
	})
	t.Run("with its token every step passes", func(t *testing.T) {
		o := opts
		o.Token = token
		if res := canary.Run(context.Background(), o); !res.OK() {
			t.Fatalf("result: %v", res)
		}
		mu.Lock()
		defer mu.Unlock()
		if got.Get(auth.ClientHeader) != "canary" || !strings.HasPrefix(got.Get("Authorization"), "Bearer ") {
			t.Fatalf("headers: %v", got)
		}
	})
}
