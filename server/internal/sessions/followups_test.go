package sessions_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	"github.com/oldsj/mainloop/server/internal/sessions"
)

// ---- G6a re-review N-P2-1: a create that hits a deleted request id ---------------------------------------

func TestACreateThatHitsADeletedRequestIdGetsANewSession(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run()
	e.waitState(e.send("one"), dbstate.DeliveryCompleted)
	old := e.bindingRow()
	ksid := *old.KagentSessionID

	// The Session was made, but the write that stores it on the binding was lost; then kagent
	// deleted the Session. The binding still holds the request id, which now belongs to a deleted
	// Session, so the next create under it is refused.
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE agent_bindings SET kagent_session_id = NULL WHERE kagent_session_id = $1`, ksid); err != nil {
		t.Fatal(err)
	}
	if _, err := e.other().DeleteSession(context.Background(), ksid); err != nil {
		t.Fatal(err)
	}

	e.waitState(e.send("two"), dbstate.DeliveryCompleted)
	now := e.bindingRow()
	if now.KagentSessionID == nil || *now.KagentSessionID == ksid {
		t.Fatalf("session %v, was %s", now.KagentSessionID, ksid)
	}
	if deref(now.KagentRequestID) == deref(old.KagentRequestID) || deref(now.KagentRequestID) == "" {
		t.Fatalf("request id %q was not replaced (was %q)", deref(now.KagentRequestID), deref(old.KagentRequestID))
	}
	if _, ok := e.srv.Session(*now.KagentSessionID); !ok {
		t.Fatal("the new Session does not exist")
	}
	// The refused create and the new create; the message went once, to the new Session.
	if e.srv.Count(kagent.ProcCreateSession) != 3 || e.sends() != 2 {
		t.Fatalf("%d creates, %d sends", e.srv.Count(kagent.ProcCreateSession), e.sends())
	}
}

// ---- P3-1: the Session id is stored on the delivery when it moves to sending ------------------------------

func TestTheSessionAMessageWasSentToIsStoredOnItsDelivery(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run()
	id := e.send("one")
	d := e.waitState(id, dbstate.DeliveryCompleted)
	if d.KagentSessionID == nil || *d.KagentSessionID != e.kagentSessionID() {
		t.Fatalf("delivery session %v, binding %s", d.KagentSessionID, e.kagentSessionID())
	}
}

func TestAnInterruptedSendIsLookedUpInTheSessionItWasSentTo(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	first := e.send("warm up")
	stop := e.run()
	e.waitState(first, dbstate.DeliveryCompleted)
	stop()
	sent := e.kagentSessionID()
	sends := e.sends()

	id := e.leftBehind(dbstate.DeliverySending, "", "in flight")
	// The binding moved on to another Session after the send started; the delivery remembers where
	// the message went.
	if _, err := e.pool.Exec(context.Background(), `UPDATE deliveries SET kagent_session_id = $2 WHERE id = $1`, id, sent); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE agent_bindings SET kagent_session_id = 'replacement', kagent_request_id = 'req-new'`); err != nil {
		t.Fatal(err)
	}
	e.srv.AddTask(completedTask("t-inflight", sent, id, "done before the crash"))
	e.run()
	d := e.waitState(id, dbstate.DeliveryCompleted)
	if deref(d.TaskID) != "t-inflight" || e.sends() != sends {
		t.Fatalf("task %v, %d sends (was %d)", d.TaskID, e.sends(), sends)
	}
}

// ---- P3-2: READY with an operation in progress is not settled ---------------------------------------------

func TestAReadySessionWithAnOperationInProgressIsWaitedFor(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(o *sessions.Options, _ *kagent.Config) { o.ReadyWait = 5 * time.Second })
	var gets atomic.Int32
	e.srv.TamperSession = func(proc string, s *kagentapi.Session) {
		if proc == kagent.ProcGetSession && gets.Add(1) <= 3 {
			s.State = kagentapi.RuntimeState_RUNTIME_STATE_READY
			s.Operation = kagentapi.RuntimeOperation_RUNTIME_OPERATION_SUSPEND
		}
	}
	e.run()
	e.waitState(e.send("one"), dbstate.DeliveryCompleted)
	e.waitState(e.send("two"), dbstate.DeliveryCompleted)
	if gets.Load() < 4 || e.sends() != 2 {
		t.Fatalf("%d GetSession calls, %d sends", gets.Load(), e.sends())
	}
}

func TestASessionThatIsAlwaysChangingIsNotSentTo(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(o *sessions.Options, _ *kagent.Config) { o.ReadyWait = 150 * time.Millisecond })
	e.srv.TamperSession = func(proc string, s *kagentapi.Session) {
		if proc == kagent.ProcCreateSession || proc == kagent.ProcGetSession {
			s.State = kagentapi.RuntimeState_RUNTIME_STATE_READY
			s.Operation = kagentapi.RuntimeOperation_RUNTIME_OPERATION_RESUME
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
		t.Fatalf("sent to a Session mid-operation: %d sends, state %s", e.sends(), d.State)
	}
}

