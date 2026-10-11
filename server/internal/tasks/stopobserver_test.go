package tasks_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

// fakeKagent is kagent for the real KagentStops observer. Until it has landed, kagent has not seen
// the in-flight message: no task carries it. Once landed, the message has a task in state.
type fakeKagent struct {
	mu                          sync.Mutex
	landed                      bool
	state                       a2a.TaskState
	finds, gets, cancels, lists int
	// gone makes the task disappear: it is no longer listed or found.
	gone bool
	// contexts are the contexts ListTasks was asked about.
	contexts []string
}

const fakeNativeTask = "native-1"

func (k *fakeKagent) GetTask(_ context.Context, _ kagent.AgentRef, id string) (*a2a.Task, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.gets++
	if k.gone || !k.landed {
		return nil, &kagent.TaskNotFound{}
	}
	return &a2a.Task{ID: a2a.TaskID(id), Status: a2a.TaskStatus{State: k.state}}, nil
}

func (k *fakeKagent) ListTasks(_ context.Context, _ kagent.AgentRef, contextID string) ([]*a2a.Task, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.lists++
	k.contexts = append(k.contexts, contextID)
	if k.gone || !k.landed {
		return nil, nil
	}
	return []*a2a.Task{{ID: fakeNativeTask, ContextID: contextID, Status: a2a.TaskStatus{State: k.state}}}, nil
}

func (k *fakeKagent) FindTaskForMessage(_ context.Context, _ kagent.AgentRef, _, msg string) (*a2a.Task, bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.finds++
	if k.gone || !k.landed {
		return nil, false, nil
	}
	return &a2a.Task{ID: fakeNativeTask, Status: a2a.TaskStatus{State: k.state}}, true, nil
}

func (k *fakeKagent) CancelTask(_ context.Context, _ kagent.AgentRef, id string) (*a2a.Task, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.cancels++
	if k.landed {
		k.state = a2a.TaskStateCanceled
	}
	return &a2a.Task{ID: a2a.TaskID(id), Status: a2a.TaskStatus{State: a2a.TaskStateCanceled}}, nil
}

func (k *fakeKagent) snapshot() (finds, cancels int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.finds, k.cancels
}

func (k *fakeKagent) land(st a2a.TaskState) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.landed, k.state = true, st
}

// sessionStops answers per session; a session not set is still running.
type sessionStops struct {
	mu sync.Mutex
	m  map[string]bool
}

func (s *sessionStops) Stopped(_ context.Context, sid string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[sid], nil
}

func (s *sessionStops) set(sid string, v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[sid] = v
}

// kagentStops installs the real observer over a fake kagent and provisions one code child; it returns
// the child, its attempt and its session. The child's brief row is in the ledger as recorded.
func (r *rig) kagentStops(k *fakeKagent) (obs *tasks.KagentStops, sup tasks.Principal, child, attempt, session string) {
	r.t.Helper()
	obs = &tasks.KagentStops{Pool: r.pool, OwnerID: owner, Sessions: r.ses.Service, Kagent: k}
	r.svc.StopObserver = obs
	r.run()
	_, sup = r.supervisor()
	child = r.codeChild(sup, "feat")
	r.eventually("child", func() bool { return r.opState(child) == "completed" })
	attempt, session = r.attemptOf(child)
	return obs, sup, child, attempt, session
}

func (r *rig) briefRow(attempt, set string, args ...any) {
	r.t.Helper()
	r.exec(`UPDATE deliveries SET `+set+` WHERE id = (SELECT brief_delivery_id FROM task_attempts WHERE id = $1)`, append([]any{attempt}, args...)...)
}

func mustStopped(t *testing.T, obs *tasks.KagentStops, session string, want bool, what string) {
	t.Helper()
	got, err := obs.Stopped(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s: stopped = %v, want %v", what, got, want)
	}
}

