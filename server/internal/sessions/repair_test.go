package sessions_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	"github.com/oldsj/mainloop/server/internal/sessions"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

func (e *env) native() *api.NativeSessionInfo {
	e.t.Helper()
	resp, err := e.API().GetMainThread(context.Background(), api.GetMainThreadRequestObject{})
	if err != nil {
		e.t.Fatal(err)
	}
	n := resp.(api.GetMainThread200JSONResponse).Native
	if n == nil {
		e.t.Fatal("no native info")
	}
	return n
}

// other is a second kagent client, to change the fake's Sessions behind the ledger's back.
func (e *env) other() *kagent.Client {
	e.t.Helper()
	c, err := kagent.New(kagent.Config{GatewayURL: e.srv.URL, UserID: "mainloop"})
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *env) bindingRow() dbq.GetMainBindingRow {
	e.t.Helper()
	row, err := dbq.New(e.pool).GetMainBinding(context.Background(), owner)
	if err != nil {
		e.t.Fatal(err)
	}
	return row
}

// ---- P1-1: the order of the deliveries ------------------------------------------------------------

func TestDeliveriesAreListedOldestFirst(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run()
	first := e.send("one")
	e.waitState(first, dbstate.DeliveryCompleted)
	second := e.send("two")
	e.waitState(second, dbstate.DeliveryCompleted)
	ds := e.native().Deliveries
	if len(ds) != 2 || ds[0].MessageId != first || ds[1].MessageId != second {
		t.Fatalf("deliveries %+v, want [%s %s]", ds, first, second)
	}
}

func TestOnlyTheNewestTwentyDeliveriesAreShownOldestFirst(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	m, err := e.svc.EnsureMain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 25 {
		if _, err := e.pool.Exec(context.Background(), `INSERT INTO deliveries (id, idempotency_key, session_id, state, created_at)
			VALUES ($1, $1, $2, 'completed', now() - make_interval(mins => $3))`,
			fmt.Sprintf("d%02d", i), m.SessionID, 100-i); err != nil {
			t.Fatal(err)
		}
	}
	ds := e.native().Deliveries
	if len(ds) != 20 || ds[0].MessageId != "d05" || ds[19].MessageId != "d24" {
		t.Fatalf("%d deliveries, first %q, last %q", len(ds), ds[0].MessageId, ds[len(ds)-1].MessageId)
	}
	for i := 1; i < len(ds); i++ {
		if ds[i-1].MessageId >= ds[i].MessageId {
			t.Fatalf("not oldest first at %d: %s then %s", i, ds[i-1].MessageId, ds[i].MessageId)
		}
	}
}

// ---- P2-1: an uncertain delivery is in flight only while it blocks a send -------------------------

func TestAnUncertainDeliveryIsInFlightOnlyInsideTheHold(t *testing.T) {
	t.Parallel()
	e := newEnv(t) // the hold is 30 minutes
	id := e.send("hi")
	ctx := context.Background()
	set := func(age string) {
		if _, err := e.pool.Exec(ctx, `UPDATE deliveries SET state='uncertain', updated_at = now() - $2::interval WHERE id=$1`, id, age); err != nil {
			t.Fatal(err)
		}
	}

	set("1 minute")
	n := e.native()
	if !n.TurnInFlight || n.Note == nil || !strings.Contains(*n.Note, "new messages wait") {
		t.Fatalf("inside the hold: in flight %v, note %v", n.TurnInFlight, n.Note)
	}
	if sent, err := e.svc.Send(ctx, uuid.NewString(), "", "next"); err != nil || !sent.Queued {
		t.Fatalf("send inside the hold: %+v, %v", sent, err)
	}

	set("2 hours")
	n = e.native()
	if n.TurnInFlight || n.Note == nil || !strings.Contains(*n.Note, "not sent again") {
		t.Fatalf("past the hold: in flight %v, note %v", n.TurnInFlight, n.Note)
	}
	if _, err := e.svc.Send(ctx, uuid.NewString(), "", "next"); err != nil {
		t.Fatalf("send past the hold: %v", err)
	}
}

// ---- P2-2: every settle publishes ------------------------------------------------------------------

