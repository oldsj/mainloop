// A runtime that never answers a cancel (crashed or wedged inside a turn) must not hold its claim
// and capacity for ever. kagent parks an idle runtime itself, and a sleeping one counts as stopped.
// One that stays awake past the delete grace has its native session deleted by force.
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

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/tasks"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1"
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

func (f *fakeNativeSession) ForceDeleteSession(context.Context, string) (*kagentapi.Session, error) {
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

type stuck struct {
	r       *rig
	native  *fakeNativeSession
	client  *stopProbeClient
	k       *fakeKagent
	child   string
	attempt string
	session string
}

// stuckStop provisions a cancelled child whose native runtime never answers: every cancel is an
// uncertain timeout, as in prod, and the session reads present, awake and not terminal until kagent
// parks it or it is deleted.
func stuckStop(t *testing.T) *stuck {
	t.Helper()
	r, _, _, child, attempt, session := stopProbeProvision(t)
	k := &fakeKagent{}
	k.land(a2a.TaskStateWorking)
	c := &stopProbeClient{k: k}
	c.setCancelError(true)
	native := &fakeNativeSession{live: kagent.SessionLiveness{Exists: true}}
	r.ses.Kagent = c
	r.svc.Sessions = r.ses.Service
	r.svc.StopObserver = &tasks.KagentStops{
		Pool: r.pool, OwnerID: owner, Sessions: r.ses.Service, Kagent: c, Liveness: native, Deleter: native,
	}
	r.exec(`UPDATE agent_bindings SET kagent_session_id = 'ks-native' WHERE session_id = $1`, session)
	r.briefRow(attempt, `state='delivered', task_id='task-1', kagent_session_id='ks-native'`)
	return &stuck{r: r, native: native, client: c, k: k, child: child, attempt: attempt, session: session}
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

// Nothing suspends a runtime any more: kagent parks an idle Actor itself. A stop whose turn is no
// longer working (here it waits for input) is not forced at the old suspend grace, and it settles
// without a delete once kagent has put the runtime to sleep.
func TestStopOfARuntimeWithNoOpenTurnSettlesWhenKagentParksIt(t *testing.T) {
	s := stuckStop(t)
	s.k.land(a2a.TaskStateInputRequired)
	s.cancelAndWait(t)

	s.waited(130) // past the old suspend grace, short of the delete grace
	s.looked(8)
	if d := s.native.deleteCount(); d != 0 {
		t.Fatalf("deleted %d times before the delete grace", d)
	}
	s.held(t, "while the runtime is awake")

	s.native.set(kagent.SessionLiveness{Exists: true, Asleep: true})
	s.r.eventually("stop confirmed", func() bool { return s.r.attemptState(s.child) == "cancelled" })
	s.released(t)
	if d := s.native.deleteCount(); d != 0 {
		t.Fatalf("deleted the session %d times although it was asleep", d)
	}
	if a, b := s.nativeDeleted(); a || b {
		t.Fatalf("a sleeping runtime was recorded as deleted: attempt=%v binding=%v", a, b)
	}
}

// Regression for five cancelled prod tasks that stayed "draining" for days: every CancelTask timed
// out, so the stop was never confirmed and the owner was asked to reconcile it by hand. A turn that
// is still working keeps its Actor awake, so once the delete grace has passed the native session is
// deleted by force (kagent refuses an ordinary delete while a turn runs), with its workspace and any
// unpushed work. Delete is not tried earlier.
func TestStopForceDeletesTheNativeSessionOfAWorkingTurn(t *testing.T) {
	s := stuckStop(t)
	s.native.afterDelete = kagent.SessionLiveness{Exists: true, Terminal: true, Deleting: true}
	s.cancelAndWait(t)

	s.waited(200) // short of the delete grace
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

// A cancel that lands just before the grace ends settles the stop; the runtime is not deleted.
func TestStopSettledByCancelJustBeforeTheGraceIsNotForced(t *testing.T) {
	s := stuckStop(t)
	s.cancelAndWait(t)

	s.waited(115)
	s.looked(5)
	s.client.setCancelError(false)
	s.r.eventually("stop confirmed", func() bool { return s.r.attemptState(s.child) == "cancelled" })
	s.released(t)
	if d := s.native.deleteCount(); d != 0 {
		t.Fatalf("deleted %d times for a stop the cancel settled", d)
	}
}

// A runtime that is already asleep is stopped: nothing is cancelled or deleted.
func TestStopOfAnAlreadySleepingRuntimeForcesNothing(t *testing.T) {
	s := stuckStop(t)
	s.native.set(kagent.SessionLiveness{Exists: true, Asleep: true})
	s.cancelAndWait(t)
	s.r.eventually("stop confirmed", func() bool { return s.r.attemptState(s.child) == "cancelled" })
	s.released(t)
	if d := s.native.deleteCount(); d != 0 {
		t.Fatalf("deleted %d times a runtime that was already asleep", d)
	}
}

// A failed delete is not a stop: the claim and capacity stay held, no delete is recorded, and a
// later look deletes again.
func TestStopKeepsTheClaimWhenTheNativeDeleteFails(t *testing.T) {
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

// A session kagent already lost is stopped without deleting anything.
func TestStopOfAnAlreadyGoneRuntimeForcesNothing(t *testing.T) {
	s := stuckStop(t)
	s.native.set(kagent.SessionLiveness{})
	s.cancelAndWait(t)
	s.r.eventually("stop confirmed", func() bool { return s.r.attemptState(s.child) == "cancelled" })
	if d := s.native.deleteCount(); d != 0 {
		t.Fatalf("deleted %d times a session kagent had already lost", d)
	}
	s.released(t)
}