// ---- P3-3: the end of an uncertain delivery's hold is announced while lookups fail ------------------------

func TestTheEndOfTheHoldIsAnnouncedOnceWhileLookupsFail(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(o *sessions.Options, _ *kagent.Config) { o.UncertainHold = 400 * time.Millisecond })
	e.srv.Script(kagenttest.Drop{})
	for range 400 {
		e.srv.FailNext("ListTasks", kagenttest.Fault{Status: 500})
	}
	e.run()
	id := e.send("hi")
	e.waitState(id, dbstate.DeliveryUncertain)

	deadline := time.Now().Add(30 * time.Second)
	var detail string
	for time.Now().Before(deadline) {
		detail = deref(e.delivery(id).Detail)
		if strings.HasPrefix(detail, "delivery unknown:") && containsHold(detail) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !containsHold(detail) {
		t.Fatalf("detail %q", detail)
	}
	before := len(e.sessionEvents())
	time.Sleep(250 * time.Millisecond)
	if after := len(e.sessionEvents()); after != before {
		t.Fatalf("%d events after the hold passed, %d before: it was announced again", after, before)
	}
	if before < 1 {
		t.Fatal("the end of the hold was not announced")
	}
	if e.sends() != 1 {
		t.Fatalf("%d sends", e.sends())
	}
}

func containsHold(detail string) bool {
	return strings.Contains(detail, "no task showed this message within the hold")
}

// ---- P3-5: follow-error backoff ----------------------------------------------------------------------------

func TestErrorDelayBacksOffFromFollowPollToTheRecheckDelay(t *testing.T) {
	t.Parallel()
	o := sessions.Options{FollowPoll: 100 * time.Millisecond, UncertainRecheck: 800 * time.Millisecond, ParkedMax: 5 * time.Second}
	for _, tt := range []struct {
		attempts int
		want     time.Duration
	}{
		{1, 100 * time.Millisecond}, {2, 200 * time.Millisecond}, {3, 400 * time.Millisecond},
		{4, 800 * time.Millisecond}, {5, 800 * time.Millisecond}, {50, 800 * time.Millisecond},
	} {
		if got := sessions.ErrorDelay(o, false, tt.attempts); got != tt.want {
			t.Errorf("attempt %d: %s, want %s", tt.attempts, got, tt.want)
		}
	}
	// An unresolved delivery is looked at at the recheck delay, whatever the attempt.
	if got := sessions.ErrorDelay(o, true, 1); got != 800*time.Millisecond {
		t.Errorf("uncertain: %s", got)
	}
	if got := sessions.ParkedDelay(o, 20); got != 5*time.Second {
		t.Errorf("parked: %s", got)
	}
}

func TestAFailingTaskReadIsRetriedLessAndLess(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(o *sessions.Options, _ *kagent.Config) {
		o.FollowPoll, o.UncertainRecheck = 20*time.Millisecond, 400*time.Millisecond
	})
	first := e.send("warm up")
	stop := e.run()
	e.waitState(first, dbstate.DeliveryCompleted)
	stop()

	id := e.leftBehind(dbstate.DeliveryDelivered, "t-x", "long")
	e.srv.AddTask(runningTask("t-x", e.kagentSessionID(), id, "so far"))
	for range 200 {
		e.srv.FailNext("GetTask", kagenttest.Fault{Status: 500})
	}
	base := e.srv.Count("GetTask")
	e.run()
	time.Sleep(1200 * time.Millisecond)
	calls := e.srv.Count("GetTask") - base
	// 20, 40, 80, 160, 320, 400... ms apart: about seven reads in 1.2s; a fixed poll would make sixty.
	if calls < 3 || calls > 14 {
		t.Fatalf("%d reads in 1.2s", calls)
	}
	if e.delivery(id).State != dbstate.DeliveryDelivered {
		t.Fatalf("state %s", e.delivery(id).State)
	}
}

// G5 review P2-2, narrowed by G6b follow-up 1: a binding whose Session was never asked for cannot
// own an unrecorded one, so the orphan sweep's safety count ignores it until the create has been
// requested (the request id is recorded before the call). A claim alone does not count.
func TestUnresolvedBindingsIgnoreTheMainBindingBeforeItsFirstSession(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.send("hello") // records the main binding and a delivery; no loop runs
	q := dbq.New(e.pool)
	count := func() int64 {
		n, err := q.CountUnresolvedBindings(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(); n != 0 {
		t.Fatalf("%d unresolved before any claim", n)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE deliveries SET attempts = 1`); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 0 {
		t.Fatalf("%d unresolved after a claim that sent no create", n)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE agent_bindings SET kagent_create_request_id = 'req-1'`); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Fatalf("%d unresolved after a create with no Session id", n)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE agent_bindings SET kagent_session_id = 'ks-1'`); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 0 {
		t.Fatalf("%d unresolved once the Session id is recorded", n)
	}
}
