// Ports of the review-3 probes: each asserts the safety outcome the reviewer demonstrated broken.
// A stop is confirmed only by the observer, from what kagent reports for the whole session.

package tasks_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	"github.com/oldsj/mainloop/server/internal/sessions"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

// Only the methods used by the real session cancel and stop observer are called.
type stopProbeClient struct {
	sessions.Kagent
	k              *fakeKagent
	foundReadError bool
	cancelError    bool
	mu             sync.Mutex
	contexts       []string
}

func (c *stopProbeClient) GetTask(ctx context.Context, a kagent.AgentRef, id string) (*a2a.Task, error) {
	return c.k.GetTask(ctx, a, id)
}

func (c *stopProbeClient) ListTasks(ctx context.Context, a kagent.AgentRef, contextID string) ([]*a2a.Task, error) {
	c.mu.Lock()
	c.contexts = append(c.contexts, contextID)
	c.mu.Unlock()
	return c.k.ListTasks(ctx, a, contextID)
}

func (c *stopProbeClient) FindTaskForMessage(ctx context.Context, a kagent.AgentRef, sid, mid string) (*a2a.Task, bool, error) {
	c.mu.Lock()
	c.contexts = append(c.contexts, sid)
	c.mu.Unlock()
	if c.foundReadError {
		// The real client returns this triple when ListTasks finds the message but GetTask fails.
		return nil, true, errors.New("injected GetTask read failure after ListTasks found the message")
	}
	return c.k.FindTaskForMessage(ctx, a, sid, mid)
}

func (c *stopProbeClient) setCancelError(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancelError = v
}

func (c *stopProbeClient) CancelTask(ctx context.Context, a kagent.AgentRef, id string) (*a2a.Task, error) {
	c.mu.Lock()
	failing := c.cancelError
	c.mu.Unlock()
	if failing {
		return nil, errors.New("injected uncertain CancelTask result")
	}
	return c.k.CancelTask(ctx, a, id)
}

func stopProbeProvision(t *testing.T) (*rig, func(), tasks.Principal, string, string, string) {
	t.Helper()
	r := newRig(t)
	stop := r.run()
	_, sup := r.supervisor()
	child := r.codeChild(sup, "feat")
	r.eventually("child", func() bool { return r.opState(child) == "completed" })
	stop()
	attempt, session := r.attemptOf(child)
	return r, stop, sup, child, attempt, session
}

func TestStopFoundButUnreadableTaskIsNotStopped(t *testing.T) {
	for _, age := range []string{"now()", "now() - interval '31 minutes'"} {
		t.Run(age, func(t *testing.T) {
			r, _, _, _, attempt, session := stopProbeProvision(t)
			k := &fakeKagent{}
			k.land(a2a.TaskStateWorking)
			c := &stopProbeClient{k: k, foundReadError: true}
			obs := &tasks.KagentStops{Pool: r.pool, OwnerID: owner, Sessions: r.ses.Service, Kagent: c}
			r.briefRow(attempt, `state='cancelled', kagent_session_id='ks-' || session_id, updated_at=`+age)
			stopped, err := obs.Stopped(context.Background(), session)
			_, cancels := k.snapshot()
			t.Logf("found=true, task=nil, GetTask error: stopped=%v error=%v cancels=%d, runtime=working", stopped, err, cancels)
			if stopped {
				t.Error("confirmed a stop although kagent found the message and its task could not be read")
			}
		})
	}
}

func TestStopLooksInTheDeliverysOwnSessionAndRenewsTheHold(t *testing.T) {
	r, _, _, _, attempt, session := stopProbeProvision(t)
	k := &fakeKagent{}
	c := &stopProbeClient{k: k}
	obs := &tasks.KagentStops{Pool: r.pool, OwnerID: owner, Sessions: r.ses.Service, Kagent: c}
	r.exec(`UPDATE agent_bindings SET kagent_session_id=NULL WHERE session_id=$1`, session)
	r.briefRow(attempt, `state='cancelled', kagent_session_id='delivery-own-session', updated_at=now()-interval '31 minutes'`)
	mustStopped(t, obs, session, true, "past the hold")
	r.briefRow(attempt, `updated_at=now()`)
	mustStopped(t, obs, session, false, "updated_at moved after the cancellation")
	k.land(a2a.TaskStateWorking)
	mustStopped(t, obs, session, false, "own session's landed task")
	mustStopped(t, obs, session, true, "after cancellation")
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, sid := range c.contexts {
		if sid != "delivery-own-session" {
			t.Errorf("looked up binding session %q, expected delivery's own session", sid)
		}
	}
	t.Logf("binding session=NULL; all %d lookups used the delivery session; updated_at renewed hold", len(c.contexts))
}

