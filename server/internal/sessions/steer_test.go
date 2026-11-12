package sessions_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/sessions"
)

// steerable lets the main Agent take steers.
func steerable(o *sessions.Options, _ *kagent.Config) { o.SteerAgents = []string{agent.Name} }

// runBoth starts the delivery and steer loops until the test ends.
func (e *env) runBoth() {
	e.t.Helper()
	r := reconcile.NewRunner(e.pool, quiet(), nil)
	for _, l := range []reconcile.Loop{e.svc.DeliveryLoop(), e.svc.SteerLoop()} {
		l.Interval = 20 * time.Millisecond
		r.Add(l)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { _ = r.Run(ctx) })
	e.t.Cleanup(func() { cancel(); wg.Wait() })
}

// runningTurn starts a turn on the main session whose task keeps working until finishTask ends it.
// Its stream ends at once, so the delivery loop follows the task by reading it.
func (e *env) runningTurn(text string) (sessionID, messageID string, taskID a2a.TaskID) {
	e.t.Helper()
	sessionID, messageID, taskID = e.mainID(), uuid.NewString(), a2a.TaskID("t-"+uuid.NewString())
	ready := make(chan struct{})
	go func() {
		// The kagent Session exists only once the first send creates it.
		defer close(ready)
		for e.bindingKsid(sessionID) == "" {
			time.Sleep(5 * time.Millisecond)
		}
	}()
	e.srv.Script(kagenttest.Pause{D: 50 * time.Millisecond}, kagenttest.Emit{Event: &a2a.Task{
		ID: taskID, Status: a2a.TaskStatus{State: a2a.TaskStateWorking},
	}})
	if _, err := e.svc.SendTo(context.Background(), sessionID, messageID, text); err != nil {
		e.t.Fatalf("send: %v", err)
	}
	<-ready
	task := runningTask(string(taskID), e.bindingKsid(sessionID), messageID, "working on it")
	e.srv.AddTask(task)
	e.waitState(messageID, dbstate.DeliveryDelivered)
	return sessionID, messageID, taskID
}

// finishTask ends the task in state, keeping what kagent recorded in it (steers included).
func (e *env) finishTask(id a2a.TaskID, state a2a.TaskState, unconsumed ...string) {
	e.t.Helper()
	task, ok := e.srv.Task(id)
	if !ok {
		e.t.Fatalf("no task %s", id)
	}
	// kagent (#101) lists unconsumed steers on the task; its final status message can carry
	// metadata of its own, which must not hide that list.
	msg := a2a.NewMessage(a2a.MessageRoleAgent)
	msg.Metadata = map[string]any{"kagent.dev/usage": map[string]any{"output_tokens": 1}}
	task.Status = a2a.TaskStatus{State: state, Message: msg}
	if len(unconsumed) > 0 {
		list := make([]any, len(unconsumed))
		for i, m := range unconsumed {
			list[i] = m
		}
		if task.Metadata == nil {
			task.Metadata = map[string]any{}
		}
		task.Metadata["kagent.steer.unconsumed"] = list
	}
	task.Artifacts = []*a2a.Artifact{{ID: "a1", Parts: a2a.ContentParts{a2a.NewTextPart("all done")}}}
	e.srv.AddTask(task)
}

