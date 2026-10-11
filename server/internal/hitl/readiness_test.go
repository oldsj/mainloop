package hitl_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/hitl"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// suspendableKagent is the fake's Session lifecycle: kagent refuses every send while the Session is
// suspended, before anything runs, with the refusal the client maps to SendNotAccepted.
type suspendable struct {
	mu        sync.Mutex
	suspended bool
	refused   int
}

func (s *suspendable) set(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.suspended = v
}

// install makes the fake refuse sends while suspended.
func (s *suspendable) install(f *fakeKagent) {
	f.onSend = func(f *fakeKagent, snd hitl.Send) error {
		s.mu.Lock()
		down := s.suspended
		if down {
			s.refused++
		}
		s.mu.Unlock()
		if down {
			f.set(func() { f.sends = f.sends[:len(f.sends)-1] }) // nothing ran, so nothing was sent
			return &kagent.SendNotAccepted{}
		}
		f.accept(snd)
		return nil
	}
}

// readier is the sessions package's ReadySession as the dispatcher sees it: it fails while the
// Session is still waking and resumes it once it has woken.
type readier struct {
	s       *suspendable
	mu      sync.Mutex
	waking  int // calls that fail before the Session is ready
	calls   int
	session string
	ws      *string
}

func (r *readier) ReadySession(_ context.Context, sessionID string, workspaceID *string, kagentSessionID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.session, r.ws = sessionID, workspaceID
	if kagentSessionID == "" {
		return errors.New("no kagent session")
	}
	if r.calls <= r.waking {
		return errors.New("the workspace is still waking")
	}
	r.s.set(false)
	return nil
}

func (r *readier) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func TestAnAnswerWakesASuspendedSessionBeforeItIsSent(t *testing.T) {
	var susp suspendable
	r := &readier{s: &susp}
	e := newEnv(t, func(s *hitl.Service) { s.Ready = r })
	susp.install(e.k)
	susp.set(true)
	e.k.put(waitingTask("task-a", "msg-a", toolRequest("t1")))
	e.observe()
	req := e.requestFor("task-a")
	e.run()
	if _, err := e.svc.Respond(context.Background(), req.ID, approveAll("t1")); err != nil {
		t.Fatal(err)
	}
	e.waitResponse("act-1", dbstate.HITLAccepted)
	if susp.refused != 0 || e.k.sendCount() != 1 || r.callCount() != 1 {
		t.Fatalf("refused sends %d, sends %d, ready calls %d", susp.refused, e.k.sendCount(), r.callCount())
	}
	if r.session != session || r.ws != nil {
		t.Fatalf("readied session %q workspace %v", r.session, r.ws)
	}
}

