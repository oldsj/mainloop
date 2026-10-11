package sessions_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/events"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/sessions"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

const owner = "owner-1"

var agent = kagent.AgentRef{Namespace: "kagent", Name: "mainloop-main"}

func quiet() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// env is a scratch database, the fake kagent and a service over both.
type env struct {
	t    *testing.T
	pool *pgxpool.Pool
	srv  *kagenttest.Server
	svc  *sessions.Service
}

type envOpt func(*sessions.Options, *kagent.Config)

func newEnv(t *testing.T, opts ...envOpt) *env {
	t.Helper()
	srv := kagenttest.New()
	t.Cleanup(srv.Close)
	srv.AddAgent(agent.Namespace, agent.Name)
	so := sessions.Options{
		UncertainHold: 30 * time.Minute, UncertainRecheck: 20 * time.Millisecond,
		FollowPoll: 20 * time.Millisecond, Lease: 600 * time.Millisecond, Concurrency: 4,
	}
	kc := kagent.Config{GatewayURL: srv.URL, UserID: "mainloop", SendRetryBudget: 2 * time.Second}
	for _, o := range opts {
		o(&so, &kc)
	}
	client, err := kagent.New(kc)
	if err != nil {
		t.Fatal(err)
	}
	pool := testdb.New(t).Pool
	return &env{t: t, pool: pool, srv: srv, svc: &sessions.Service{
		Pool: pool, Kagent: client, OwnerID: owner, Agent: agent, Log: quiet(), Opts: so,
	}}
}

// run starts a runner with the delivery loop until the returned stop is called (or the test ends).
// Stopping lets the handlers return, like a process shutting down.
func (e *env) run() (stop func()) {
	e.t.Helper()
	r := reconcile.NewRunner(e.pool, quiet(), nil)
	l := e.svc.DeliveryLoop()
	l.Interval = 20 * time.Millisecond
	r.Add(l)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { _ = r.Run(ctx) })
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); wg.Wait() }) }
	e.t.Cleanup(stop)
	return stop
}

func (e *env) send(text string) string {
	e.t.Helper()
	id := uuid.NewString()
	if _, err := e.svc.Send(context.Background(), id, "", text); err != nil {
		e.t.Fatalf("send: %v", err)
	}
	return id
}

