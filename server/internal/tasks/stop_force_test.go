// A runtime that never answers a cancel (crashed, suspended, or wedged inside a turn) must not hold its
// attempt's claim and capacity, or the owner's attention, for good. Once a requested stop has waited
// long enough the resolver has the observer suspend the runtime, and when that does not settle it
// delete the native session. How long it has waited is time since the request, not a number of looks.
// A turn that is still open cannot be suspended: kagent refuses a suspend while a task is working, so
// an unresponsive running turn is deleted, and only a runtime with no open turn is suspended.

package tasks_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/tasks"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// fakeNativeSession is the kagent session the observer checks and deletes. Deleting moves it to
// afterDelete, so a test chooses whether the delete is final or still on its way out.
type fakeNativeSession struct {
	mu          sync.Mutex
	live        kagent.SessionLiveness
	afterDelete kagent.SessionLiveness
	deleteErr   error
	deletes     int
}

func (f *fakeNativeSession) ObserveLiveness(context.Context, string) (kagent.SessionLiveness, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live, nil
}

func (f *fakeNativeSession) DeleteSession(context.Context, string) (*kagentapi.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes++
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	f.live = f.afterDelete
	return &kagentapi.Session{}, nil
}

func (f *fakeNativeSession) set(live kagent.SessionLiveness) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.live = live
}

func (f *fakeNativeSession) setDeleteErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteErr = err
}

func (f *fakeNativeSession) deleteCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deletes
}

// fakeSuspender is the workspace service's suspend. It parks the native session, or refuses the way
// kagent does while one of the session's tasks is still working (admitAgentLifecycle answers
// Aborted); any task that is not working lets the suspend through.
type fakeSuspender struct {
	mu      sync.Mutex
	native  *fakeNativeSession
	k       *fakeKagent
	suspend int
}

func (f *fakeSuspender) Suspend(context.Context, string) (api.WorkspaceLifecycle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.suspend++
	if f.k.working() {
		return api.WorkspaceLifecycle{}, &kagent.SessionError{
			Op: "AgentService SuspendAgent", Code: connect.CodeAborted, Detail: "a task is working", Wire: true,
		}
	}
	f.native.set(kagent.SessionLiveness{Exists: true, Suspended: true})
	return api.WorkspaceLifecycle{}, nil
}

func (f *fakeSuspender) suspends() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.suspend
}

type stuck struct {
	r       *rig
	native  *fakeNativeSession
	suspend *fakeSuspender
	client  *stopProbeClient
	k       *fakeKagent
	child   string
	attempt string
	session string
}

// stuckStop provisions a cancelled child whose native runtime never answers: every cancel is an
// uncertain timeout, as in prod, and the session reads present and not terminal until suspended or
// deleted. The turn is working, so the workspace service's suspend refuses, as kagent does while a
// turn is open, unless a test lands the turn in another state.
func stuckStop(t *testing.T) *stuck {
	t.Helper()
	r, _, _, child, attempt, session := stopProbeProvision(t)
	k := &fakeKagent{}
	k.land(a2a.TaskStateWorking)
	c := &stopProbeClient{k: k}
	c.setCancelError(true)
	native := &fakeNativeSession{live: kagent.SessionLiveness{Exists: true}}
	susp := &fakeSuspender{native: native, k: k}
	r.ses.Kagent = c
	r.svc.Sessions = r.ses.Service
	r.svc.StopObserver = &tasks.KagentStops{
		Pool: r.pool, OwnerID: owner, Sessions: r.ses.Service, Kagent: c, Liveness: native, Workspaces: susp, Deleter: native,
	}
	r.exec(`UPDATE agent_bindings SET kagent_session_id = 'ks-native' WHERE session_id = $1`, session)
	r.briefRow(attempt, `state='delivered', task_id='task-1', kagent_session_id='ks-native'`)
	return &stuck{r: r, native: native, suspend: susp, client: c, k: k, child: child, attempt: attempt, session: session}
}

func cancelChild(t *testing.T, r *rig, child string) {
	t.Helper()
	sup := r.own
	if _, err := r.svc.Cancel(context.Background(), sup, action(child, r.view(sup, child), uuid.NewString())); err != nil {
		t.Fatal(err)
	}
}

