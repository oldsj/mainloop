package app_test

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/app"
	"github.com/oldsj/mainloop/server/internal/app/apicheck"
	"github.com/oldsj/mainloop/server/internal/app/apptest"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	"github.com/oldsj/mainloop/server/internal/sessions"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func sessionsServer(t *testing.T) http.Handler {
	t.Helper()
	cfg, err := config.Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": "mainloop.test", "MAINLOOP_API_HOSTS": "api.test"})
	if err != nil {
		t.Fatal(err)
	}
	d := apptest.Deps(t, cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)), testdb.New(t).Pool)
	d.DB = up()
	h, err := app.New(d).Handler()
	if err != nil {
		t.Fatal(err)
	}
	return apicheck.Wrap(t, h)
}

func TestChatAndMainThreadOverHTTP(t *testing.T) {
	h := sessionsServer(t)

	r := send(h, "GET", "/main-thread", "", nil)
	var info api.MainThreadInfo
	if r.status != 200 {
		t.Fatalf("%d %s", r.status, r.body)
	}
	strict(t, r.body, &info)
	if info.Mode != "native" || info.Native != nil || info.SessionId != nil {
		t.Fatalf("before the first message: %s", r.body)
	}

	r = send(h, "POST", "/chat", `{"message":"hello"}`, nil)
	if r.status != 200 {
		t.Fatalf("%d %s", r.status, r.body)
	}
	var chat api.ChatResponse
	strict(t, r.body, &chat)
	if !chat.Pending || chat.DeliveryMessageId == nil || chat.Message != nil || chat.SpawnedSessionId != nil || chat.ConversationId == "" {
		t.Fatalf("%s", r.body)
	}

	// The turn is open (no delivery loop runs here), so the next message is queued behind it.
	r = send(h, "POST", "/chat", `{"message":"again","conversation_id":"`+chat.ConversationId+`"}`, nil)
	if r.status != 202 {
		t.Fatalf("%d %s", r.status, r.body)
	}
	var queued api.ChatResponse
	strict(t, r.body, &queued)
	if queued.Status != api.ChatResponseStatusQueued || !queued.Pending || queued.DeliveryMessageId == nil {
		t.Fatalf("%s", r.body)
	}

	r = send(h, "POST", "/chat", `{"message":"x","conversation_id":"missing"}`, nil)
	if r.status != 404 {
		t.Fatalf("%d %s", r.status, r.body)
	}
	r = send(h, "POST", "/chat", `{"message":"   "}`, nil)
	if r.status != 422 {
		t.Fatalf("%d %s", r.status, r.body)
	}
	var ve api.HTTPValidationError
	strict(t, r.body, &ve)

	r = send(h, "GET", "/main-thread", "", nil)
	strict(t, r.body, &info)
	if info.Native == nil || !info.Native.TurnInFlight || len(info.Native.Deliveries) != 2 ||
		info.Native.Deliveries[0].State != "recorded" || info.Native.Deliveries[1].State != "queued" {
		t.Fatalf("%s", r.body)
	}

	r = send(h, "GET", "/conversations/"+chat.ConversationId, "", nil)
	if r.status != 200 {
		t.Fatalf("%d %s", r.status, r.body)
	}
	var conv api.ConversationResponse
	strict(t, r.body, &conv)
	if len(conv.Messages) != 2 || conv.Messages[0].Role != api.MessageRoleUser || conv.Messages[0].Content != "hello" ||
		conv.Messages[1].Role != api.MessageRoleUser || conv.Messages[1].Content != "again" {
		t.Fatalf("%s", r.body)
	}
	if r = send(h, "GET", "/conversations/missing", "", nil); r.status != 404 {
		t.Fatalf("%d %s", r.status, r.body)
	}
}

func providerServer(t *testing.T, withKagent bool) http.Handler {
	t.Helper()
	cfg, err := config.Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": "mainloop.test", "MAINLOOP_API_HOSTS": "api.test"})
	if err != nil {
		t.Fatal(err)
	}
	var k sessions.Kagent
	if withKagent {
		srv := kagenttest.New()
		t.Cleanup(srv.Close)
		srv.AddAgent(cfg.Kagent.Namespace, cfg.Kagent.MainAgent)
		srv.AddAgent(cfg.Kagent.Namespace, cfg.Kagent.MainCodexAgent)
		c, err := kagent.New(kagent.Config{GatewayURL: srv.URL, UserID: "mainloop"})
		if err != nil {
			t.Fatal(err)
		}
		k = c
	}
	pool := testdb.New(t).Pool
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	d := apptest.Deps(t, cfg, log, pool, apptest.WithKagent(k))
	d.DB = up()
	h, err := app.New(d).Handler()
	if err != nil {
		t.Fatal(err)
	}
	return apicheck.Wrap(t, h)
}

