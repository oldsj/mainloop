package hitl_test

import (
	"errors"
	"sync"

	"context"
	"github.com/a2aproject/a2a-go/v2/a2a"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/hitl"
)

func (e *env) hint() {
	e.t.Helper()
	if err := hitl.RequestScan(context.Background(), e.svc.Pool, ksid, "task-a"); err != nil {
		e.t.Fatalf("request scan: %v", err)
	}
}

// With a long ScanEvery a session rests after a scan; a park hint makes it due at the next sweep,
// and a burst of hints is one scan.
func TestAParkHintScansTheSessionAtOnceAndABurstCoalesces(t *testing.T) {
	e := newEnv(t, func(s *hitl.Service) { s.Opts.ScanEvery = time.Hour })
	e.observe() // the first scan of a new session; nothing waits yet
	e.k.put(waitingTask("task-a", "msg-a", toolRequest("t1")))
	before := e.k.listed(ksid)

	e.observe()
	if n := len(e.requests()); n != 0 || e.k.listed(ksid) != before {
		t.Fatalf("a resting session was scanned: %d requests, listed %d→%d", n, before, e.k.listed(ksid))
	}

	for range 5 {
		e.hint()
	}
	e.observe()
	if got := e.requestFor("task-a"); got.Availability != dbstate.HITLPending {
		t.Fatalf("request after the hint: %+v", got)
	}
	if n := e.k.listed(ksid) - before; n != 1 {
		t.Fatalf("five hints caused %d scans, want 1", n)
	}

	// The scan consumed the mark.
	listed := e.k.listed(ksid)
	e.observe()
	if e.k.listed(ksid) != listed {
		t.Fatal("the session was scanned again without a hint")
	}
}

// A hint is not a way round a session's failure backoff, and it does nothing for a task whose
// request is already live (the refresh loop keeps that one honest).
func TestAParkHintLeavesBackoffAndLiveRequestsAlone(t *testing.T) {
	e := newEnv(t, func(s *hitl.Service) { s.Opts.ScanEvery = time.Hour })
	e.k.put(waitingTask("task-a", "msg-a", toolRequest("t1")))
	e.observe() // adopts task-a

	e.hint()
	if n := e.count(`SELECT count(*) FROM hitl_sessions WHERE kagent_session_id = $1 AND scanned_at IS NULL`, ksid); n != 0 {
		t.Fatal("a hint marked a session whose task already has a live request")
	}

	if _, err := e.pool.Exec(context.Background(),
		`UPDATE hitl_requests SET availability = 'answered'`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE hitl_sessions SET next_scan_at = now() + interval '1 hour' WHERE kagent_session_id = $1`, ksid); err != nil {
		t.Fatal(err)
	}
	e.hint()
	before := e.k.listed(ksid)
	e.observe()
	if e.k.listed(ksid) != before {
		t.Fatal("a hint scanned a session that is backed off")
	}
}

// wakes records the tasks the dispatcher asked to wake.
type wakes struct {
	mu    sync.Mutex
	calls [][2]string
	err   error
}

func (w *wakes) WakeTurn(_ context.Context, kagentSessionID, taskID string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, [2]string{kagentSessionID, taskID})
	return w.err
}

func (w *wakes) got() [][2]string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([][2]string(nil), w.calls...)
}

// An accepted answer wakes the delivery of its task, after the send and the state change.
func TestAnAcceptedAnswerWakesTheTasksDelivery(t *testing.T) {
	w := &wakes{}
	e := newEnv(t, func(s *hitl.Service) { s.Turns = w })
	e.k.put(waitingTask("task-a", "msg-a", toolRequest("t1")))
	e.observe()
	req := e.requestFor("task-a")
	e.run()

	if _, err := e.svc.Respond(context.Background(), req.ID, approveAll("t1")); err != nil {
		t.Fatal(err)
	}
	e.waitResponse("act-1", dbstate.HITLAccepted)
	deadline := time.Now().Add(5 * time.Second)
	for len(w.got()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := w.got(); len(got) != 1 || got[0] != [2]string{ksid, "task-a"} {
		t.Fatalf("wakes = %v, want one for (%s, task-a)", got, ksid)
	}
}

// An answer kagent has not taken wakes nothing, and a wake that fails does not undo the answer.
func TestAnUncertainAnswerWakesNothingAndAFailedWakeIsOnlyLogged(t *testing.T) {
	w := &wakes{}
	e := newEnv(t, func(s *hitl.Service) { s.Turns = w })
	e.k.onSend = func(*fakeKagent, hitl.Send) error { return context.DeadlineExceeded }
	e.k.put(waitingTask("task-a", "msg-a", toolRequest("t1")))
	e.observe()
	req := e.requestFor("task-a")
	e.run()
	if _, err := e.svc.Respond(context.Background(), req.ID, approveAll("t1")); err != nil {
		t.Fatal(err)
	}
	e.waitResponse("act-1", dbstate.HITLUncertain)
	time.Sleep(150 * time.Millisecond)
	if got := w.got(); len(got) != 0 {
		t.Fatalf("wakes for an unconfirmed answer = %v", got)
	}

	// The message turns out to have arrived; the wake fails, and the answer is accepted anyway.
	w.mu.Lock()
	w.err = errors.New("db down")
	w.mu.Unlock()
	e.k.set(func() {
		task := e.k.tasks["task-a"]
		task.History = append(task.History, userMessage(e.k.sends[0]))
		task.Status = a2a.TaskStatus{State: a2a.TaskStateWorking}
	})
	e.waitResponse("act-1", dbstate.HITLAccepted)
	deadline := time.Now().Add(5 * time.Second)
	for len(w.got()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := w.got(); len(got) == 0 {
		t.Fatal("no wake after the answer was confirmed")
	}
}