// cancelAndWait cancels the child, starts the loops and returns once the attempt is draining, the
// moment its stop is requested.
func (s *stuck) cancelAndWait(t *testing.T) {
	t.Helper()
	cancelChild(t, s.r, s.child)
	s.r.run()
	s.r.eventually("attempt draining", func() bool {
		return s.r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND stop_requested_at IS NOT NULL`, s.attempt) == 1
	})
}

// waited moves the stop request into the past, as if the stop had waited this long.
func (s *stuck) waited(seconds int) {
	s.r.exec(`UPDATE task_attempts SET stop_requested_at = now() - make_interval(secs => $2::float8) WHERE id = $1`, s.attempt, float64(seconds))
}

// looked waits until the resolver has looked at the stop this many times, so a test can say nothing
// was forced although it had every chance.
func (s *stuck) looked(n int) {
	s.r.eventually(fmt.Sprintf("%d looks at the stop", n), func() bool {
		return s.r.count(`SELECT resolve_attempts FROM task_attempts WHERE id = $1`, s.attempt) >= n
	})
}

func (s *stuck) held(t *testing.T, what string) {
	t.Helper()
	if slot, claim := s.r.held(s.attempt); slot != 1 || claim != 1 {
		t.Fatalf("held slot=%d claim=%d %s, want both", slot, claim, what)
	}
}

func (s *stuck) released(t *testing.T) {
	t.Helper()
	if slot, claim := s.r.held(s.attempt); slot != 0 || claim != 0 {
		t.Fatalf("held slot=%d claim=%d after the stop settled", slot, claim)
	}
	if n := s.r.count(`SELECT count(*) FROM attention_items WHERE source_kind = 'task_reconciliation' AND state = 'open'`); n != 0 {
		t.Fatalf("%d owner cards open for a stop the system settled itself", n)
	}
}

func (s *stuck) nativeDeleted() (attempt, binding bool) {
	attempt = s.r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND native_deleted_at IS NOT NULL`, s.attempt) == 1
	binding = s.r.count(`SELECT count(*) FROM agent_bindings WHERE session_id = $1 AND kagent_deleted_at IS NOT NULL`, s.session) == 1
	return attempt, binding
}

// A runtime with no open turn is stopped by suspending it, which keeps its workspace. Within the
// default grace nothing is forced however often the resolver looks; after it, the runtime is
// suspended and the suspended session counts as stopped. This is the narrow case: the turn is no
// longer working (here it waits for input), so kagent lets the suspend through.
func TestStopOfARuntimeWithNoOpenTurnIsSuspendedAfterTheDefaultGrace(t *testing.T) {
	t.Parallel()
	s := stuckStop(t)
	s.k.land(a2a.TaskStateInputRequired)
	s.cancelAndWait(t)

	s.looked(4)
	s.waited(110) // just inside the two-minute default
	s.looked(8)
	if n, d := s.suspend.suspends(), s.native.deleteCount(); n != 0 || d != 0 {
		t.Fatalf("suspended %d and deleted %d times inside the grace", n, d)
	}
	s.held(t, "inside the grace")

	s.waited(130)
	s.r.eventually("stop confirmed", func() bool { return s.r.attemptState(s.child) == "cancelled" })
	s.released(t)
	if n := s.suspend.suspends(); n != 1 {
		t.Fatalf("suspended %d times, want exactly once", n)
	}
	if d := s.native.deleteCount(); d != 0 {
		t.Fatalf("deleted the session %d times although suspending settled the stop", d)
	}
	if a, b := s.nativeDeleted(); a || b {
		t.Fatalf("a suspend was recorded as a delete: attempt=%v binding=%v", a, b)
	}
}

// Regression for five cancelled prod tasks that stayed "draining" for days: every CancelTask timed
// out, so the stop was never confirmed and the owner was asked to reconcile it by hand. A turn that
// is still working cannot be suspended — kagent refuses — so once the longer delete grace has passed
// the native session is deleted, with its workspace and any unpushed work. Delete is not tried
// earlier, and the refused suspend does not release anything.
func TestStopDeletesTheNativeSessionOfAWorkingTurnThatSuspendCannotEnd(t *testing.T) {
	t.Parallel()
	s := stuckStop(t) // the turn is working, so suspend is refused
	s.native.afterDelete = kagent.SessionLiveness{Exists: true, Terminal: true, Deleting: true}
	s.cancelAndWait(t)

	s.waited(200) // past suspend, short of delete
	s.r.eventually("suspend tried", func() bool { return s.suspend.suspends() >= 1 })
	s.looked(8)
	if d := s.native.deleteCount(); d != 0 {
		t.Fatalf("deleted %d times before the delete grace", d)
	}

	s.waited(400)
	s.r.eventually("native session deleted", func() bool { return s.native.deleteCount() == 1 })
	// While kagent is still deleting it, nothing is released and nothing is deleted twice.
	s.held(t, "while the session is still being deleted")
	// The fake counts the delete when it is called; the markers follow once it returns.
	s.r.eventually("delete recorded on the attempt and the binding", func() bool {
		a, b := s.nativeDeleted()
		return a && b
	})
	s.native.set(kagent.SessionLiveness{}) // kagent has no record of it any more
	s.r.eventually("stop confirmed", func() bool { return s.r.attemptState(s.child) == "cancelled" })
	s.released(t)
	if got := s.native.deleteCount(); got != 1 {
		t.Fatalf("native session deleted %d times, want exactly once", got)
	}
}

// A cancel that lands just before the grace ends settles the stop; the runtime is neither suspended
// nor deleted.
func TestStopSettledByCancelJustBeforeTheGraceIsNotForced(t *testing.T) {
	t.Parallel()
	s := stuckStop(t)
	s.cancelAndWait(t)

	s.waited(115)
	s.looked(5)
	s.client.setCancelError(false)
	s.r.eventually("stop confirmed", func() bool { return s.r.attemptState(s.child) == "cancelled" })
	s.released(t)
	if n, d := s.suspend.suspends(), s.native.deleteCount(); n != 0 || d != 0 {
		t.Fatalf("suspended %d and deleted %d times for a stop the cancel settled", n, d)
	}
}

// A runtime that is already suspended is stopped: nothing is cancelled, suspended or deleted.
func TestStopOfAnAlreadySuspendedRuntimeForcesNothing(t *testing.T) {
	t.Parallel()
	s := stuckStop(t)
	s.native.set(kagent.SessionLiveness{Exists: true, Suspended: true})
	s.cancelAndWait(t)
	s.r.eventually("stop confirmed", func() bool { return s.r.attemptState(s.child) == "cancelled" })
	s.released(t)
	if n, d := s.suspend.suspends(), s.native.deleteCount(); n != 0 || d != 0 {
		t.Fatalf("suspended %d and deleted %d times a runtime that was already suspended", n, d)
	}
}

// A failed delete is not a stop: the claim and capacity stay held, no delete is recorded, and a
// later look deletes again.
func TestStopKeepsTheClaimWhenTheNativeDeleteFails(t *testing.T) {
	t.Parallel()
	s := stuckStop(t)
	s.native.setDeleteErr(errors.New("injected delete failure"))
	s.cancelAndWait(t)
	s.waited(600)
	s.r.eventually("delete tried again", func() bool { return s.native.deleteCount() >= 2 })
	s.held(t, "after failed deletes")
	if a, b := s.nativeDeleted(); a || b {
		t.Fatalf("a failed delete was recorded: attempt=%v binding=%v", a, b)
	}
	s.native.mu.Lock()
	s.native.deleteErr = nil
	s.native.afterDelete = kagent.SessionLiveness{}
	s.native.mu.Unlock()
	s.r.eventually("stop confirmed", func() bool { return s.r.attemptState(s.child) == "cancelled" })
}

// kagent refuses a delete with Aborted while another lifecycle operation holds the session. That
// is not a stop and records nothing; once the resolver has looked the bound number of times the
// stop is listed as unresolved, which is what the orchestration health page shows, and a later
// delete still settles it.
func TestStopWhoseDeleteIsRefusedIsListedUnresolved(t *testing.T) {
	t.Parallel()
	s := stuckStop(t)
	s.r.svc.Limits.ResolveBound = 3
	s.native.setDeleteErr(&kagent.SessionError{Op: "SessionService DeleteSession", Code: connect.CodeAborted, Wire: true})
	s.cancelAndWait(t)
	s.waited(600)

	s.r.eventually("stop unresolved", func() bool {
		return s.r.str(`SELECT stop_state FROM task_attempts WHERE id = $1`, s.attempt) == "unresolved"
	})
	s.held(t, "while the delete is refused")
	if a, b := s.nativeDeleted(); a || b {
		t.Fatalf("a refused delete was recorded: attempt=%v binding=%v", a, b)
	}
	// The stop is visible on the health surface whatever state its task is in.
	if n := s.r.count(`SELECT count(*) FROM attention_items WHERE source_kind = 'task_reconciliation' AND state = 'open'`); n != 0 {
		t.Fatalf("%d owner cards open for a stop the system is resolving", n)
	}
	rows, err := dbq.New(s.r.pool).ListUnresolvedStops(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].AttemptID != s.attempt {
		t.Fatalf("unresolved stops = %+v, want attempt %s", rows, s.attempt)
	}

	s.native.mu.Lock()
	s.native.deleteErr = nil
	s.native.afterDelete = kagent.SessionLiveness{}
	s.native.mu.Unlock()
	s.r.eventually("stop confirmed", func() bool { return s.r.attemptState(s.child) == "cancelled" })
	s.released(t)
	if rows, err := dbq.New(s.r.pool).ListUnresolvedStops(context.Background(), owner); err != nil || len(rows) != 0 {
		t.Fatalf("a settled stop is still listed unresolved: %v %v", rows, err)
	}
}

// A session kagent already lost is stopped without suspending or deleting anything.
func TestStopOfAnAlreadyGoneRuntimeForcesNothing(t *testing.T) {
	t.Parallel()
	s := stuckStop(t)
	s.native.set(kagent.SessionLiveness{})
	s.cancelAndWait(t)
	s.r.eventually("stop confirmed", func() bool { return s.r.attemptState(s.child) == "cancelled" })
	if n, d := s.suspend.suspends(), s.native.deleteCount(); n != 0 || d != 0 {
		t.Fatalf("suspended %d and deleted %d times a session kagent had already lost", n, d)
	}
	s.released(t)
}