// A message that was sent and that no task carries yet may still land: the observer holds it as
// possibly running until the sessions ledger's hold has passed.
func TestKagentStopsSentWithNoTaskInsideTheHoldIsNotStopped(t *testing.T) {
	r := newRig(t)
	k := &fakeKagent{}
	obs, _, _, attempt, session := r.kagentStops(k)
	r.briefRow(attempt, `state = 'cancelled', kagent_session_id = 'ks-' || session_id, updated_at = now()`)
	mustStopped(t, obs, session, false, "sent, no task, inside the hold")
	if _, cancels := k.snapshot(); cancels != 0 {
		t.Fatalf("cancels = %d with no task to cancel", cancels)
	}
	// Still not stopped on the next look: the hold is on the row's age, not on the number of looks.
	mustStopped(t, obs, session, false, "sent, no task, second look")
}

func TestKagentStopsCancelsALandedTaskThenSeesItStopped(t *testing.T) {
	r := newRig(t)
	k := &fakeKagent{}
	obs, _, _, attempt, session := r.kagentStops(k)
	r.briefRow(attempt, `state = 'cancelled', kagent_session_id = 'ks-' || session_id`)
	k.land(a2a.TaskStateWorking)
	mustStopped(t, obs, session, false, "task found running")
	if _, cancels := k.snapshot(); cancels != 1 {
		t.Fatalf("cancels = %d, want 1", cancels)
	}
	mustStopped(t, obs, session, true, "next look")
}

func TestKagentStopsLandedTaskRecordedOnTheRow(t *testing.T) {
	r := newRig(t)
	k := &fakeKagent{}
	obs, _, _, attempt, session := r.kagentStops(k)
	r.briefRow(attempt, `state = 'delivered', task_id = 'task-1', kagent_session_id = 'ks-' || session_id`)
	k.land(a2a.TaskStateWorking)
	mustStopped(t, obs, session, false, "recorded task running")
	mustStopped(t, obs, session, true, "recorded task cancelled")
	// A task kagent no longer has is gone, whatever the row's age.
	k.mu.Lock()
	k.gone = true
	k.mu.Unlock()
	mustStopped(t, obs, session, true, "recorded task gone")
}

func TestKagentStopsNeverSentIsStopped(t *testing.T) {
	r := newRig(t)
	k := &fakeKagent{}
	obs, _, _, attempt, session := r.kagentStops(k)
	r.briefRow(attempt, `state = 'cancelled', kagent_session_id = NULL, updated_at = now()`)
	mustStopped(t, obs, session, true, "never sent")
	if finds, cancels := k.snapshot(); finds != 0 || cancels != 0 {
		t.Fatalf("kagent was asked about a message that was never sent: finds=%d cancels=%d", finds, cancels)
	}
	// Deciding is per delivery: the binding's own kagent Session does not make the row a sent one.
	if n := r.count(`SELECT count(*) FROM agent_bindings WHERE session_id = $1 AND kagent_session_id IS NOT NULL`, session); n != 1 {
		t.Fatalf("binding has no kagent session (%d): the test would prove nothing", n)
	}
}

func TestKagentStopsPastTheHoldIsStopped(t *testing.T) {
	r := newRig(t)
	k := &fakeKagent{}
	obs, _, _, attempt, session := r.kagentStops(k)
	r.briefRow(attempt, `state = 'cancelled', kagent_session_id = 'ks-' || session_id, updated_at = now() - interval '31 minutes'`)
	mustStopped(t, obs, session, true, "sent, no task, past the 30 minute hold")
	// The hold is the observer's setting when it has one.
	obs.UncertainHold = time.Hour
	mustStopped(t, obs, session, false, "sent, no task, inside a longer hold")
}

