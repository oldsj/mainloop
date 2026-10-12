package sessions_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	"github.com/oldsj/mainloop/server/internal/sessions"
)

func (e *env) API() *sessions.SessionsAPI { return &sessions.SessionsAPI{Svc: e.svc} }

func (e *env) counts() (sessions, bindings, deliveries, messages int) {
	e.t.Helper()
	err := e.pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM sessions), (SELECT count(*) FROM agent_bindings),
		(SELECT count(*) FROM deliveries), (SELECT count(*) FROM session_messages)`).
		Scan(&sessions, &bindings, &deliveries, &messages)
	if err != nil {
		e.t.Fatal(err)
	}
	return
}

func TestGetsAreReadsOnly(t *testing.T) {
	e := newEnv(t)
	a := e.API()
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `INSERT INTO topics (id, user_id, name, status_line) VALUES ('tp1', $1, 'Infra', 'quiet')`, owner); err != nil {
		t.Fatal(err)
	}

	resp, err := a.GetMainThread(ctx, api.GetMainThreadRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	info := api.MainThreadInfo(resp.(api.GetMainThread200JSONResponse))
	if info.Mode != "native" || info.Native != nil || info.SessionId != nil || info.ConversationId != nil {
		t.Fatalf("before ensure: %+v", info)
	}
	if len(info.Topics) != 1 || info.Topics[0].Name != "Infra" || info.Topics[0].StatusLine != "quiet" || info.Topics[0].Pending != 0 {
		t.Fatalf("topics %+v", info.Topics)
	}
	if _, err := a.GetConversation(ctx, api.GetConversationRequestObject{ConversationId: "nope"}); apierr.StatusOf(err) != 404 {
		t.Fatalf("got %v", err)
	}
	if s, b, d, m := e.counts(); s+b+d+m != 0 {
		t.Fatalf("GETs wrote rows: %d %d %d %d", s, b, d, m)
	}
	if e.srv.Count("CreateSession") != 0 {
		t.Fatal("a GET called kagent")
	}
}

func TestMainThreadShowsTheNativeSessionAndItsLedger(t *testing.T) {
	e := newEnv(t)
	a := e.API()
	ctx := context.Background()
	stop := e.run()
	id := e.send("hello")
	e.waitState(id, dbstate.DeliveryCompleted)
	stop()

	resp, err := a.GetMainThread(ctx, api.GetMainThreadRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	info := api.MainThreadInfo(resp.(api.GetMainThread200JSONResponse))
	n := info.Native
	if n == nil || info.SessionId == nil || *info.SessionId != n.SessionId || *info.ConversationId != n.SessionId {
		t.Fatalf("%+v", info)
	}
	if n.Kind != "claude" || n.Role != "main" || n.AgentName != "mainloop-main" || n.Turns != 1 || n.TurnInFlight ||
		n.KagentSessionId == nil || n.SessionState == nil || n.Note != nil {
		t.Fatalf("native %+v (state %v, note %v)", n, n.SessionState, n.Note)
	}
	if len(n.Deliveries) != 1 || n.Deliveries[0].MessageId != id || n.Deliveries[0].State != "completed" ||
		n.Deliveries[0].Source != "user" || n.Deliveries[0].TaskId == nil {
		t.Fatalf("deliveries %+v", n.Deliveries)
	}

	cr, err := a.GetConversation(ctx, api.GetConversationRequestObject{ConversationId: n.SessionId})
	if err != nil {
		t.Fatal(err)
	}
	c := api.ConversationResponse(cr.(api.GetConversation200JSONResponse))
	if c.Conversation.Id != n.SessionId || c.Conversation.MessageCount != 2 || c.Conversation.UserId != owner ||
		c.Conversation.Summary != nil || c.Conversation.SummarizedThroughId != nil ||
		c.Conversation.Title == nil || *c.Conversation.Title != "Main thread" || len(c.Messages) != 2 ||
		c.Messages[0].Role != api.MessageRoleUser || c.Messages[1].Role != api.MessageRoleAssistant ||
		c.Messages[1].ConversationId != n.SessionId {
		t.Fatalf("conversation %+v", c)
	}
}

func TestMainThreadSaysSoWhenADeliveryIsUnknown(t *testing.T) {
	e := newEnv(t)
	e.srv.Script(kagenttest.Drop{})
	e.run()
	id := e.send("hi")
	e.waitState(id, dbstate.DeliveryUncertain)
	resp, err := e.API().GetMainThread(context.Background(), api.GetMainThreadRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	n := resp.(api.GetMainThread200JSONResponse).Native
	if !n.TurnInFlight || n.Note == nil || !strings.HasPrefix(*n.Note, "delivery unknown:") || n.Deliveries[0].State != "uncertain" {
		t.Fatalf("%+v note %v", n, n.Note)
	}
}

func TestSendMessageStatuses(t *testing.T) {
	e := newEnv(t)
	a := e.API()
	ctx := context.Background()
	call := func(msg string, conv *string) (api.SendMessageResponseObject, error) {
		return a.SendMessage(ctx, api.SendMessageRequestObject{Body: &api.ChatRequest{Message: msg, ConversationId: conv}})
	}
	first, err := call("hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	ok, isOK := first.(api.SendMessage200JSONResponse)
	if !isOK || !ok.Pending || ok.Message != nil || ok.SpawnedSessionId != nil || ok.DeliveryMessageId == nil {
		t.Fatalf("%+v", ok)
	}
	if _, err := uuid.Parse(*ok.DeliveryMessageId); err != nil {
		t.Fatal(err)
	}
	if _, err := call("again", &ok.ConversationId); apierr.StatusOf(err) != 409 {
		t.Fatal("want 409 while the turn is open")
	}
	other := "elsewhere"
	if _, err := call("hello", &other); apierr.StatusOf(err) != 404 {
		t.Fatal("want 404 for another conversation")
	}
	if _, err := call("   ", nil); apierr.StatusOf(err) != 422 {
		t.Fatalf("want 422, got %v", err)
	}
}

// A message carries who wrote it, so a brief stays marked after the ledger (which shows only the
// newest deliveries) has moved past it.
func TestAMessageSaysWhoSentItForTheWholeHistory(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.run()
	main, err := e.svc.EnsureMain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	brief := uuid.NewString()
	if _, err := e.svc.SendAs(ctx, sessions.SourceMainloop, brief, "", "the brief"); err != nil {
		t.Fatal(err)
	}
	e.waitState(brief, dbstate.DeliveryCompleted)
	var last string
	for range 25 {
		last = e.send("owner words")
		e.waitState(last, dbstate.DeliveryCompleted)
	}

	resp, err := e.API().GetMainThread(ctx, api.GetMainThreadRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range api.MainThreadInfo(resp.(api.GetMainThread200JSONResponse)).Native.Deliveries {
		if d.MessageId == brief {
			t.Fatalf("the brief is still in the newest deliveries; the test no longer proves anything")
		}
	}

	cr, err := e.API().GetConversation(ctx, api.GetConversationRequestObject{ConversationId: main.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	var mainloop, user, assistant int
	for _, m := range cr.(api.GetConversation200JSONResponse).Messages {
		switch {
		case m.Role == api.MessageRoleAssistant:
			if m.Source != nil {
				t.Fatalf("an assistant message has source %q", *m.Source)
			}
			assistant++
		case m.Id == brief:
			if m.Source == nil || *m.Source != api.MessageSourceMainloop {
				t.Fatalf("the brief has source %v, want mainloop", m.Source)
			}
			mainloop++
		default:
			if m.Source == nil || *m.Source != api.MessageSourceUser {
				t.Fatalf("an owner message has source %v, want user", m.Source)
			}
			user++
		}
	}
	if mainloop != 1 || user != 25 || assistant != 26 {
		t.Fatalf("%d brief, %d owner, %d assistant messages; want 1, 25, 26", mainloop, user, assistant)
	}
}

// The same holds for a session's own conversation, and for the source the delegated brief and
// task reports are sent with.
func TestASessionConversationMarksMessagesMainloopSent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.withProviders()
	e.run()
	sid := e.newSession("start")
	e.waitState(e.firstDelivery(sid), dbstate.DeliveryCompleted)
	brief, owned := uuid.NewString(), uuid.NewString()
	if _, err := e.svc.SendToAs(ctx, sessions.SourceMainloop, sid, brief, "do the thing"); err != nil {
		t.Fatal(err)
	}
	e.waitState(brief, dbstate.DeliveryCompleted)
	if _, err := e.svc.SendTo(ctx, sid, owned, "also this"); err != nil {
		t.Fatal(err)
	}
	e.waitState(owned, dbstate.DeliveryCompleted)

	conv, err := e.API().GetSessionConversation(ctx, api.GetSessionConversationRequestObject{SessionId: sid})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]api.MessageSource{}
	for _, m := range conv.(api.GetSessionConversation200JSONResponse).Messages {
		if m.Source != nil {
			got[m.Id] = *m.Source
		}
	}
	if len(got) != 3 || got[e.firstDelivery(sid)] != api.MessageSourceUser || got[brief] != api.MessageSourceMainloop || got[owned] != api.MessageSourceUser {
		t.Fatalf("sources %v; want the prompt and %s user, %s mainloop", got, owned, brief)
	}
}
