package canary_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	// sendStatus answers /message; reply makes the agent answer each user message.
	sendStatus string
	reply      bool
	listStatus int
	model      string
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
		if f.listStatus != 0 {
			w.WriteHeader(f.listStatus)
			return
		}
		write(f.sessions)
	case r.Method == http.MethodPost && len(parts) == 1:
		s := api.Session{Id: "s-new", Title: body["title"].(string), Status: api.SessionStatusActive}
		f.sessions = append(f.sessions, s)
		userMsg("first", s.Id)
		write(s)
	case len(parts) == 3 && parts[2] == "model":
		f.model, _ = body["model"].(string)
		write(map[string]any{"session_id": parts[1]})
	case len(parts) == 3 && parts[2] == "message":
		if f.sendStatus == "ok" {
			userMsg(body["message_id"].(string), parts[1])
		}
		write(api.SessionMessageResponse{Status: api.SessionMessageResponseStatus(f.sendStatus), MessageId: "m"})
	case len(parts) == 3 && parts[2] == "conversation":
		write(api.SessionConversationResponse{Session: api.Session{Id: parts[1], Status: api.SessionStatusActive}, Messages: f.messages[parts[1]]})
	case len(parts) == 3 && parts[2] == "archive":
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
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return canary.Run(context.Background(), canary.Options{
		Name: "prod", API: srv.URL, Model: "haiku", Slow: time.Hour, Deadline: 300 * time.Millisecond,
		Poll: 20 * time.Millisecond, Request: time.Second, Now: func() time.Time { return today },
	})
}

func TestFirstRunOfTheDayCreatesASessionOnTheSmallModel(t *testing.T) {
	f := &fakeAPI{reply: true, sessions: []api.Session{
		{Id: "s-old", Title: canary.TitlePrefix + "2026-11-11"},
		{Id: "s-other", Title: "Real work"},
	}}
	res := run(t, f)
	if !res.OK() || res.Session != "s-new" {
		t.Fatalf("result: %v", res)
	}
	if f.model != "haiku" {
		t.Fatalf("model %q", f.model)
	}
	if f.sessions[2].Title != canary.TitlePrefix+"2026-11-12" {
		t.Fatalf("title %q", f.sessions[2].Title)
	}
	if !contains(f.calls, "POST /sessions/s-old/archive") || contains(f.calls, "POST /sessions/s-other/archive") {
		t.Fatalf("only yesterday's canary is archived: %v", f.calls)
	}
}

func TestLaterRunSendsOneWordToTodaysSession(t *testing.T) {
	f := &fakeAPI{reply: true, sendStatus: "ok", sessions: []api.Session{{Id: "s-today", Title: canary.TitlePrefix + "2026-11-12"}}}
	res := run(t, f)
	if !res.OK() || res.Session != "s-today" {
		t.Fatalf("result: %v", res)
	}
	if contains(f.calls, "POST /sessions") || f.model != "" {
		t.Fatalf("must reuse the session: %v", f.calls)
	}
}

func TestReportsTheFailedStep(t *testing.T) {
	cases := []struct {
		name string
		f    *fakeAPI
		step string
	}{
		{"api down", &fakeAPI{listStatus: http.StatusBadGateway}, canary.StepList},
		{"turn still open", &fakeAPI{sendStatus: "queued", sessions: []api.Session{{Id: "s", Title: canary.TitlePrefix + "2026-11-12"}}}, canary.StepSend},
		{"no reply", &fakeAPI{sendStatus: "ok", sessions: []api.Session{{Id: "s", Title: canary.TitlePrefix + "2026-11-12"}}}, canary.StepReply},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := run(t, c.f)
			if res.Err == nil || res.Step != c.step {
				t.Fatalf("got %v, want step %s", res, c.step)
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

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
