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
	"github.com/oldsj/mainloop/server/internal/canary"
)

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
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return canary.Run(context.Background(), canary.Options{
		Name: "prod", API: srv.URL, Model: "claude-haiku-5-5", Slow: time.Hour, Deadline: 300 * time.Millisecond,
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