// The reviewer's probe, now asserting the fix: the owner cancels while the brief's send is in flight.
// The observer cannot see a task, so the stop stays unconfirmed, the attempt keeps its branch claim
// and capacity slot, and only when the message lands and is stopped are they released.
func TestInFlightSendKeepsTheClaimUntilItsTurnIsStopped(t *testing.T) {
	r := newRig(t)
	r.ses.agent = "unknown"
	k := &fakeKagent{}
	_, sup, first, attempt, _ := r.kagentStops(k)
	// The sessions delivery loop committed "sending" for the brief; its POST to kagent is in flight.
	r.briefRow(attempt, `state = 'sending', kagent_session_id = 'ks-' || session_id`)
	if _, err := r.svc.Cancel(context.Background(), r.own, action(first, r.view(r.own, first), "c1")); err != nil {
		t.Fatal(err)
	}
	r.eventually("looked at", func() bool { f, _ := k.snapshot(); return f > 1 })
	if got := r.str(`SELECT state || '/' || coalesce(task_id, '<no task>') FROM deliveries WHERE id = (SELECT brief_delivery_id FROM task_attempts WHERE id = $1)`, attempt); got != "cancelled/<no task>" {
		t.Fatalf("brief row = %s: the cancel did not close the in-flight row", got)
	}

	// A second child on the branch cannot take it: the first attempt may still be running.
	second := r.codeChild(sup, "feat")
	for range 5 {
		r.sweep(r.svc.SettleSweep())
		time.Sleep(30 * time.Millisecond)
	}
	if got := r.attemptState(first); got != "draining" {
		t.Fatalf("first attempt = %s, want draining while a send may still land", got)
	}
	if slot, claim := r.held(attempt); slot != 1 || claim != 1 {
		t.Fatalf("held slot=%d claim=%d, want both", slot, claim)
	}
	if got := r.taskStatus(first); got == "cancelled" {
		t.Fatal("the task ended cancelled with the stop unobserved")
	}
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE task_id = $1`, second); n != 0 {
		t.Fatalf("second child got an attempt on a held branch: %d", n)
	}

	// The message lands and kagent starts its turn: the resolver stops it, then sees it stopped.
	k.land(a2a.TaskStateWorking)
	r.eventually("first ended", func() bool {
		r.sweep(r.svc.SettleSweep())
		return r.attemptState(first) == "cancelled"
	})
	if _, cancels := k.snapshot(); cancels < 1 {
		t.Fatalf("cancels = %d: the landed turn was never stopped", cancels)
	}
	if slot, claim := r.held(attempt); slot != 0 || claim != 0 {
		t.Fatalf("held slot=%d claim=%d after the stop was observed", slot, claim)
	}
	r.eventually("second gets the branch", func() bool {
		r.sweep(r.svc.SettleSweep())
		return r.opState(second) == "completed" && r.attemptState(second) == "active"
	})
}

// Cancelling a supervisor reaches its children at once, in the cancel's own transaction, even while
// the supervisor's stop stays unconfirmed.
func TestCancelledSupervisorCancelsChildrenWhileItsStopIsUnresolved(t *testing.T) {
	r := newRig(t)
	r.ses.agent = "unknown"
	ss := &sessionStops{m: map[string]bool{}}
	r.svc.StopObserver = ss
	r.svc.Limits.ResolveBound = 2
	r.run()
	root, sup := r.supervisor()
	child := r.codeChild(sup, "feat")
	r.eventually("child", func() bool { return r.opState(child) == "completed" })
	childAttempt, _ := r.attemptOf(child)
	rootAttempt, rootSession := r.attemptOf(root)

	if _, err := r.svc.Cancel(context.Background(), r.own, action(root, r.view(r.own, root), "c-root")); err != nil {
		t.Fatal(err)
	}
	r.eventually("root stop unresolved", func() bool {
		return r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND stop_state = 'unresolved'`, rootAttempt) == 1
	})
	if got := r.attemptState(root); got != "draining" {
		t.Fatalf("root attempt = %s, want draining", got)
	}
	// The parent is still draining, and its child has already been told to stop.
	r.eventually("child task cancelled", func() bool { return r.taskStatus(child) == "cancelled" })
	r.eventually("child attempt draining or ended", func() bool {
		r.sweep(r.svc.SettleSweep())
		st := r.attemptState(child)
		return st == "draining" || st == "cancelled"
	})
	if got := r.attemptState(root); got != "draining" {
		t.Fatalf("root attempt = %s: the child cascade waited for the parent's stop", got)
	}

	// Everything resolves once kagent confirms.
	ss.set(rootSession, true)
	_, childSession := r.attemptOf(child)
	ss.set(childSession, true)
	r.eventually("all ended", func() bool {
		r.sweep(r.svc.SettleSweep())
		return r.attemptState(root) == "cancelled" && r.attemptState(child) == "cancelled"
	})
	if slot, claim := r.held(childAttempt); slot != 0 || claim != 0 {
		t.Fatalf("held slot=%d claim=%d", slot, claim)
	}
}
