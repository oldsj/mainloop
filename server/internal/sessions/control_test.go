package sessions_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/apierr"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	"github.com/oldsj/mainloop/server/internal/providers"
	"github.com/oldsj/mainloop/server/internal/sessions"
)

// withProviders gives the service the built-in provider profiles and makes the fake kagent know
// every Agent they name.
func (e *env) withProviders() {
	e.t.Helper()
	cfg, err := config.Parse(map[string]string{"AGENT_TOKEN_KEY": "k"})
	if err != nil {
		e.t.Fatal(err)
	}
	reg, err := providers.NewRegistry(cfg.Kagent)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, p := range reg.Profiles() {
		for _, a := range p.Agents {
			e.srv.AddAgent(a.Namespace, a.Name)
		}
	}
	e.svc.Providers = reg
}

// newSession creates a native session through the service.
func (e *env) newSession(prompt string) string {
	e.t.Helper()
	id, err := e.svc.Create(context.Background(), sessions.NewSession{Title: "Fix the thing", Description: "d", Prompt: prompt})
	if err != nil {
		e.t.Fatalf("create: %v", err)
	}
	return id
}

// firstDelivery is the id of the session's first delivery (its prompt).
func (e *env) firstDelivery(sessionID string) string {
	e.t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT id FROM deliveries WHERE session_id = $1 ORDER BY created_at, id LIMIT 1`, sessionID).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) sessionStatus(id string) dbstate.SessionStatus {
	e.t.Helper()
	var st dbstate.SessionStatus
	if err := e.pool.QueryRow(context.Background(), `SELECT status FROM sessions WHERE id = $1`, id).Scan(&st); err != nil {
		e.t.Fatal(err)
	}
	return st
}

func (e *env) queueHeld(sessionID string) bool {
	e.t.Helper()
	var held bool
	if err := e.pool.QueryRow(context.Background(),
		`SELECT queue_held FROM agent_bindings WHERE session_id = $1`, sessionID).Scan(&held); err != nil {
		e.t.Fatal(err)
	}
	return held
}

func (e *env) sessionMessages(sessionID string) []dbq.ListSessionMessagesRow {
	e.t.Helper()
	rows, err := dbq.New(e.pool).ListSessionMessages(context.Background(), sessionID)
	if err != nil {
		e.t.Fatal(err)
	}
	return rows
}

// runningTask is a task that is still working, with some of the reply already written.
func runningTask(id, contextID, messageID, partial string) *a2a.Task {
	user := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello"))
	user.ID, user.ContextID, user.TaskID = messageID, contextID, a2a.TaskID(id)
	return &a2a.Task{
		ID: a2a.TaskID(id), ContextID: contextID, History: []*a2a.Message{user},
		Artifacts: []*a2a.Artifact{{ID: "a1", Parts: a2a.ContentParts{a2a.NewTextPart(partial)}}},
		Status:    a2a.TaskStatus{State: a2a.TaskStateWorking},
	}
}

// openTurn starts a turn that stays open: the task is working and the stream never ends.
func (e *env) openTurn(sessionID, text string) (messageID string) {
	e.t.Helper()
	id := uuid.NewString()
	task := runningTask("t-open", e.bindingKsid(sessionID), id, "half of the answ")
	e.srv.AddTask(task)
	e.srv.Script(kagenttest.Emit{Event: task}, kagenttest.Hang{})
	if _, err := e.svc.SendTo(context.Background(), sessionID, id, text); err != nil {
		e.t.Fatalf("send: %v", err)
	}
	e.waitState(id, dbstate.DeliveryDelivered)
	return id
}

// ---- create, list, get, conversation, native --------------------------------------------------------

func TestACreatedSessionSendsItsPromptAndShowsOnEveryRoute(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	a := e.API()
	ctx := context.Background()
	e.run()

	kind := "codex"
	resp, err := a.CreateSession(ctx, api.CreateSessionRequestObject{Body: &api.SessionCreate{
		Title: "Fix the thing", Description: "why", Prompt: "do it", AgentKind: &kind,
	}})
	if err != nil {
		t.Fatal(err)
	}
	created := api.Session(resp.(api.CreateSession200JSONResponse))
	if created.Title != "Fix the thing" || created.Status != api.SessionStatus(dbstate.SessionActive) ||
		created.AgentKind == nil || *created.AgentKind != "codex" || created.Color == nil || created.ArchivedAt != nil {
		t.Fatalf("created %+v", created)
	}
	did := e.firstDelivery(created.Id)
	d := e.waitState(did, dbstate.DeliveryCompleted)
	if d.SessionID != created.Id {
		t.Fatalf("delivery %+v", d)
	}
	// The prompt went to the Agent the codex profile names for a workspace-less session.
	if e.sends() != 1 || e.srv.Count(kagent.ProcCreateSession) != 1 {
		t.Fatalf("%d sends, %d creates", e.sends(), e.srv.Count(kagent.ProcCreateSession))
	}

	list, err := a.ListSessions(ctx, api.ListSessionsRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	rows := list.(api.ListSessions200JSONResponse)
	if len(rows) != 1 || rows[0].Id != created.Id {
		t.Fatalf("list %+v", rows)
	}
	other := "failed"
	none, err := a.ListSessions(ctx, api.ListSessionsRequestObject{Params: api.ListSessionsParams{Status: &other}})
	if err != nil || len(none.(api.ListSessions200JSONResponse)) != 0 {
		t.Fatalf("filtered list %v %v", none, err)
	}

	got, err := a.GetSession(ctx, api.GetSessionRequestObject{SessionId: created.Id})
	if err != nil || api.Session(got.(api.GetSession200JSONResponse)).Id != created.Id {
		t.Fatalf("get %v %v", got, err)
	}
	conv, err := a.GetSessionConversation(ctx, api.GetSessionConversationRequestObject{SessionId: created.Id})
	if err != nil {
		t.Fatal(err)
	}
	c := api.SessionConversationResponse(conv.(api.GetSessionConversation200JSONResponse))
	if c.Session.Id != created.Id || len(c.Messages) != 2 || c.Messages[0].Content != "do it" ||
		c.Messages[1].Role != api.MessageRoleAssistant {
		t.Fatalf("conversation %+v", c)
	}
	nat, err := a.GetSessionNative(ctx, api.GetSessionNativeRequestObject{SessionId: created.Id})
	if err != nil {
		t.Fatal(err)
	}
	n := api.NativeSessionInfo(nat.(api.GetSessionNative200JSONResponse))
	if n.Kind != "codex" || n.Role != "agent" || n.Turns != 1 || n.TurnInFlight || n.KagentSessionId == nil ||
		n.AgentName == "" || len(n.Deliveries) != 1 {
		t.Fatalf("native %+v", n)
	}

	// The main thread is not listed.
	e.send("hi")
	list, _ = a.ListSessions(ctx, api.ListSessionsRequestObject{})
	if len(list.(api.ListSessions200JSONResponse)) != 1 {
		t.Fatalf("main thread listed: %+v", list)
	}
}

func TestSessionRoutesRefuseWhatIsNotTheOwners(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	a := e.API()
	ctx := context.Background()

	bad := "no-such-provider"
	if _, err := a.CreateSession(ctx, api.CreateSessionRequestObject{Body: &api.SessionCreate{Title: "t", Prompt: "p", AgentKind: &bad}}); apierr.StatusOf(err) != 422 {
		t.Fatalf("unknown provider: %v", err)
	}
	if _, err := a.CreateSession(ctx, api.CreateSessionRequestObject{Body: &api.SessionCreate{Title: "t", Prompt: "  "}}); apierr.StatusOf(err) != 422 {
		t.Fatalf("blank prompt: %v", err)
	}
	if s, _, _, _ := e.counts(); s != 0 {
		t.Fatalf("%d sessions after refused creates", s)
	}

	const gone = "nope"
	if _, err := a.GetSession(ctx, api.GetSessionRequestObject{SessionId: gone}); apierr.StatusOf(err) != 404 {
		t.Fatalf("get: %v", err)
	}
	if _, err := a.GetSessionConversation(ctx, api.GetSessionConversationRequestObject{SessionId: gone}); apierr.StatusOf(err) != 404 {
		t.Fatalf("conversation: %v", err)
	}
	if _, err := a.GetSessionNative(ctx, api.GetSessionNativeRequestObject{SessionId: gone}); apierr.StatusOf(err) != 404 {
		t.Fatalf("native: %v", err)
	}
	if _, err := a.SendSessionMessage(ctx, api.SendSessionMessageRequestObject{SessionId: gone, Body: &api.SessionMessageRequest{Message: "x"}}); apierr.StatusOf(err) != 404 {
		t.Fatalf("message: %v", err)
	}
	if _, err := a.StopTurn(ctx, api.StopTurnRequestObject{SessionId: gone}); apierr.StatusOf(err) != 404 {
		t.Fatalf("stop: %v", err)
	}
	if _, err := a.CancelSession(ctx, api.CancelSessionRequestObject{SessionId: gone}); apierr.StatusOf(err) != 404 {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := a.ArchiveSession(ctx, api.ArchiveSessionRequestObject{SessionId: gone}); apierr.StatusOf(err) != 404 {
		t.Fatalf("archive: %v", err)
	}
}

// ---- message: one open turn, ended and archived sessions ---------------------------------------------

func TestOneOpenTurnPerSessionAndASecondMessageIsRefused(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	a := e.API()
	ctx := context.Background()
	id := e.newSession("first")
	e.run()
	e.waitState(e.firstDelivery(id), dbstate.DeliveryCompleted)
	e.openTurn(id, "second")

	_, err := a.SendSessionMessage(ctx, api.SendSessionMessageRequestObject{SessionId: id, Body: &api.SessionMessageRequest{Message: "third"}})
	if busy, ok := apierr.As(err); !ok || busy.Status != 409 || !strings.Contains(busy.Message, "turn is already in progress") {
		t.Fatalf("got %v", err)
	}
	var open int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE session_id = $1 AND state IN ('recorded','sending','delivered')`, id).Scan(&open); err != nil || open != 1 {
		t.Fatalf("%d open turns (%v)", open, err)
	}

	if _, err := a.SendSessionMessage(ctx, api.SendSessionMessageRequestObject{SessionId: id, Body: &api.SessionMessageRequest{Message: ""}}); apierr.StatusOf(err) != 422 {
		t.Fatalf("empty message: %v", err)
	}
	if _, err := a.SendSessionMessage(ctx, api.SendSessionMessageRequestObject{SessionId: id}); apierr.StatusOf(err) != 422 {
		t.Fatalf("no body: %v", err)
	}
}