func TestAFailedTaskMirrorsWhatItProducedAndPublishes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	failed := &a2a.Task{
		ID: "t-part", ContextID: "ctx",
		Artifacts: []*a2a.Artifact{{ID: "a1", Parts: a2a.ContentParts{a2a.NewTextPart("half of the answer")}}},
		Status: a2a.TaskStatus{
			State: a2a.TaskStateFailed, Message: a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("out of memory")),
		},
	}
	e.srv.Script(kagenttest.Emit{Event: failed})
	e.srv.AddTask(failed)
	e.run()
	id := e.send("hi")
	d := e.waitState(id, dbstate.DeliveryFailed)
	if !strings.HasPrefix(deref(d.Detail), "task failed: out of memory") {
		t.Fatalf("detail %q", deref(d.Detail))
	}
	msgs := e.messages()
	if len(msgs) != 2 || msgs[1].Role != "assistant" || msgs[1].Content != "half of the answer" {
		t.Fatalf("messages %+v", msgs)
	}
	evs := e.sessionEvents()
	if len(evs) != 1 || evs[0].MessageId != msgs[1].ID || evs[0].SessionId != d.SessionID {
		t.Fatalf("events %+v", evs)
	}
	if e.bindingRow().Turns != 0 {
		t.Fatal("a failed turn was counted")
	}
}

func TestAStoppedTurnPublishes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	stopped := &a2a.Task{ID: "t-stop", ContextID: "ctx", Status: a2a.TaskStatus{State: a2a.TaskStateCanceled}}
	e.srv.Script(kagenttest.Emit{Event: stopped})
	e.srv.AddTask(stopped)
	e.run()
	id := e.send("hi")
	e.waitState(id, dbstate.DeliveryCancelled)
	// The event names the stop note, the assistant message the turn ended with.
	evs := e.sessionEvents()
	msgs := e.messages()
	last := msgs[len(msgs)-1]
	if len(evs) != 1 || evs[0].MessageId != last.ID || !strings.Contains(last.Content, sessions.TurnStoppedNote) {
		t.Fatalf("events %+v, last message %+v", evs, last)
	}
}

// ---- P2-3: parked tasks are read less and less often ----------------------------------------------

func TestAParkedTaskIsReadWithBackoff(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(o *sessions.Options, _ *kagent.Config) { o.ParkedMax = 160 * time.Millisecond })
	parked := &a2a.Task{ID: "t-park", ContextID: "ctx", Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired}}
	e.srv.Script(kagenttest.Emit{Event: parked})
	e.srv.AddTask(parked)
	e.run()
	id := e.send("hi")
	e.waitState(id, dbstate.DeliveryDelivered)
	time.Sleep(1500 * time.Millisecond)
	// At the 20 ms follow poll that is about 75 reads; with the backoff doubling to 160 ms it is
	// about 12.
	if n := e.srv.Count("GetTask"); n < 3 || n > 25 {
		t.Fatalf("%d GetTask calls in 1.5 s", n)
	}
	if got := e.delivery(id).State; got != dbstate.DeliveryDelivered {
		t.Fatalf("state %s", got)
	}
}

// ---- P2-4: the kagent Session is made usable before a send ----------------------------------------

func TestASuspendedSessionIsResumedBeforeTheNextSend(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run()
	e.waitState(e.send("one"), dbstate.DeliveryCompleted)
	ksid := e.kagentSessionID()
	if _, err := e.other().SuspendSession(context.Background(), ksid); err != nil {
		t.Fatal(err)
	}
	e.waitState(e.send("two"), dbstate.DeliveryCompleted)
	if n := e.srv.Count(kagent.ProcResumeSession); n != 1 {
		t.Fatalf("%d resumes", n)
	}
	if e.kagentSessionID() != ksid || e.srv.Count(kagent.ProcCreateSession) != 1 {
		t.Fatal("the session was replaced instead of resumed")
	}
}

func TestASessionStillStartingIsWaitedFor(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(o *sessions.Options, _ *kagent.Config) { o.ReadyWait = 5 * time.Second })
	var gets atomic.Int32
	e.srv.TamperSession = func(proc string, s *kagentapi.Session) {
		if proc == kagent.ProcGetSession && gets.Add(1) <= 3 {
			s.State = kagentapi.RuntimeState_RUNTIME_STATE_CREATING
		}
	}
	e.run()
	e.waitState(e.send("one"), dbstate.DeliveryCompleted) // creates the session
	e.waitState(e.send("two"), dbstate.DeliveryCompleted) // reads it: starting, starting, starting, ready
	if gets.Load() < 4 {
		t.Fatalf("%d GetSession calls", gets.Load())
	}
	if e.sends() != 2 {
		t.Fatalf("%d sends", e.sends())
	}
}