func TestMainProviderOverHTTP(t *testing.T) {
	h := providerServer(t, true)

	var p api.MainProviderInfo
	r := send(h, "GET", "/main-thread/provider", "", nil)
	if r.status != 200 {
		t.Fatalf("%d %s", r.status, r.body)
	}
	strict(t, r.body, &p)
	if p.DesiredProvider != api.MainProviderClaude || p.Status != api.MainProviderInfoStatusNotStarted || p.EffectiveProvider != nil || len(p.History) != 0 {
		t.Fatalf("before any use: %s", r.body)
	}

	r = send(h, "PUT", "/main-thread/provider", `{"provider":"codex"}`, nil)
	if r.status != 200 {
		t.Fatalf("%d %s", r.status, r.body)
	}
	strict(t, r.body, &p)
	// A main that never started has no old Session to keep, so the switch records no history.
	if p.DesiredProvider != api.MainProviderCodex || p.Status != api.MainProviderInfoStatusReady || p.EffectiveProvider == nil ||
		*p.EffectiveProvider != api.MainProviderCodex || p.KagentSessionId == nil || len(p.History) != 0 || p.Recoverable {
		t.Fatalf("after the switch: %s", r.body)
	}
	// The same request again changes nothing.
	again := send(h, "PUT", "/main-thread/provider", `{"provider":"codex","restart_failed":false}`, nil)
	if again.status != 200 || again.body != r.body {
		t.Fatalf("repeat: %d %s", again.status, again.body)
	}

	// The provider state is on the main-thread view too.
	r = send(h, "GET", "/main-thread", "", nil)
	var info api.MainThreadInfo
	strict(t, r.body, &info)
	if info.Provider == nil || info.Provider.DesiredProvider != api.MainProviderCodex || info.Native == nil || info.Native.AgentName != "mainloop-main-codex" {
		t.Fatalf("%s", r.body)
	}

	// A message makes the main busy: the next switch is refused and nothing changes.
	if r = send(h, "POST", "/chat", `{"message":"hello"}`, nil); r.status != 200 {
		t.Fatalf("%d %s", r.status, r.body)
	}
	r = send(h, "PUT", "/main-thread/provider", `{"provider":"claude"}`, nil)
	if r.status != 409 {
		t.Fatalf("%d %s", r.status, r.body)
	}
	var coded api.CodedHTTPError
	strict(t, r.body, &coded)
	if coded.Code == nil || *coded.Code != api.CodedHTTPErrorCodeBusy {
		t.Fatalf("code = %v, want busy: %s", coded.Code, r.body)
	}
	r = send(h, "GET", "/main-thread/provider", "", nil)
	strict(t, r.body, &p)
	if p.DesiredProvider != api.MainProviderCodex || len(p.History) != 0 {
		t.Fatalf("a refused switch changed the state: %s", r.body)
	}
}

func TestMainProviderRefusals(t *testing.T) {
	h := providerServer(t, true)
	for name, tc := range map[string]struct {
		body    string
		headers map[string]string
		status  int
	}{
		"unknown provider":  {`{"provider":"gemini"}`, nil, 422},
		"missing provider":  {`{}`, nil, 422},
		"unknown field":     {`{"provider":"codex","force":true}`, nil, 422},
		"foreign origin":    {`{"provider":"codex"}`, map[string]string{"Origin": "https://evil.example"}, 403},
		"frontend origin":   {`{"provider":"codex"}`, map[string]string{"Origin": "https://mainloop.test"}, 200},
		"oversized request": {`{"provider":"codex","pad":"` + strings.Repeat("x", 5<<20) + `"}`, nil, 413},
	} {
		t.Run(name, func(t *testing.T) {
			r := send(h, "PUT", "/main-thread/provider", tc.body, tc.headers)
			if r.status != tc.status {
				t.Fatalf("%d %.200s", r.status, r.body)
			}
		})
	}
}

func TestMainProviderWithoutKagentIsUnavailable(t *testing.T) {
	h := providerServer(t, false)
	if r := send(h, "PUT", "/main-thread/provider", `{"provider":"codex"}`, nil); r.status != 503 {
		t.Fatalf("%d %s", r.status, r.body)
	}
	// Reading the state needs no kagent.
	r := send(h, "GET", "/main-thread/provider", "", nil)
	if r.status != 200 {
		t.Fatalf("%d %s", r.status, r.body)
	}
}