func TestAMessageToAnIdleSessionIsSentOnce(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	ctx := context.Background()
	id := e.newSession("first")
	e.run()
	e.waitState(e.firstDelivery(id), dbstate.DeliveryCompleted)

	r, err := e.API().SendSessionMessage(ctx, api.SendSessionMessageRequestObject{SessionId: id, Body: &api.SessionMessageRequest{Message: "more"}})
	if err != nil {
		t.Fatal(err)
	}
	ok, isOK := r.(api.SendSessionMessage200JSONResponse)
	if !isOK || ok.Status != api.SessionMessageResponseStatusOk {
		t.Fatalf("got %T", r)
	}
	e.waitState(ok.MessageId, dbstate.DeliveryCompleted)
	if e.sends() != 2 || e.srv.Count(kagent.ProcCreateSession) != 1 {
		t.Fatalf("%d sends, %d creates", e.sends(), e.srv.Count(kagent.ProcCreateSession))
	}
}

func TestACancelledOrArchivedSessionTakesNoMessage(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	a := e.API()
	ctx := context.Background()
	id := e.newSession("first")
	e.run()
	e.waitState(e.firstDelivery(id), dbstate.DeliveryCompleted)

	if _, err := e.svc.Cancel(ctx, id); err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{"cancelled", "archived"} {
		if step == "archived" {
			if err := e.svc.Archive(ctx, id); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := a.SendSessionMessage(ctx, api.SendSessionMessageRequestObject{SessionId: id, Body: &api.SessionMessageRequest{Message: "more"}}); apierr.StatusOf(err) != 409 {
			t.Fatalf("%s: got %v", step, err)
		}
	}
	if e.sends() != 1 {
		t.Fatalf("%d sends", e.sends())
	}
}

// ---- stop-turn ------------------------------------------------------------------------------------------

func TestStopTurnCancelsTheTaskAndEndsTheTurnWithItsPartialReply(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	ctx := context.Background()
	id := e.newSession("first")
	e.run()
	e.waitState(e.firstDelivery(id), dbstate.DeliveryCompleted)
	mid := e.openTurn(id, "write a long answer")

	r, err := e.API().StopTurn(ctx, api.StopTurnRequestObject{SessionId: id})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := r.(api.StopTurn200JSONResponse); !ok || got.Status != api.SessionStopTurnResponseStatus(sessions.Stopped) {
		t.Fatalf("got %T %+v", r, r)
	}
	d := e.waitState(mid, dbstate.DeliveryCancelled)
	if task, ok := e.srv.Task("t-open"); !ok || task.Status.State != a2a.TaskStateCanceled {
		t.Fatalf("task %+v", task)
	}
	if e.srv.Count("CancelTask") != 1 {
		t.Fatalf("%d cancels", e.srv.Count("CancelTask"))
	}
	if d.TaskID == nil || *d.TaskID != "t-open" {
		t.Fatalf("delivery %+v", d)
	}
	// The partial reply and the stop note are one message; the queue is held.
	msgs := e.sessionMessages(id)
	last := msgs[len(msgs)-1]
	if last.Role != "assistant" || !strings.Contains(last.Content, "half of the answ") ||
		!strings.HasSuffix(last.Content, sessions.TurnStoppedNote) {
		t.Fatalf("last message %q", last.Content)
	}
	if !e.queueHeld(id) {
		t.Fatal("the queue is not held")
	}
	// The loop sees the same cancellation and does not write a second note.
	time.Sleep(150 * time.Millisecond)
	if got := len(e.sessionMessages(id)); got != len(msgs) {
		t.Fatalf("%d messages after the loop caught up, %d before", got, len(msgs))
	}
	// And the session takes another message; nothing was sent twice.
	if e.sends() != 2 {
		t.Fatalf("%d sends", e.sends())
	}
}

func TestStopTurnWithNothingOpen(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	id := e.newSession("first")
	e.run()
	e.waitState(e.firstDelivery(id), dbstate.DeliveryCompleted)

	out, err := e.svc.Stop(context.Background(), id)
	if err != nil || out != sessions.NoOpenTurn {
		t.Fatalf("%v %v", out, err)
	}
	if e.srv.Count("CancelTask") != 0 {
		t.Fatal("cancelled a task that was not running")
	}
}

func TestStopTurnWhenKagentFailsLeavesTheTurnOpen(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	ctx := context.Background()
	id := e.newSession("first")
	e.run()
	e.waitState(e.firstDelivery(id), dbstate.DeliveryCompleted)
	mid := e.openTurn(id, "long")

	e.srv.FailNext("CancelTask", kagenttest.Fault{RPC: &kagenttest.RPCError{Code: -32603, Message: "boom"}})
	_, err := e.API().StopTurn(ctx, api.StopTurnRequestObject{SessionId: id})
	if bad, ok := apierr.As(err); !ok || bad.Status != 502 || !strings.HasPrefix(bad.Message, "Could not stop the turn") {
		t.Fatalf("got %v", err)
	}
	if d := e.delivery(mid); d.State != dbstate.DeliveryDelivered || e.queueHeld(id) {
		t.Fatalf("delivery %s, held %v: a failed stop wrote something", d.State, e.queueHeld(id))
	}
	// Trying again works.
	out, err := e.svc.Stop(ctx, id)
	if err != nil || out != sessions.Stopped {
		t.Fatalf("retry: %v %v", out, err)
	}
	e.waitState(mid, dbstate.DeliveryCancelled)
}

func TestStopTurnIsRefusedWhileKagentHasNoTaskOrKeepsRunningIt(t *testing.T) {
	t.Run("no task yet", func(t *testing.T) {
		e := newEnv(t)
		e.withProviders()
		id := e.newSession("first")
		e.run()
		e.waitState(e.firstDelivery(id), dbstate.DeliveryCompleted)
		// A message was sent and kagent shows no task for it: it may still land.
		mid := e.leftBehind2(id, dbstate.DeliverySending, "")
		_, err := e.svc.Stop(context.Background(), id)
		var un *sessions.UnconfirmedError
		if !errors.As(err, &un) {
			t.Fatalf("got %v", err)
		}
		if e.delivery(mid).State != dbstate.DeliverySending || e.srv.Count("CancelTask") != 0 {
			t.Fatal("a stop with no task changed something")
		}
		if _, err := e.API().StopTurn(context.Background(), api.StopTurnRequestObject{SessionId: id}); apierr.StatusOf(err) != 409 {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("still running after the cancel", func(t *testing.T) {
		e := newEnv(t)
		e.withProviders()
		id := e.newSession("first")
		e.run()
		e.waitState(e.firstDelivery(id), dbstate.DeliveryCompleted)
		mid := e.openTurn(id, "long")
		e.srv.TamperTask = func(proc string, task *a2a.Task) {
			if proc == "CancelTask" {
				task.Status.State = a2a.TaskStateWorking
			}
		}
		_, err := e.svc.Stop(context.Background(), id)
		var un *sessions.UnconfirmedError
		if !errors.As(err, &un) {
			t.Fatalf("got %v", err)
		}
		if e.delivery(mid).State != dbstate.DeliveryDelivered || e.queueHeld(id) {
			t.Fatal("an unconfirmed stop changed the delivery")
		}
	})
}

func TestStopTurnForATurnThatFinishedRecordsItAsFinished(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	ctx := context.Background()
	id := e.newSession("first")
	e.run()
	e.waitState(e.firstDelivery(id), dbstate.DeliveryCompleted)
	mid := e.openTurn(id, "long")
	// The task completes in kagent; the loop has not noticed yet because the stop wins the race.
	done := completedTask("t-open", "", mid, "the full answer")
	e.srv.AddTask(done)

	out, err := e.svc.Stop(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if out != sessions.Finished && out != sessions.Stopped {
		t.Fatalf("outcome %s", out)
	}
	d := e.waitState(mid, dbstate.DeliveryCompleted)
	if out == sessions.Finished && d.State != dbstate.DeliveryCompleted {
		t.Fatalf("state %s", d.State)
	}
	msgs := e.sessionMessages(id)
	if last := msgs[len(msgs)-1]; last.Content != "the full answer" {
		t.Fatalf("last message %q", last.Content)
	}
	if e.queueHeld(id) {
		t.Fatal("a finished turn held the queue")
	}
}

func TestStopTurnForAMessageNeverSentIsCancelledWithoutACall(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	id := e.newSession("first") // no delivery loop: the prompt stays recorded
	did := e.firstDelivery(id)

	out, err := e.svc.Stop(context.Background(), id)
	if err != nil || out != sessions.Stopped {
		t.Fatalf("%v %v", out, err)
	}
	if d := e.delivery(did); d.State != dbstate.DeliveryCancelled {
		t.Fatalf("state %s", d.State)
	}
	if e.srv.Count("CancelTask") != 0 || e.sends() != 0 {
		t.Fatal("kagent was called for a message it never saw")
	}
	// The loop never sends it afterwards.
	e.run()
	time.Sleep(150 * time.Millisecond)
	if e.sends() != 0 {
		t.Fatal("a cancelled message was sent")
	}
}

// ---- cancel and archive ------------------------------------------------------------------------------------

func TestCancelEndsTheSessionAndStopsItsTask(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	a := e.API()
	ctx := context.Background()
	id := e.newSession("first")
	e.run()
	e.waitState(e.firstDelivery(id), dbstate.DeliveryCompleted)
	mid := e.openTurn(id, "long")

	r, err := a.CancelSession(ctx, api.CancelSessionRequestObject{SessionId: id})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := r.(api.CancelSession200JSONResponse)
	if !ok || got.Status != api.SessionCancelResponseStatus(dbstate.SessionCancelled) || got.Agent != api.SessionCancelResponseAgent(sessions.AgentStopped) {
		t.Fatalf("got %T %+v", r, r)
	}
	if e.sessionStatus(id) != dbstate.SessionCancelled {
		t.Fatalf("status %s", e.sessionStatus(id))
	}
	if d := e.waitState(mid, dbstate.DeliveryCancelled); d.State != dbstate.DeliveryCancelled {
		t.Fatalf("state %s", d.State)
	}
	if task, _ := e.srv.Task("t-open"); task.Status.State != a2a.TaskStateCanceled {
		t.Fatalf("task %s", task.Status.State)
	}
	// Cancelling again changes nothing and calls nothing.
	n := e.srv.Count("CancelTask")
	again, err := e.svc.Cancel(ctx, id)
	if err != nil || again.Agent != sessions.AgentStopped || again.Status != dbstate.SessionCancelled || e.srv.Count("CancelTask") != n {
		t.Fatalf("second cancel %+v %v", again, err)
	}
}

func TestCancelReportsUnknownWhenKagentDoesNotConfirm(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	ctx := context.Background()
	id := e.newSession("first")
	e.run()
	e.waitState(e.firstDelivery(id), dbstate.DeliveryCompleted)
	e.openTurn(id, "long")

	e.srv.FailNext("CancelTask", kagenttest.Fault{RPC: &kagenttest.RPCError{Code: -32603, Message: "boom"}})
	res, err := e.svc.Cancel(ctx, id)
	if err != nil || res.Agent != sessions.AgentUnknown || res.Status != dbstate.SessionCancelled {
		t.Fatalf("%+v %v", res, err)
	}
	// It is not retried: the session is cancelled either way.
	if e.srv.Count("CancelTask") != 1 {
		t.Fatalf("%d cancels", e.srv.Count("CancelTask"))
	}
	again, err := e.svc.Cancel(ctx, id)
	if err != nil || again.Agent != sessions.AgentUnknown || e.srv.Count("CancelTask") != 1 {
		t.Fatalf("second cancel %+v %v", again, err)
	}
}

func TestCancelOfASessionWithNothingRunning(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	id := e.newSession("first")
	e.run()
	e.waitState(e.firstDelivery(id), dbstate.DeliveryCompleted)

	res, err := e.svc.Cancel(context.Background(), id)
	if err != nil || res.Agent != sessions.AgentNotRunning || res.Status != dbstate.SessionCancelled {
		t.Fatalf("%+v %v", res, err)
	}
	if e.srv.Count("CancelTask") != 0 {
		t.Fatal("cancelled a task that was not running")
	}
}

func TestCancelOfAMessageNeverSentSendsNothing(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	id := e.newSession("first")
	did := e.firstDelivery(id)

	res, err := e.svc.Cancel(context.Background(), id)
	if err != nil || res.Agent != sessions.AgentNotRunning {
		t.Fatalf("%+v %v", res, err)
	}
	e.run()
	time.Sleep(150 * time.Millisecond)
	if d := e.delivery(did); d.State != dbstate.DeliveryCancelled || e.sends() != 0 {
		t.Fatalf("state %s, %d sends", d.State, e.sends())
	}
}

func TestTheMainThreadCannotBeCancelled(t *testing.T) {
	e := newEnv(t)
	m, err := e.svc.EnsureMain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.API().CancelSession(context.Background(), api.CancelSessionRequestObject{SessionId: m.SessionID}); apierr.StatusOf(err) != 409 {
		t.Fatalf("got %v", err)
	}
	if e.sessionStatus(m.SessionID) == dbstate.SessionCancelled {
		t.Fatal("the main thread was cancelled")
	}
}

func TestArchiveClearsAFinishedSessionOnly(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	a := e.API()
	ctx := context.Background()
	id := e.newSession("first")
	e.run()
	e.waitState(e.firstDelivery(id), dbstate.DeliveryCompleted)

	if _, err := a.ArchiveSession(ctx, api.ArchiveSessionRequestObject{SessionId: id}); apierr.StatusOf(err) != 409 {
		t.Fatalf("a live session: got %v", err)
	}
	if _, err := e.svc.Cancel(ctx, id); err != nil {
		t.Fatal(err)
	}
	r, err := a.ArchiveSession(ctx, api.ArchiveSessionRequestObject{SessionId: id})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := r.(api.ArchiveSession200JSONResponse); !ok || got.Status != api.Archived {
		t.Fatalf("got %T %+v", r, r)
	}
	g, _ := a.GetSession(ctx, api.GetSessionRequestObject{SessionId: id})
	if s := api.Session(g.(api.GetSession200JSONResponse)); s.ArchivedAt == nil {
		t.Fatalf("not archived: %+v", s)
	}
	list, _ := a.ListSessions(ctx, api.ListSessionsRequestObject{})
	if len(list.(api.ListSessions200JSONResponse)) != 0 {
		t.Fatalf("archived session still listed: %+v", list)
	}
	// Archiving twice is still fine.
	if err := e.svc.Archive(ctx, id); err != nil {
		t.Fatalf("second archive: %v", err)
	}
}

func TestArchiveRefusesTheMainSessionOverHTTPShape(t *testing.T) {
	e := newEnv(t)
	m, err := e.svc.EnsureMain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.API().ArchiveSession(context.Background(), api.ArchiveSessionRequestObject{SessionId: m.SessionID})
	if c, ok := apierr.As(err); !ok || c.Status != 409 || !strings.Contains(c.Message, "main") {
		t.Fatalf("got %v", err)
	}
}

// leftBehind2 puts a delivery of a session in a state without a claim, as a crashed runner leaves it.
func (e *env) leftBehind2(sessionID string, state dbstate.DeliveryState, taskID string) string {
	e.t.Helper()
	id := uuid.NewString()
	if _, err := e.svc.SendTo(context.Background(), sessionID, id, "left behind"); err != nil {
		e.t.Fatalf("send: %v", err)
	}
	var task, ref *string
	if taskID != "" {
		ref2 := "a2a:task/" + taskID
		task, ref = &taskID, &ref2
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE deliveries SET state=$2, task_id=$3, evidence_ref=$4, attempts=1,
		lease_owner='dead-process', lease_until = now() + interval '1 hour' WHERE id=$1`,
		id, string(state), task, ref); err != nil {
		e.t.Fatal(err)
	}
	return id
}