func TestASessionThatNeverGetsReadyIsNotSentTo(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(o *sessions.Options, _ *kagent.Config) { o.ReadyWait = 150 * time.Millisecond })
	e.srv.TamperSession = func(proc string, s *kagentapi.Session) {
		if proc == kagent.ProcCreateSession || proc == kagent.ProcGetSession {
			s.State = kagentapi.RuntimeState_RUNTIME_STATE_CREATING
		}
	}
	e.run()
	id := e.send("hi")
	deadline := time.Now().Add(30 * time.Second)
	for e.srv.Count(kagent.ProcGetSession) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if e.srv.Count(kagent.ProcGetSession) < 2 {
		t.Fatal("the wait never polled")
	}
	if d := e.delivery(id); e.sends() != 0 || d.State == dbstate.DeliveryCompleted {
		t.Fatalf("sent to a session that was not ready: %d sends, state %s", e.sends(), d.State)
	}
}

func TestASessionKagentNoLongerHasIsReplaced(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run()
	e.waitState(e.send("one"), dbstate.DeliveryCompleted)
	old := e.bindingRow()
	if _, err := e.other().DeleteSession(context.Background(), *old.KagentSessionID); err != nil {
		t.Fatal(err)
	}
	e.waitState(e.send("two"), dbstate.DeliveryCompleted)
	now := e.bindingRow()
	if now.KagentSessionID == nil || *now.KagentSessionID == *old.KagentSessionID {
		t.Fatalf("session %v, was %v", now.KagentSessionID, old.KagentSessionID)
	}
	if deref(now.KagentRequestID) == deref(old.KagentRequestID) || e.srv.Count(kagent.ProcCreateSession) != 2 {
		t.Fatalf("request id %q (was %q), %d creates", deref(now.KagentRequestID), deref(old.KagentRequestID),
			e.srv.Count(kagent.ProcCreateSession))
	}
	if _, ok := e.srv.Session(*now.KagentSessionID); !ok {
		t.Fatal("the replacement does not exist")
	}
}

func TestBindingWritesFromAStaleClaimMatchNothing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.leftBehind(dbstate.DeliveryRecorded, "", "hi")
	q := dbq.New(e.pool)
	stale := "dead-process"
	n, err := q.ReplaceDeliveryKagentSession(context.Background(), dbq.ReplaceDeliveryKagentSessionParams{
		ID: id, LeaseOwner: &stale, Attempts: 99, NewRequestID: ptr("other"),
	})
	if err != nil || n != 0 {
		t.Fatalf("replace by a stale claim changed %d rows (%v)", n, err)
	}
	n, err = q.SetDeliveryKagentSession(context.Background(), dbq.SetDeliveryKagentSessionParams{
		ID: id, LeaseOwner: &stale, Attempts: 99, KagentSessionID: ptr("ks"),
	})
	if err != nil || n != 0 {
		t.Fatalf("set by a stale claim changed %d rows (%v)", n, err)
	}
}

func ptr[T any](v T) *T { return &v }

// ---- P2-5: the main session cannot be archived -----------------------------------------------------

func TestTheMainSessionCannotBeArchived(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	m, err := e.svc.EnsureMain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.pool.Exec(context.Background(), `UPDATE sessions SET archived_at = now() WHERE id = $1`, m.SessionID)
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "23514" {
		t.Fatalf("archive: %v", err)
	}
	// Other sessions archive as before.
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO sessions (id, user_id, title, description, prompt) VALUES ('other', $1, 'o', 'o', 'o')`, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE sessions SET archived_at = now() WHERE id = 'other'`); err != nil {
		t.Fatal(err)
	}
}

// ---- P3-1, P3-2 ------------------------------------------------------------------------------------

func TestADeliveryWhoseBindingIsGoneFailsInsteadOfLooping(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.send("hi")
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM agent_bindings`); err != nil {
		t.Fatal(err)
	}
	e.run()
	d := e.waitState(id, dbstate.DeliveryFailed)
	if !strings.Contains(deref(d.Detail), "no agent binding") || e.sends() != 0 {
		t.Fatalf("detail %q, %d sends", deref(d.Detail), e.sends())
	}
}

func TestSendRefusesAMessageIDAnotherMessageUses(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	m, err := e.svc.EnsureMain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	taken := uuid.NewString()
	if _, err := e.pool.Exec(context.Background(),
		`INSERT INTO session_messages (id, session_id, role, content) VALUES ($1, $2, 'assistant', 'x')`, taken, m.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Send(context.Background(), taken, "", "hi"); !errors.Is(err, sessions.ErrInvalid) {
		t.Fatalf("send: %v", err)
	}
	if _, _, d, _ := e.counts(); d != 0 {
		t.Fatalf("%d deliveries recorded", d)
	}
}