func TestStopOfAnAlreadyCancelledSessionIsStillObserved(t *testing.T) {
	for _, mode := range []string{"owner cancelled session first", "crash after session cancel before attempt evidence", "binding session nil, delivery session present"} {
		t.Run(mode, func(t *testing.T) {
			r, _, sup, child, attempt, session := stopProbeProvision(t)
			k := &fakeKagent{}
			c := &stopProbeClient{k: k}
			r.ses.Kagent = c
			// Use the real sessions.Service.Cancel, without the rig's answer override.
			r.svc.Sessions = r.ses.Service
			r.svc.StopObserver = &tasks.KagentStops{Pool: r.pool, OwnerID: owner, Sessions: r.ses.Service, Kagent: c}
			r.briefRow(attempt, `state='sending', kagent_session_id='ks-' || session_id`)
			cancelTask := func() {
				t.Helper()
				if _, err := r.svc.Cancel(context.Background(), r.own, action(child, r.view(r.own, child), uuid.NewString())); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "crash after session cancel before attempt evidence" {
				cancelTask()
				// This is the committed state after drain, before stop's Cancel call returns.
				r.exec(`UPDATE task_attempts SET state='draining', stop_state='requested', updated_at=now() WHERE id=$1`, attempt)
			}
			if mode == "binding session nil, delivery session present" {
				r.exec(`UPDATE agent_bindings SET kagent_session_id=NULL WHERE session_id=$1`, session)
			} else {
				res, err := r.ses.Service.Cancel(context.Background(), session)
				if err != nil || res.Agent != sessions.AgentUnknown {
					t.Fatalf("first session cancel: result=%+v error=%v", res, err)
				}
				t.Logf("first real session cancel: agent=%s; ledger=%s", res.Agent, r.str(`SELECT state FROM deliveries WHERE id=(SELECT brief_delivery_id FROM task_attempts WHERE id=$1)`, attempt))
			}
			k.land(a2a.TaskStateWorking)
			if mode != "crash after session cancel before attempt evidence" {
				cancelTask()
			}
			r.run()
			r.eventually("attempt ended", func() bool { return r.attemptState(child) == "cancelled" })
			slot, claim := r.held(attempt)
			finds, cancels := k.snapshot()
			k.mu.Lock()
			nativeState := k.state
			k.mu.Unlock()
			t.Logf("after task cancel: attempt=%s task=%s slot=%d claim=%d finds=%d cancels=%d evidence=%q runtime=%s", r.attemptState(child), r.taskStatus(child), slot, claim, finds, cancels, r.str(`SELECT array_to_string(evidence_refs, ',') FROM task_attempts WHERE id=$1`, attempt), nativeState)
			second := r.codeChild(sup, "feat")
			r.eventually("second writer", func() bool { return r.opState(second) == "completed" })
			t.Logf("second writer: state=%s generation=%d", r.attemptState(second), r.count(`SELECT writer_generation FROM task_attempts WHERE task_id=$1`, second))
			if !nativeState.Terminal() {
				t.Error("the attempt ended while its native turn remained working")
			}
			if slot != 0 || claim != 0 {
				t.Errorf("slot=%d claim=%d after the stop was confirmed", slot, claim)
			}
		})
	}
}

func TestStopSeesANativeTaskWithoutADelivery(t *testing.T) {
	r, _, sup, child, attempt, session := stopProbeProvision(t)
	f := kagenttest.New()
	t.Cleanup(f.Close)
	c, err := kagent.New(kagent.Config{GatewayURL: f.URL, UserID: "test-owner", RequestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ksid := "ks-" + session
	f.AddTask(&a2a.Task{ID: "tracked-completed", ContextID: ksid, Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}})
	// A native continuation or another client starts a task in this context without a Mainloop delivery.
	f.AddTask(&a2a.Task{ID: "native-untracked", ContextID: ksid, Status: a2a.TaskStatus{State: a2a.TaskStateWorking}})
	r.briefRow(attempt, `state='completed', task_id='tracked-completed', kagent_session_id='ks-' || session_id`)
	r.ses.Kagent = c
	r.svc.Sessions = r.ses.Service
	obs := &tasks.KagentStops{Pool: r.pool, OwnerID: owner, Sessions: r.ses.Service, Kagent: c}
	r.svc.StopObserver = obs
	stopped, err := obs.Stopped(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := c.ListTasks(context.Background(), kagent.AgentRef{Namespace: "kagent", Name: "agent"}, ksid)
	if err != nil {
		t.Fatal(err)
	}
	working := 0
	for _, task := range listed {
		if !task.Status.State.Terminal() {
			working++
		}
	}
	t.Logf("observer stopped=%v; real client ListTasks returned %d tasks, %d working", stopped, len(listed), working)
	if _, err := r.svc.Cancel(context.Background(), r.own, action(child, r.view(r.own, child), uuid.NewString())); err != nil {
		t.Fatal(err)
	}
	r.run()
	r.eventually("attempt cancelled", func() bool { return r.attemptState(child) == "cancelled" })
	slot, claim := r.held(attempt)
	native, ok := f.Task("native-untracked")
	if !ok {
		t.Fatal("native task disappeared")
	}
	second := r.codeChild(sup, "feat")
	r.eventually("second writer", func() bool { return r.opState(second) == "completed" })
	t.Logf("after task cancel: slot=%d claim=%d native task=%s second generation=%d", slot, claim, native.Status.State, r.count(`SELECT writer_generation FROM task_attempts WHERE task_id=$1`, second))
	if stopped || (!native.Status.State.Terminal() && (slot != 1 || claim != 1)) {
		t.Error("native task was missed and its writer claim was released")
	}
}

type stopProbeEndBeforeBrief struct {
	tasks.SessionRuntime
	real *sessions.Service
	r    *rig
	once sync.Once
}

func (s *stopProbeEndBeforeBrief) SendTo(ctx context.Context, sid, mid, text string) (sessions.Sent, error) {
	var workspace bool
	if err := s.r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workspaces WHERE id=$1)`, sid).Scan(&workspace); err != nil {
		return sessions.Sent{}, err
	}
	var cancelErr error
	if workspace {
		s.once.Do(func() { _, cancelErr = s.real.Cancel(ctx, sid) })
	}
	if cancelErr != nil {
		return sessions.Sent{}, cancelErr
	}
	return s.real.SendTo(ctx, sid, mid, text)
}

// A session that ended between activation and the first send can never take the brief: the create
// operation gives up with evidence and a reconciliation block instead of retrying for ever.
func TestInitialBriefToAnEndedSessionGivesUp(t *testing.T) {
	r := newRig(t)
	r.svc.Sessions = &stopProbeEndBeforeBrief{SessionRuntime: r.ses, real: r.ses.Service, r: r}
	r.run()
	_, sup := r.supervisor()
	child := r.codeChild(sup, "feat")
	r.eventually("create given up", func() bool { return r.opState(child) == "blocked" })
	attempt, session := r.attemptOf(child)
	r.sweep(r.svc.BriefSweep())
	if got := r.str(`SELECT status FROM sessions WHERE id=$1`, session); got != "cancelled" {
		t.Fatalf("session = %s: the test would prove nothing", got)
	}
	if got, reason := r.taskStatus(child), r.str(`SELECT reason FROM tasks WHERE id = $1`, child); got != "blocked" || reason != "reconciliation" {
		t.Fatalf("task = %s/%s, want blocked/reconciliation", got, reason)
	}
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND 'brief-delivery-failed' = ANY (evidence_refs) AND brief_delivery_id IS NULL`, attempt); n != 1 {
		t.Fatal("no brief-delivery-failed evidence")
	}
	// The operation is not driven again.
	attempts := r.count(`SELECT attempts FROM task_operations WHERE task_id=$1 AND kind='create'`, child)
	time.Sleep(300 * time.Millisecond)
	r.exec(`UPDATE task_operations SET next_attempt_at=now() WHERE task_id=$1 AND kind='create'`, child)
	time.Sleep(300 * time.Millisecond)
	if got := r.count(`SELECT attempts FROM task_operations WHERE task_id=$1 AND kind='create'`, child); got != attempts {
		t.Fatalf("create operation was driven again: %d -> %d", attempts, got)
	}
}

// Filling the bounded evidence array does not touch the stop state, which lives in its own column: the
// stop stays requested while the observer cannot cancel the native task, and only an observation of a
// stopped session releases the claim and slot.
func TestStopDoesNotDependOnTheEvidenceCap(t *testing.T) {
	r, _, sup, child, attempt, _ := stopProbeProvision(t)
	k := &fakeKagent{}
	k.land(a2a.TaskStateWorking)
	c := &stopProbeClient{k: k}
	c.setCancelError(true)
	r.ses.Kagent = c
	r.svc.Sessions = r.ses.Service
	r.svc.StopObserver = &tasks.KagentStops{Pool: r.pool, OwnerID: owner, Sessions: r.ses.Service, Kagent: c}
	r.briefRow(attempt, `state='delivered', task_id='task-1', kagent_session_id='ks-' || session_id`)
	r.exec(`UPDATE task_attempts SET evidence_refs=ARRAY(SELECT 'native-delivery-failed:' || n::text FROM generate_series(1,64) n) WHERE id=$1`, attempt)
	if _, err := r.svc.Cancel(context.Background(), r.own, action(child, r.view(r.own, child), uuid.NewString())); err != nil {
		t.Fatal(err)
	}
	r.run()
	r.eventually("stop looked at", func() bool {
		return r.count(`SELECT resolve_attempts FROM task_attempts WHERE id=$1`, attempt) >= 1
	})
	second := r.codeChild(sup, "feat")
	time.Sleep(300 * time.Millisecond)
	if slot, claim := r.held(attempt); slot != 1 || claim != 1 {
		t.Fatalf("held slot=%d claim=%d with the native task unobserved, want both", slot, claim)
	}
	if got := r.str(`SELECT state || '/' || stop_state FROM task_attempts WHERE id=$1`, attempt); got != "draining/requested" && got != "draining/unresolved" {
		t.Fatalf("attempt = %s, want draining with an unconfirmed stop", got)
	}
	if n := r.count(`SELECT cardinality(evidence_refs) FROM task_attempts WHERE id=$1`, attempt); n != 64 {
		t.Fatalf("evidence = %d entries: the test needs the array full", n)
	}
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE task_id = $1`, second); n != 0 {
		t.Fatal("a second writer got the branch while the native task was unobserved")
	}
	// kagent answers again: the observer cancels the native task, sees it stopped, and releases.
	c.setCancelError(false)
	r.eventually("stop confirmed", func() bool { return r.attemptState(child) == "cancelled" })
	if slot, claim := r.held(attempt); slot != 0 || claim != 0 {
		t.Fatalf("held slot=%d claim=%d after the observation", slot, claim)
	}
	r.eventually("second writer", func() bool { return r.opState(second) == "completed" })
	if g := r.count(`SELECT writer_generation FROM task_attempts WHERE task_id=$1`, second); g != 2 {
		t.Fatalf("second generation = %d", g)
	}
}

func stopProbeDeadlock(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && (pg.Code == "40P01" || pg.Code == "40001")
}

// Parent and child cancellation operations, child reports, and all settle steps contend on locks.
func TestConcurrentParentAndChildCancelsDoNotDeadlock(t *testing.T) {
	for round := range 4 {
		t.Run(fmt.Sprint(round), func(t *testing.T) {
			r := newRig(t)
			r.ses.agent = "unknown"
			r.svc.StopObserver = r.stops
			r.svc.Limits.MaxChildrenPerParent = 8
			r.svc.Limits.MaxChildrenGlobal = 16
			r.svc.Limits.ResolveBound = 2
			r.run()
			root, sup := r.supervisor()
			children := make([]string, 6)
			kids := make([]tasks.Principal, 6)
			for i := range children {
				children[i] = r.codeChild(sup, fmt.Sprintf("branch-%d", i))
				r.eventually("child", func() bool { return r.opState(children[i]) == "completed" })
				_, binding := r.attemptOf(children[i])
				kids[i] = r.agent(dbstate.RoleChild, binding)
			}
			r.exec(`SELECT pg_stat_clear_snapshot()`)
			before := r.count(`SELECT deadlocks::int FROM pg_stat_database WHERE datname=current_database()`)
			var wg sync.WaitGroup
			errs := make(chan error, 100)
			start := make(chan struct{})
			for _, id := range append(children, root) {
				v := r.view(r.own, id)
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					_, err := r.svc.Cancel(context.Background(), r.own, action(id, v, uuid.NewString()))
					errs <- err
				}()
			}
			for _, kid := range kids {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					_, err := r.svc.Report(context.Background(), kid, report(kid, uuid.NewString(), dbstate.OutcomeProgress))
					errs <- err
				}()
			}
			close(start)
			wg.Wait()
			close(errs)
			direct := 0
			for err := range errs {
				if stopProbeDeadlock(err) {
					direct++
					t.Errorf("deadlock: %v", err)
				}
			}
			r.eventually("parent drains and children cancelled", func() bool {
				return r.attemptState(root) == "draining" && r.count(`SELECT count(*) FROM tasks WHERE parent_task_id=$1 AND status='cancelled'`, root) == 6
			})
			for range 5 {
				r.sweep(r.svc.SettleSweep())
				r.sweep(r.svc.CompleteSweep())
				r.sweep(r.svc.BriefSweep())
			}
			r.stops.set(true)
			r.eventually("all attempts settled", func() bool {
				r.sweep(r.svc.SettleSweep())
				return r.count(`SELECT count(*) FROM task_attempts WHERE state NOT IN ('completed','failed','cancelled')`) == 0
			})
			time.Sleep(time.Second)
			r.exec(`SELECT pg_stat_clear_snapshot()`)
			after := r.count(`SELECT deadlocks::int FROM pg_stat_database WHERE datname=current_database()`)
			held := r.count(`SELECT count(*) FROM workspace_writer_claims WHERE held`)
			capacity := r.count(`SELECT count(*) FROM task_attempts WHERE capacity_held`)
			t.Logf("round %d: parent/child concurrent cancels=7 reports=6 direct40P01/40001=%d pg_deadlocks=%d held=%d capacity=%d", round, direct, after-before, held, capacity)
			if after != before || held != 0 || capacity != 0 {
				t.Error("deadlock or leaked claim/capacity")
			}
		})
	}
}

// A crash after the attempt was moved to draining and before any kagent call leaves stop_state
// 'requested' and nothing else. The settle sweep alone resolves it: the observer finds the native task
// still working, cancels it, and only a later look that sees it stopped confirms and fences.
func TestStopRequestedBeforeAnyCancelIsResolvedBySweep(t *testing.T) {
	r, _, _, child, attempt, _ := stopProbeProvision(t)
	k := &fakeKagent{}
	k.land(a2a.TaskStateWorking)
	c := &stopProbeClient{k: k}
	r.ses.Kagent = c
	r.svc.Sessions = r.ses.Service
	r.svc.StopObserver = &tasks.KagentStops{Pool: r.pool, OwnerID: owner, Sessions: r.ses.Service, Kagent: c}
	r.briefRow(attempt, `state='delivered', task_id='task-1', kagent_session_id='ks-' || session_id`)
	r.exec(`UPDATE task_attempts SET state='draining', stop_state='requested', updated_at=now() - interval '1 hour' WHERE id=$1`, attempt)
	r.exec(`UPDATE tasks SET status='cancelled', reason=NULL WHERE id=$1`, child)
	if slot, claim := r.held(attempt); slot != 1 || claim != 1 {
		t.Fatalf("seed: slot=%d claim=%d", slot, claim)
	}
	if got := r.str(`SELECT stop_state FROM task_attempts WHERE id=$1`, attempt); got != "requested" {
		t.Fatalf("seed stop_state = %s", got)
	}
	r.eventually("attempt ended by the sweep", func() bool {
		r.sweep(r.svc.SettleSweep())
		return r.count(`SELECT count(*) FROM task_attempts WHERE id=$1 AND state IN ('fenced', 'failed', 'cancelled', 'completed')`, attempt) == 1
	})
	if got := r.str(`SELECT stop_state FROM task_attempts WHERE id=$1`, attempt); got != "confirmed" {
		t.Fatalf("stop_state = %s, want confirmed", got)
	}
	_, cancels := k.snapshot()
	k.mu.Lock()
	nativeState := k.state
	k.mu.Unlock()
	if cancels < 1 || !nativeState.Terminal() {
		t.Fatalf("native task: cancels=%d state=%v, want it cancelled by the observer", cancels, nativeState)
	}
	if slot, claim := r.held(attempt); slot != 0 || claim != 0 {
		t.Fatalf("slot=%d claim=%d after confirmation", slot, claim)
	}
}