func (e *env) delivery(id string) dbq.Delivery {
	e.t.Helper()
	d, err := dbq.New(e.pool).GetDelivery(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

func (e *env) waitState(id string, want dbstate.DeliveryState) dbq.Delivery {
	e.t.Helper()
	var d dbq.Delivery
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		d = e.delivery(id)
		if d.State == want {
			return d
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("delivery %s is %s (detail %q), want %s", id, d.State, deref(d.Detail), want)
	return d
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (e *env) messages() []dbq.SessionMessage {
	e.t.Helper()
	m, err := e.svc.EnsureMain(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	rows, err := dbq.New(e.pool).ListSessionMessages(context.Background(), m.SessionID)
	if err != nil {
		e.t.Fatal(err)
	}
	return rows
}

func (e *env) sessionEvents() []api.EventSessionMessage {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(),
		`SELECT payload FROM events WHERE type = $1 ORDER BY id`, string(events.TypeSessionMessage))
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []api.EventSessionMessage
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			e.t.Fatal(err)
		}
		var p api.EventSessionMessage
		if err := json.Unmarshal(raw, &p); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func (e *env) sends() int { return e.srv.Count("SendStreamingMessage") }

// kagentSessionID is the kagent session of the main binding.
func (e *env) kagentSessionID() string {
	e.t.Helper()
	m, err := e.svc.EnsureMain(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	row, err := dbq.New(e.pool).GetMainBinding(context.Background(), owner)
	if err != nil || row.KagentSessionID == nil {
		e.t.Fatalf("no kagent session for %s: %v", m.SessionID, err)
	}
	return *row.KagentSessionID
}

// completedTask is a finished task for a message, with a reply in an artifact.
func completedTask(taskID, contextID, messageID, reply string) *a2a.Task {
	user := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello"))
	user.ID, user.ContextID, user.TaskID = messageID, contextID, a2a.TaskID(taskID)
	return &a2a.Task{
		ID: a2a.TaskID(taskID), ContextID: contextID, History: []*a2a.Message{user},
		Artifacts: []*a2a.Artifact{{ID: "a1", Parts: a2a.ContentParts{a2a.NewTextPart(reply)}}},
		Status:    a2a.TaskStatus{State: a2a.TaskStateCompleted},
	}
}

// ---- send ---------------------------------------------------------------------------------------

func TestSendIsRecordedOncePerMessageID(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := uuid.NewString()
	first, err := e.svc.Send(ctx, id, "", "hello")
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.svc.Send(ctx, id, first.ConversationID, "hello")
	if err != nil || again != first {
		t.Fatalf("second send %+v, %v; first %+v", again, err, first)
	}
	if n := len(e.messages()); n != 1 {
		t.Fatalf("%d messages", n)
	}
	if e.sends() != 0 {
		t.Fatal("Send called kagent")
	}
	d := e.delivery(id)
	if d.State != dbstate.DeliveryRecorded || d.SessionID != first.ConversationID {
		t.Fatalf("delivery %+v", d)
	}
}

func TestSendIsRefusedWhileATurnIsOpen(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.send("one")
	_, err := e.svc.Send(ctx, uuid.NewString(), "", "two")
	if !errors.Is(err, sessions.ErrBusy) {
		t.Fatalf("got %v, want ErrBusy", err)
	}
	if n := len(e.messages()); n != 1 {
		t.Fatalf("a refused send left %d messages", n)
	}
}

func TestSendValidation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for name, c := range map[string]struct {
		id, conv, text string
		want           error
	}{
		"empty":        {uuid.NewString(), "", "  \n", sessions.ErrInvalid},
		"too long":     {uuid.NewString(), "", strings.Repeat("x", sessions.MaxMessageRunes+1), sessions.ErrInvalid},
		"bad id":       {"not-a-uuid", "", "hi", sessions.ErrInvalid},
		"other thread": {uuid.NewString(), "no-such-conversation", "hi", sessions.ErrNotFound},
	} {
		if _, err := e.svc.Send(ctx, c.id, c.conv, c.text); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", name, err, c.want)
		}
	}
}

// ---- delivery -----------------------------------------------------------------------------------

func TestReplyIsMirroredAndAnnouncedWithTheSettle(t *testing.T) {
	e := newEnv(t)
	e.run()
	id := e.send("hello there")
	d := e.waitState(id, dbstate.DeliveryCompleted)
	if d.TaskID == nil || deref(d.EvidenceRef) != "a2a:task/"+*d.TaskID || d.LeaseOwner != nil {
		t.Fatalf("delivery %+v", d)
	}
	msgs := e.messages()
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[0].Content != "hello there" ||
		msgs[1].Role != "assistant" || msgs[1].Content != "ok" {
		t.Fatalf("messages %+v", msgs)
	}
	evs := e.sessionEvents()
	if len(evs) != 1 || evs[0].MessageId != msgs[1].ID || evs[0].SessionId != d.SessionID ||
		evs[0].Role != api.EventSessionMessageRoleAssistant {
		t.Fatalf("events %+v", evs)
	}
	if e.sends() != 1 {
		t.Fatalf("%d sends", e.sends())
	}
	main, _ := dbq.New(e.pool).GetMainBinding(context.Background(), owner)
	if main.Turns != 1 || main.SessionStatus != dbstate.SessionIdle {
		t.Fatalf("turns %d, status %s", main.Turns, main.SessionStatus)
	}
	// The turn is over: the next message goes through.
	next := e.send("and again")
	e.waitState(next, dbstate.DeliveryCompleted)
}

func TestKagentSeesTheLedgerMessageID(t *testing.T) {
	e := newEnv(t)
	e.run()
	id := e.send("hi")
	e.waitState(id, dbstate.DeliveryCompleted)
	reqs := e.srv.Requests()
	var got []string
	for _, r := range reqs {
		if r.Key == "SendStreamingMessage" {
			got = append(got, r.MessageID)
		}
	}
	if len(got) != 1 || got[0] != id {
		t.Fatalf("kagent saw message ids %v, want [%s]", got, id)
	}
}

func TestNotAcceptedIsRetriedWithTheSameMessageID(t *testing.T) {
	e := newEnv(t)
	e.srv.FailNext("SendStreamingMessage", kagenttest.Fault{RPC: kagenttest.NotAccepted(10)})
	e.srv.FailNext("SendStreamingMessage", kagenttest.Fault{RPC: kagenttest.NotAccepted(10)})
	e.run()
	id := e.send("hi")
	e.waitState(id, dbstate.DeliveryCompleted)
	if e.sends() != 3 {
		t.Fatalf("%d sends", e.sends())
	}
	for _, r := range e.srv.Requests() {
		if r.Key == "SendStreamingMessage" && r.MessageID != id {
			t.Fatalf("message id %q, want %q", r.MessageID, id)
		}
	}
}

func TestRefusalsEndInFailedAndTheOwnerMaySendAgain(t *testing.T) {
	t.Run("not accepted past the budget is not sent", func(t *testing.T) {
		e := newEnv(t, func(_ *sessions.Options, k *kagent.Config) { k.SendRetryBudget = 40 * time.Millisecond })
		for range 8 {
			e.srv.FailNext("SendStreamingMessage", kagenttest.Fault{RPC: kagenttest.NotAccepted(15)})
		}
		e.run()
		id := e.send("hi")
		d := e.waitState(id, dbstate.DeliveryFailed)
		if !strings.HasPrefix(deref(d.Detail), "not sent:") {
			t.Fatalf("detail %q", deref(d.Detail))
		}
	})
	t.Run("a rejected send is failed, not retried", func(t *testing.T) {
		e := newEnv(t)
		e.srv.FailNext("SendStreamingMessage", kagenttest.Fault{RPC: &kagenttest.RPCError{Code: -32602, Message: "invalid params"}})
		e.run()
		id := e.send("hi")
		d := e.waitState(id, dbstate.DeliveryFailed)
		if !strings.HasPrefix(deref(d.Detail), "send rejected:") || e.sends() != 1 {
			t.Fatalf("detail %q, %d sends", deref(d.Detail), e.sends())
		}
	})
	t.Run("a failed task keeps its reason, redacted", func(t *testing.T) {
		e := newEnv(t)
		failed := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("boom\nAuthorization: Bearer abc.def.ghi and password=hunter2"))
		failedTask := &a2a.Task{
			ID: "t-fail", ContextID: "ctx", Status: a2a.TaskStatus{State: a2a.TaskStateFailed, Message: failed},
		}
		e.srv.Script(kagenttest.Emit{Event: failedTask})
		e.srv.AddTask(failedTask)
		e.run()
		id := e.send("hi")
		d := e.waitState(id, dbstate.DeliveryFailed)
		det := deref(d.Detail)
		if !strings.HasPrefix(det, "task failed: boom") || strings.Contains(det, "hunter2") || strings.Contains(det, "abc.def") {
			t.Fatalf("detail %q", det)
		}
		if deref(d.EvidenceRef) != "a2a:task/t-fail#failed" {
			t.Fatalf("evidence %q", deref(d.EvidenceRef))
		}
		// Nothing was produced to mirror, but the page is told the turn ended, naming the user message.
		if evs := e.sessionEvents(); len(evs) != 1 || evs[0].MessageId != id || len(e.messages()) != 1 {
			t.Fatalf("events %+v, %d messages", e.sessionEvents(), len(e.messages()))
		}
	})
}

func TestADroppedStreamBecomesUncertainAndIsResolvedByLookup(t *testing.T) {
	e := newEnv(t)
	e.srv.Script(kagenttest.Drop{})
	e.run()
	id := e.send("hi")
	d := e.waitState(id, dbstate.DeliveryUncertain)
	if d.TaskID != nil {
		t.Fatalf("task %v", d.TaskID)
	}
	// While uncertain, the session stays blocked: nothing is replayed, nothing new is sent.
	if _, err := e.svc.Send(context.Background(), uuid.NewString(), "", "next"); !errors.Is(err, sessions.ErrBusy) {
		t.Fatalf("send during uncertain: %v", err)
	}
	// The message did reach kagent: its task shows up and the lookup adopts it.
	e.srv.AddTask(completedTask("t-found", e.kagentSessionID(), id, "the reply"))
	d = e.waitState(id, dbstate.DeliveryCompleted)
	if deref(d.TaskID) != "t-found" {
		t.Fatalf("task %v", d.TaskID)
	}
	if e.sends() != 1 {
		t.Fatalf("%d sends: the message was replayed", e.sends())
	}
	msgs := e.messages()
	if len(msgs) != 2 || msgs[1].Content != "the reply" {
		t.Fatalf("messages %+v", msgs)
	}
}

func TestAnUncertainDeliveryNobodyCanFindIsFailedAfterTheHoldWithoutAResend(t *testing.T) {
	e := newEnv(t, func(o *sessions.Options, _ *kagent.Config) { o.UncertainHold = 250 * time.Millisecond })
	e.srv.Script(kagenttest.Drop{})
	e.run()
	id := e.send("hi")
	e.waitState(id, dbstate.DeliveryUncertain)
	d := e.waitState(id, dbstate.DeliveryFailed)
	if !strings.Contains(deref(d.Detail), "not sent again") || e.sends() != 1 {
		t.Fatalf("detail %q, %d sends", deref(d.Detail), e.sends())
	}
	if evs := e.sessionEvents(); len(evs) != 1 || evs[0].MessageId != id {
		t.Fatalf("the hold expiry published %+v", evs)
	}
	// After the hold the owner may resend: that is a new message with a new id.
	e.waitState(e.send("hi again"), dbstate.DeliveryCompleted)
}

func TestAStreamThatEndsWithoutATaskIsUncertainNotFailed(t *testing.T) {
	e := newEnv(t)
	e.srv.Script(kagenttest.Comment{})
	e.run()
	id := e.send("hi")
	e.waitState(id, dbstate.DeliveryUncertain)
}

// ---- crash and restart --------------------------------------------------------------------------

// A process that dies leaves its row claimed. The next claimer waits out the lease, then acts
// from the state the row was left in. insert writes the row as the dead process left it.
func (e *env) leftBehind(state dbstate.DeliveryState, taskID, text string) string {
	e.t.Helper()
	ctx := context.Background()
	id := e.send(text)
	var task, ref *string
	if taskID != "" {
		ref2 := "a2a:task/" + taskID
		task, ref = &taskID, &ref2
	}
	if _, err := e.pool.Exec(ctx, `UPDATE deliveries SET state=$2, task_id=$3, evidence_ref=$4, attempts=1,
		lease_owner='dead-process', lease_until = now() - interval '1 second' WHERE id=$1`,
		id, string(state), task, ref); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func TestARecordedRowLeftByACrashIsSentOnce(t *testing.T) {
	e := newEnv(t)
	id := e.leftBehind(dbstate.DeliveryRecorded, "", "hello")
	e.run()
	e.waitState(id, dbstate.DeliveryCompleted)
	if e.sends() != 1 {
		t.Fatalf("%d sends", e.sends())
	}
}

func TestASendingRowLeftByACrashIsLookedUpNotResent(t *testing.T) {
	t.Run("kagent has the task", func(t *testing.T) {
		e := newEnv(t)
		// The kagent session exists, as it does once a send has started.
		first := e.send("warm up")
		stop := e.run()
		e.waitState(first, dbstate.DeliveryCompleted)
		stop()
		sends := e.sends()

		id := e.leftBehind(dbstate.DeliverySending, "", "the one that was in flight")
		e.srv.AddTask(completedTask("t-inflight", e.kagentSessionID(), id, "done before the crash"))
		e.run()
		d := e.waitState(id, dbstate.DeliveryCompleted)
		if deref(d.TaskID) != "t-inflight" || e.sends() != sends {
			t.Fatalf("task %v, %d sends (was %d)", d.TaskID, e.sends(), sends)
		}
	})
	t.Run("kagent has nothing", func(t *testing.T) {
		e := newEnv(t, func(o *sessions.Options, _ *kagent.Config) { o.UncertainHold = 200 * time.Millisecond })
		first := e.send("warm up")
		stop := e.run()
		e.waitState(first, dbstate.DeliveryCompleted)
		stop()
		sends := e.sends()

		id := e.leftBehind(dbstate.DeliverySending, "", "lost")
		e.run()
		e.waitState(id, dbstate.DeliveryFailed)
		if e.sends() != sends {
			t.Fatalf("%d sends (was %d): the message was replayed", e.sends(), sends)
		}
	})
}

func TestADeliveredRowLeftByACrashIsFollowedToTheEnd(t *testing.T) {
	e := newEnv(t)
	first := e.send("warm up")
	stop := e.run()
	e.waitState(first, dbstate.DeliveryCompleted)
	stop()
	sends := e.sends()

	id := e.leftBehind(dbstate.DeliveryDelivered, "t-running", "long task")
	e.srv.AddTask(completedTask("t-running", e.kagentSessionID(), id, "finished while we were away"))
	e.run()
	e.waitState(id, dbstate.DeliveryCompleted)
	msgs := e.messages()
	if last := msgs[len(msgs)-1]; last.Content != "finished while we were away" || e.sends() != sends {
		t.Fatalf("last message %q, %d sends", last.Content, e.sends())
	}
}

func TestARunnerStoppedMidTurnIsReplacedByOneThatFinishesIt(t *testing.T) {
	e := newEnv(t)
	running := &a2a.Task{ID: "t-1", ContextID: "ctx", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}
	e.srv.Script(kagenttest.Emit{Event: running}, kagenttest.Hang{})
	stop := e.run()
	id := e.send("hi")
	e.waitState(id, dbstate.DeliveryDelivered)
	stop()
	if d := e.delivery(id); d.State != dbstate.DeliveryDelivered {
		t.Fatalf("a stopped runner left %s", d.State)
	}
	// The task finished while no runner was up.
	e.srv.AddTask(completedTask("t-1", e.kagentSessionID(), id, "all done"))
	e.run()
	e.waitState(id, dbstate.DeliveryCompleted)
	if e.sends() != 1 {
		t.Fatalf("%d sends", e.sends())
	}
	if n := len(e.sessionEvents()); n != 1 {
		t.Fatalf("%d session:message events", n)
	}
}

func TestAnInterruptedSendIsNotResentByTheNextRunner(t *testing.T) {
	e := newEnv(t)
	e.srv.Script(kagenttest.Hang{}) // the request is out, nothing comes back
	stop := e.run()
	id := e.send("hi")
	deadline := time.Now().Add(10 * time.Second)
	for e.delivery(id).State != dbstate.DeliverySending && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	for e.sends() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	e.run()
	// Nothing in kagent shows the message, so it stays unresolved; it is not sent again.
	e.waitState(id, dbstate.DeliveryUncertain)
	time.Sleep(200 * time.Millisecond)
	if e.sends() != 1 {
		t.Fatalf("%d sends", e.sends())
	}
}

func TestTwoRunnersDeliverEachMessageOnce(t *testing.T) {
	e := newEnv(t)
	e.run()
	e.run()
	id := e.send("hi")
	e.waitState(id, dbstate.DeliveryCompleted)
	time.Sleep(100 * time.Millisecond)
	if e.sends() != 1 || len(e.sessionEvents()) != 1 {
		t.Fatalf("%d sends, %d events", e.sends(), len(e.sessionEvents()))
	}
}

func TestThePartialUniqueIndexAllowsOneOpenTurnPerSession(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	first := e.send("one")
	m, _ := e.svc.EnsureMain(ctx)
	for _, st := range []string{"recorded", "sending", "delivered"} {
		_, err := e.pool.Exec(ctx, `INSERT INTO deliveries (id, idempotency_key, session_id, state) VALUES ($1,$1,$2,$3)`,
			uuid.NewString(), m.SessionID, st)
		if err == nil {
			t.Errorf("a second %s delivery was accepted", st)
		}
	}
	for _, st := range []string{"completed", "failed", "cancelled", "uncertain"} {
		if _, err := e.pool.Exec(ctx, `INSERT INTO deliveries (id, idempotency_key, session_id, state) VALUES ($1,$1,$2,$3)`,
			uuid.NewString(), m.SessionID, st); err != nil {
			t.Errorf("a %s delivery next to an open one was refused: %v", st, err)
		}
	}
	_ = first
}

func TestEnsureMainIsIdempotentAndConcurrencySafe(t *testing.T) {
	e := newEnv(t)
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	for range 8 {
		wg.Go(func() {
			m, err := e.svc.EnsureMain(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			ids <- m.SessionID
		})
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		seen[id] = true
	}
	if len(seen) != 1 {
		t.Fatalf("%d main sessions", len(seen))
	}
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM sessions`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d sessions, %v", n, err)
	}
}

func TestOptionsFromSettings(t *testing.T) {
	cfg, err := config.Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": "x.test"})
	if err != nil {
		t.Fatal(err)
	}
	o := sessions.OptionsFrom(cfg.Sessions)
	if o.UncertainHold != 30*time.Minute || o.UncertainRecheck != 30*time.Second || o.FollowPoll != 2*time.Second ||
		o.Concurrency != 16 || o.EnsureEvery != time.Minute || o.CarryOverMessages != 6 {
		t.Fatalf("%+v", o)
	}
	if _, err := config.Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": "x.test", "SESSION_DELIVERY_CONCURRENCY": "0"}); err == nil {
		t.Fatal("concurrency 0 accepted")
	}
	if _, err := config.Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": "x.test", "MAIN_CARRY_OVER_MESSAGES": "-1"}); err == nil {
		t.Fatal("a negative carry-over accepted")
	}
}

func TestUnauthorizedIsNotSent(t *testing.T) {
	e := newEnv(t)
	e.srv.FailNext("SendStreamingMessage", kagenttest.Fault{RPC: kagenttest.Unauthorized()})
	e.run()
	id := e.send("hi")
	d := e.waitState(id, dbstate.DeliveryFailed)
	if !strings.HasPrefix(deref(d.Detail), "not sent:") || e.sends() != 1 {
		t.Fatalf("detail %q, %d sends", deref(d.Detail), e.sends())
	}
}

// A claimer that lost its lease cannot change the row, whatever it tries.
func TestFencedWritesFromAStaleClaimMatchNothing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.send("hi")
	q := dbq.New(e.pool)
	if _, err := e.pool.Exec(ctx, `UPDATE deliveries SET state='sending', attempts=2, lease_owner='new', lease_until=now()+interval '1 minute' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	stale := "old"
	for name, run := range map[string]func() (int64, error){
		"finish": func() (int64, error) {
			return q.FinishDelivery(ctx, dbq.FinishDeliveryParams{ID: id, LeaseOwner: &stale, Attempts: 1, State: dbstate.DeliveryFailed})
		},
		"delivered": func() (int64, error) {
			task := "t"
			return q.MarkDeliveryDelivered(ctx, dbq.MarkDeliveryDeliveredParams{ID: id, LeaseOwner: &stale, Attempts: 1, TaskID: &task})
		},
		"wrong attempt": func() (int64, error) {
			own := "new"
			return q.FinishDelivery(ctx, dbq.FinishDeliveryParams{ID: id, LeaseOwner: &own, Attempts: 1, State: dbstate.DeliveryFailed})
		},
	} {
		if n, err := run(); err != nil || n != 0 {
			t.Errorf("%s: %d rows, %v", name, n, err)
		}
	}
	if d := e.delivery(id); d.State != dbstate.DeliverySending {
		t.Fatalf("state %s", d.State)
	}
}
