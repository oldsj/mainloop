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
		if f.createStatus != 0 {
			w.WriteHeader(f.createStatus)
			return
		}
		f.model, _ = body["model"].(string)
		s := api.Session{Id: "s-new", Title: body["title"].(string), Status: api.SessionStatusActive}
		f.sessions = append(f.sessions, s)
		userMsg("first", s.Id)
		write(s)
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

func TestEachRunUsesAFreshSmallModelSessionAndRetiresIt(t *testing.T) {
	f := &fakeAPI{reply: true, sessions: []api.Session{
		{Id: "s-left", Title: canary.TitlePrefix + "2026-11-12 09:50"},
		{Id: "s-other", Title: "Real work"},
	}, ended: map[string]bool{"s-left": true}}
	res := run(t, f)
	if !res.OK() || res.Session != "s-new" {
		t.Fatalf("result: %v", res)
	}
	if f.model != "claude-haiku-5-5" {
		t.Fatalf("model must be set at create: %q", f.model)
	}
	if f.sessions[2].Title != canary.TitlePrefix+"2026-11-12 10:00" {
		t.Fatalf("title %q", f.sessions[2].Title)
	}
	if !slices.Equal(f.archived, []string{"s-left", "s-new"}) {
		t.Fatalf("a left-over and this run's session are archived, nothing else: %v", f.archived)
	}
}

func TestReportsTheFailedStepAndStillRetires(t *testing.T) {
	cases := []struct {
		name     string
		f        *fakeAPI
		step     string
		archived []string
	}{
		{"api down", &fakeAPI{listStatus: http.StatusBadGateway}, canary.StepList, nil},
		{"create refused", &fakeAPI{createStatus: http.StatusUnprocessableEntity}, canary.StepCreate, nil},
		{"no reply", &fakeAPI{}, canary.StepReply, []string{"s-new"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := run(t, c.f)
			if res.Err == nil || res.Step != c.step {
				t.Fatalf("got %v, want step %s", res, c.step)
			}
			if !slices.Equal(c.f.archived, c.archived) {
				t.Fatalf("archived %v, want %v", c.f.archived, c.archived)
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