func TestAnAnswerWaitsRecordedWhileTheSessionIsStillWaking(t *testing.T) {
	var susp suspendable
	r := &readier{s: &susp, waking: 3}
	e := newEnv(t, func(s *hitl.Service) { s.Ready = r })
	susp.install(e.k)
	susp.set(true)
	e.k.put(waitingTask("task-a", "msg-a", toolRequest("t1")))
	e.observe()
	req := e.requestFor("task-a")
	e.run()
	if _, err := e.svc.Respond(context.Background(), req.ID, approveAll("t1")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for r.callCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// Waking is not a send: the response is still recorded, and nothing was sent or marked sending.
	if got := e.response("act-1"); got.State != dbstate.HITLRecorded || e.k.sendCount() != 0 {
		t.Fatalf("state %s, sends %d while the session wakes", got.State, e.k.sendCount())
	}
	e.waitResponse("act-1", dbstate.HITLAccepted)
	if e.k.sendCount() != 1 || susp.refused != 0 {
		t.Fatalf("sends %d, refused %d", e.k.sendCount(), susp.refused)
	}
}

func TestAWorkspaceSessionBehindASuspendedWorkspaceIsWokenWithItsWorkspace(t *testing.T) {
	var susp suspendable
	r := &readier{s: &susp}
	e := newEnv(t, func(s *hitl.Service) { s.Ready = r })
	susp.install(e.k)
	susp.set(true)
	ctx := context.Background()
	for _, q := range []string{`DELETE FROM agent_bindings`, `DELETE FROM sessions`} {
		if _, err := e.pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	e.addSession("sess-ws", "ks-ws", "agent")
	for _, q := range []string{
		`INSERT INTO projects (id, user_id, owner, name, full_name, html_url) VALUES ('p', $1, 'o', 'n', 'o/n', 'https://x')`,
		`INSERT INTO workspaces (id, user_id, project_id, repo_url, branch, agent_kind, agent_namespace, agent_name, request_id)
		 VALUES ('sess-ws', $1, 'p', 'https://x', 'b', 'claude', 'kagent', 'claude-ws', 'req-ws')`,
	} {
		if _, err := e.pool.Exec(ctx, q, owner); err != nil {
			t.Fatal(err)
		}
	}
	e.k.put(inContext(waitingTask("task-a", "msg-a", toolRequest("t1")), "ks-ws"))
	e.observe()
	req := e.requestFor("task-a")
	e.run()
	if _, err := e.svc.Respond(ctx, req.ID, approveAll("t1")); err != nil {
		t.Fatal(err)
	}
	e.waitResponse("act-1", dbstate.HITLAccepted)
	if r.ws == nil || *r.ws != "sess-ws" || r.session != "sess-ws" || susp.refused != 0 {
		t.Fatalf("readied session %q workspace %v, refused sends %d", r.session, r.ws, susp.refused)
	}
}

func TestKagentsNotReadyRefusalIsNotASend(t *testing.T) {
	// No readiness step: the refusal alone must leave the response recorded and retried.
	var susp suspendable
	e := newEnv(t)
	susp.install(e.k)
	susp.set(true)
	e.k.put(waitingTask("task-a", "msg-a", toolRequest("t1")))
	e.observe()
	req := e.requestFor("task-a")
	e.run()
	if _, err := e.svc.Respond(context.Background(), req.ID, approveAll("t1")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		susp.mu.Lock()
		n := susp.refused
		susp.mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := e.response("act-1").State; got == dbstate.HITLUncertain || got == dbstate.HITLRejectedTransport {
		t.Fatalf("a refused send became %s", got)
	}
	susp.set(false) // the Session resumes
	e.waitResponse("act-1", dbstate.HITLAccepted)
}

func TestACredentialRefusalEndsTheScanAndBacksNobodyOff(t *testing.T) {
	e := newEnv(t)
	e.addSession("sess-2", "ks-2", "agent")
	e.k.put(waitingTask("task-a", "msg-a", toolRequest("t1")))
	e.k.set(func() {
		e.k.listErrFor = map[string]error{ksid: &kagent.ConfigError{Msg: "refused", Credentials: true}}
	})
	e.observe()
	for _, k := range []string{ksid, "ks-2"} {
		if n := e.count(`SELECT failures FROM hitl_sessions WHERE kagent_session_id = $1`, k); n != 0 {
			t.Fatalf("%s: failures = %d, want nobody backed off", k, n)
		}
	}
	if e.k.listed("ks-2") != 0 {
		t.Fatal("the scan went on after our own credentials were refused")
	}
	// A refusal of one conversation backs that session off and goes on to the next.
	e.k.set(func() {
		e.k.listErrFor = map[string]error{ksid: &kagent.ConfigError{Msg: "refused"}}
	})
	e.observe()
	if n := e.count(`SELECT failures FROM hitl_sessions WHERE kagent_session_id = $1`, ksid); n != 1 {
		t.Fatalf("failures = %d, want the one session backed off", n)
	}
	if e.k.listed("ks-2") == 0 {
		t.Fatal("the next session was not scanned")
	}
}

func TestTheHoldEndsOnTimeNotAtTheNextDoubledRecheck(t *testing.T) {
	e := newEnv(t, func(s *hitl.Service) {
		s.Opts.UncertainRecheck = 400 * time.Millisecond
		s.Opts.UncertainCeiling = time.Hour
		s.Opts.UncertainHold = 500 * time.Millisecond
	})
	e.k.onSend = func(*fakeKagent, hitl.Send) error { return context.DeadlineExceeded }
	e.k.put(waitingTask("task-a", "msg-a", toolRequest("t1")))
	e.observe()
	req := e.requestFor("task-a")
	e.run()
	if _, err := e.svc.Respond(context.Background(), req.ID, approveAll("t1")); err != nil {
		t.Fatal(err)
	}
	e.waitResponse("act-1", dbstate.HITLUncertain)
	start := time.Now()
	e.waitResponse("act-1", dbstate.HITLRejectedTransport)
	// The first recheck is at 400 ms and would be followed by one at 800 ms; the hold ends at 500 ms.
	if took := time.Since(start); took > 700*time.Millisecond {
		t.Fatalf("the response was failed %s after it became uncertain; the hold is 500 ms", took)
	}
}

func TestACancelledSendLeavesTheResponseRecorded(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.k.onSend = func(*fakeKagent, hitl.Send) error {
		cancel() // the process is stopping as the send starts
		return &kagent.Unreachable{Err: context.Canceled}
	}
	e.k.put(waitingTask("task-a", "msg-a", toolRequest("t1")))
	e.observe()
	req := e.requestFor("task-a")
	if _, err := e.svc.Respond(context.Background(), req.ID, approveAll("t1")); err != nil {
		t.Fatal(err)
	}
	l := e.svc.ResponseLoop()
	l.Interval = 10 * time.Millisecond
	r := reconcile.NewRunner(e.pool, quiet(), nil)
	r.Add(l)
	done := make(chan struct{})
	go func() { _ = r.Run(ctx); close(done) }()
	<-done
	if got := e.response("act-1"); got.State != dbstate.HITLRecorded {
		t.Fatalf("state = %s (detail %q); a send that provably did not leave must go back to recorded", got.State, deref(got.Detail))
	}
}