func (e *env) steer(id string) dbq.DeliverySteer {
	e.t.Helper()
	st, err := dbq.New(e.pool).GetSteer(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

func (e *env) waitSteer(id string, want dbstate.SteerState) dbq.DeliverySteer {
	e.t.Helper()
	var st dbq.DeliverySteer
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		st = e.steer(id)
		if st.State == want {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("steer %s is %s (detail %q), want %s", id, st.State, deref(st.Detail), want)
	return st
}

func (e *env) sendSteer(sessionID, text string) (string, sessions.Sent) {
	e.t.Helper()
	id := uuid.NewString()
	sent, err := e.svc.SendToWith(context.Background(), sessions.SourceUser, sessionID, id, text, sessions.SendOptions{Steer: true})
	if err != nil {
		e.t.Fatalf("steer: %v", err)
	}
	return id, sent
}

// steerSends are the A2A sends kagent saw that were steers into taskID.
func (e *env) steerSends(messageID string) int {
	n := 0
	for _, r := range e.srv.Requests() {
		if r.Key == "SendStreamingMessage" && r.MessageID == messageID && r.Metadata["kagent.steer"] == true {
			n++
		}
	}
	return n
}

func TestASteerJoinsTheRunningTurnAndEndsWithIt(t *testing.T) {
	e := newEnv(t, steerable)
	e.runBoth()
	sid, x, task := e.runningTurn("build it")

	m, sent := e.sendSteer(sid, "also run the tests")
	if !sent.Steered || sent.Queued {
		t.Fatalf("sent %+v, want steered", sent)
	}
	if st := e.waitSteer(m, dbstate.SteerDelivered); st.DeliveryID != x {
		t.Fatalf("steer joined %s, want %s", st.DeliveryID, x)
	}
	if got, ok := e.srv.Task(task); !ok || got.History[len(got.History)-1].ID != m {
		t.Fatalf("kagent's task does not end with the steer: %+v", got)
	}
	if e.steerSends(m) != 1 {
		t.Fatalf("%d steer sends", e.steerSends(m))
	}
	// The steer holds no turn of its own: the session has no delivery under its id.
	if _, err := dbq.New(e.pool).GetDelivery(context.Background(), m); err == nil {
		t.Fatal("a steer that joined the turn has a delivery of its own")
	}

	e.finishTask(task, a2a.TaskStateCompleted)
	e.waitState(x, dbstate.DeliveryCompleted)
	e.waitSteer(m, dbstate.SteerCompleted)

	// The record reads prompt, steer, reply: the order Claude Code shows them in.
	var got []string
	for _, msg := range e.sessionMessages(sid) {
		got = append(got, string(msg.Role)+":"+msg.Content)
	}
	want := []string{"user:build it", "user:also run the tests", "assistant:all done"}
	if len(got) != len(want) {
		t.Fatalf("record %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("record %q, want %q", got, want)
		}
	}
}

func TestASteerIsRecordedOncePerMessageID(t *testing.T) {
	e := newEnv(t, steerable)
	e.runBoth()
	sid, _, _ := e.runningTurn("build it")
	m, _ := e.sendSteer(sid, "also this")
	again, err := e.svc.SendToWith(context.Background(), sessions.SourceUser, sid, m, "also this", sessions.SendOptions{Steer: true})
	if err != nil || !again.Steered {
		t.Fatalf("repeat: %+v, %v", again, err)
	}
	if n := len(e.sessionMessages(sid)); n != 2 {
		t.Fatalf("%d messages after a repeated steer", n)
	}
	e.waitSteer(m, dbstate.SteerDelivered)
	if e.steerSends(m) != 1 {
		t.Fatalf("%d steer sends", e.steerSends(m))
	}
}

func TestARefusedSteerRunsOnceAsTheNextTurn(t *testing.T) {
	e := newEnv(t, steerable)
	e.srv.SteerRefusal = "steer_input_required"
	e.runBoth()
	sid, x, task := e.runningTurn("build it")
	m, _ := e.sendSteer(sid, "and then this")

	st := e.waitSteer(m, dbstate.SteerConverted)
	if d := e.delivery(m); d.State != dbstate.DeliveryQueued {
		t.Fatalf("converted steer's delivery is %s, want queued behind the running turn (detail %q)", d.State, deref(st.Detail))
	}
	e.finishTask(task, a2a.TaskStateCompleted)
	e.waitState(x, dbstate.DeliveryCompleted)
	e.waitState(m, dbstate.DeliveryCompleted)
	if e.steerSends(m) != 1 {
		t.Fatalf("%d steer sends: a refused steer is not sent again", e.steerSends(m))
	}
	if n := e.srv.Count("SendStreamingMessage"); n != 3 {
		t.Fatalf("%d sends, want the turn, the refused steer and the steer's own turn", n)
	}
}

func TestASteerTheTurnNeverGotRunsAsTheNextTurnAheadOfLaterMessages(t *testing.T) {
	e := newEnv(t, steerable)
	// Only the delivery loop runs, so the steer is never sent before the turn ends.
	e.run()
	sid, x, task := e.runningTurn("build it")
	m, _ := e.sendSteer(sid, "steer me in")
	later := uuid.NewString()
	if sent, err := e.svc.SendTo(context.Background(), sid, later, "a later message"); err != nil || !sent.Queued {
		t.Fatalf("later send %+v, %v", sent, err)
	}

	e.finishTask(task, a2a.TaskStateCompleted)
	e.waitState(x, dbstate.DeliveryCompleted)
	if st := e.steer(m); st.State != dbstate.SteerConverted {
		t.Fatalf("steer is %s, want converted with the turn's end", st.State)
	}
	e.waitState(m, dbstate.DeliveryCompleted)
	e.waitState(later, dbstate.DeliveryCompleted)
	if e.delivery(m).UpdatedAt.After(e.delivery(later).UpdatedAt) {
		t.Fatal("the later message ran before the converted steer")
	}
	if e.steerSends(m) != 0 {
		t.Fatal("an unsent steer was sent into an ended turn")
	}
}

func TestASteerTheTurnDidNotConsumeRunsAsTheNextTurn(t *testing.T) {
	e := newEnv(t, steerable)
	e.runBoth()
	sid, x, task := e.runningTurn("build it")
	m, _ := e.sendSteer(sid, "this one gets lost")
	e.waitSteer(m, dbstate.SteerDelivered)

	e.finishTask(task, a2a.TaskStateFailed, m)
	e.waitState(x, dbstate.DeliveryFailed)
	e.waitSteer(m, dbstate.SteerConverted)
	e.waitState(m, dbstate.DeliveryCompleted)
	if e.steerSends(m) != 1 {
		t.Fatalf("%d steer sends", e.steerSends(m))
	}
}

func TestASteerEndsWithAFailedTurnThatConsumedIt(t *testing.T) {
	e := newEnv(t, steerable)
	e.runBoth()
	sid, x, task := e.runningTurn("build it")
	m, _ := e.sendSteer(sid, "this one was read")
	e.waitSteer(m, dbstate.SteerDelivered)

	e.finishTask(task, a2a.TaskStateFailed)
	e.waitState(x, dbstate.DeliveryFailed)
	e.waitSteer(m, dbstate.SteerFailed)
	if _, err := dbq.New(e.pool).GetDelivery(context.Background(), m); err == nil {
		t.Fatal("a consumed steer ran again")
	}
}

func TestAStoppedTurnCancelsItsSteers(t *testing.T) {
	e := newEnv(t, steerable)
	e.run()
	sid, x, _ := e.runningTurn("build it")
	m, _ := e.sendSteer(sid, "never mind")
	if _, err := e.svc.Stop(context.Background(), sid); err != nil {
		t.Fatal(err)
	}
	e.waitState(x, dbstate.DeliveryCancelled)
	if st := e.steer(m); st.State != dbstate.SteerCancelled {
		t.Fatalf("steer is %s, want cancelled with the stopped turn", st.State)
	}
	if _, err := dbq.New(e.pool).GetDelivery(context.Background(), m); err == nil {
		t.Fatal("a steer into a stopped turn became a turn of its own")
	}
}

func TestASteerIsAnOrdinaryMessageWhenItCannotJoin(t *testing.T) {
	cases := []struct {
		name string
		opts []func(*sessions.Options, *kagent.Config)
		text string
	}{
		{"agent takes no steers", nil, "also this"},
		{"slash command", []func(*sessions.Options, *kagent.Config){steerable}, "/compact"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var opts []envOpt
			for _, o := range c.opts {
				opts = append(opts, o)
			}
			e := newEnv(t, opts...)
			e.run()
			sid, _, _ := e.runningTurn("build it")
			id, sent := e.sendSteer(sid, c.text)
			if sent.Steered || !sent.Queued {
				t.Fatalf("sent %+v, want queued", sent)
			}
			if d := e.delivery(id); d.State != dbstate.DeliveryQueued {
				t.Fatalf("delivery %s", d.State)
			}
		})
	}
	t.Run("no running turn", func(t *testing.T) {
		e := newEnv(t, steerable)
		id, sent := e.sendSteer(e.mainID(), "hello")
		if sent.Steered || sent.Queued {
			t.Fatalf("sent %+v, want admitted", sent)
		}
		if d := e.delivery(id); d.State != dbstate.DeliveryRecorded {
			t.Fatalf("delivery %s", d.State)
		}
	})
}

func TestASteerWhoseAnswerWasLostIsLookedUpNotResent(t *testing.T) {
	e := newEnv(t, steerable)
	e.runBoth()
	sid, x, task := e.runningTurn("build it")
	// kagent takes the steer, but its answer is cut short.
	e.srv.FailNext("SendStreamingMessage", kagenttest.Fault{Truncate: true})
	m, _ := e.sendSteer(sid, "also this")
	e.waitSteer(m, dbstate.SteerDelivered)
	if e.steerSends(m) != 1 {
		t.Fatalf("%d steer sends: an uncertain steer was sent again", e.steerSends(m))
	}
	e.finishTask(task, a2a.TaskStateCompleted)
	e.waitState(x, dbstate.DeliveryCompleted)
	e.waitSteer(m, dbstate.SteerCompleted)
}
