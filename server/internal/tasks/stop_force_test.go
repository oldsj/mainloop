// A runtime that never answers a cancel (crashed, suspended, or wedged inside a turn) must not hold its
// attempt's claim and capacity, or the owner's attention, for good. After a few looks the resolver has
// the observer delete the native session; once kagent reports it gone, the stop is confirmed.

package tasks_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

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

func (f *fakeNativeSession) deleteCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deletes
}

// stuckStop provisions a cancelled child whose native runtime never answers: every cancel is an
// uncertain timeout, as in prod, and the session reads present and not terminal until deleted.
func stuckStop(t *testing.T, forceAfter int) (r *rig, native *fakeNativeSession, child, attempt string) {
	t.Helper()
	r, _, _, child, attempt, session := stopProbeProvision(t)
	k := &fakeKagent{}
	k.land(a2a.TaskStateWorking)
	c := &stopProbeClient{k: k}
	c.setCancelError(true)
	native = &fakeNativeSession{live: kagent.SessionLiveness{Exists: true}}
	r.ses.Kagent = c
	r.svc.Sessions = r.ses.Service
	r.svc.StopObserver = &tasks.KagentStops{
		Pool: r.pool, OwnerID: owner, Sessions: r.ses.Service, Kagent: c, Liveness: native, Deleter: native,
	}
	r.svc.Limits.ForceStopAfter = forceAfter
	r.exec(`UPDATE agent_bindings SET kagent_session_id = 'ks-native' WHERE session_id = $1`, session)
	r.briefRow(attempt, `state='delivered', task_id='task-1', kagent_session_id='ks-native'`)
	return r, native, child, attempt
}

func cancelChild(t *testing.T, r *rig, child string) {
	t.Helper()
	sup := r.own
	if _, err := r.svc.Cancel(context.Background(), sup, action(child, r.view(sup, child), uuid.NewString())); err != nil {
		t.Fatal(err)
	}
}

// Regression for five cancelled prod tasks that stayed "draining" for days: every CancelTask timed
// out, so the stop was never confirmed and the owner was asked to reconcile it by hand.
func TestStopOfAnUnresponsiveRuntimeDeletesTheNativeSessionAndSettles(t *testing.T) {
	r, native, child, attempt := stuckStop(t, 1)
	native.afterDelete = kagent.SessionLiveness{Exists: true, Terminal: true, Deleting: true}
	cancelChild(t, r, child)
	r.run()

	r.eventually("native session deleted", func() bool { return native.deleteCount() == 1 })
	// While kagent is still deleting it, nothing is released and nothing is deleted twice.
	if slot, claim := r.held(attempt); slot != 1 || claim != 1 {
		t.Fatalf("held slot=%d claim=%d while the session is still being deleted, want both", slot, claim)
	}
	native.set(kagent.SessionLiveness{}) // kagent has no record of it any more
	r.eventually("stop confirmed", func() bool { return r.attemptState(child) == "cancelled" })
	if slot, claim := r.held(attempt); slot != 0 || claim != 0 {
		t.Fatalf("held slot=%d claim=%d after the stop settled", slot, claim)
	}
	if got := native.deleteCount(); got != 1 {
		t.Fatalf("native session deleted %d times, want exactly once", got)
	}
	if n := r.count(`SELECT count(*) FROM attention_items WHERE source_kind = 'task_reconciliation' AND state = 'open'`); n != 0 {
		t.Fatalf("%d owner cards open for a stop the system settled itself", n)
	}
}

// Nothing is deleted while a cancel could still work: before the configured number of looks the
// observer only cancels.
func TestStopDoesNotDeleteTheNativeSessionBeforeItsLooks(t *testing.T) {
	r, native, child, attempt := stuckStop(t, 1000)
	cancelChild(t, r, child)
	r.run()
	r.eventually("stop looked at", func() bool {
		return r.count(`SELECT resolve_attempts FROM task_attempts WHERE id=$1`, attempt) >= 3
	})
	if got := native.deleteCount(); got != 0 {
		t.Fatalf("native session deleted %d times before the force threshold", got)
	}
}

// A failed delete is not a stop: the claim and capacity stay held, and a later look deletes again.
func TestStopKeepsTheClaimWhenTheNativeDeleteFails(t *testing.T) {
	r, native, child, attempt := stuckStop(t, 1)
	native.deleteErr = errors.New("injected delete failure")
	cancelChild(t, r, child)
	r.run()
	r.eventually("delete tried again", func() bool { return native.deleteCount() >= 2 })
	if slot, claim := r.held(attempt); slot != 1 || claim != 1 {
		t.Fatalf("held slot=%d claim=%d after failed deletes, want both", slot, claim)
	}
	native.mu.Lock()
	native.deleteErr = nil
	native.afterDelete = kagent.SessionLiveness{}
	native.mu.Unlock()
	r.eventually("stop confirmed", func() bool { return r.attemptState(child) == "cancelled" })
}

// A session kagent already lost is stopped without deleting anything.
func TestStopOfAnAlreadyGoneRuntimeDeletesNothing(t *testing.T) {
	r, native, child, attempt := stuckStop(t, 1)
	native.set(kagent.SessionLiveness{})
	cancelChild(t, r, child)
	r.run()
	r.eventually("stop confirmed", func() bool { return r.attemptState(child) == "cancelled" })
	if got := native.deleteCount(); got != 0 {
		t.Fatalf("deleted a session kagent had already lost: %d", got)
	}
	if slot, claim := r.held(attempt); slot != 0 || claim != 0 {
		t.Fatalf("held slot=%d claim=%d", slot, claim)
	}
}
