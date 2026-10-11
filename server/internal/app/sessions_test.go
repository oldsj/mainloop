package app_test

import (
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/app"
	"github.com/oldsj/mainloop/server/internal/app/apicheck"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func sessionsServer(t *testing.T) http.Handler {
	t.Helper()
	cfg, err := config.Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": "mainloop.test", "MAINLOOP_API_HOSTS": "api.test"})
	if err != nil {
		t.Fatal(err)
	}
	h, err := app.New(app.Deps{
		Config: cfg, Log: slog.New(slog.NewJSONHandler(io.Discard, nil)), DB: up(), Pool: testdb.New(t).Pool,
	}).Handler()
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

	// The turn is open (no delivery loop runs here), so the next message is refused.
	r = send(h, "POST", "/chat", `{"message":"again","conversation_id":"`+chat.ConversationId+`"}`, nil)
	if r.status != 409 {
		t.Fatalf("%d %s", r.status, r.body)
	}
	var he api.HTTPError
	strict(t, r.body, &he)

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
	if info.Native == nil || !info.Native.TurnInFlight || len(info.Native.Deliveries) != 1 || info.Native.Deliveries[0].State != "recorded" {
		t.Fatalf("%s", r.body)
	}

	r = send(h, "GET", "/conversations/"+chat.ConversationId, "", nil)
	if r.status != 200 {
		t.Fatalf("%d %s", r.status, r.body)
	}
	var conv api.ConversationResponse
	strict(t, r.body, &conv)
	if len(conv.Messages) != 1 || conv.Messages[0].Role != api.MessageRoleUser || conv.Messages[0].Content != "hello" {
		t.Fatalf("%s", r.body)
	}
	if r = send(h, "GET", "/conversations/missing", "", nil); r.status != 404 {
		t.Fatalf("%d %s", r.status, r.body)
	}
}
