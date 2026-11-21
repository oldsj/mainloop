package sessions_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	"github.com/oldsj/mainloop/server/internal/sessions"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1"
)

// A kagent Session whose runtime crashed is FAILED with a revert operation until a RevertSession
// call succeeds; delivery makes that call, a bounded number of times, before it gives up.
func TestDeliveryRevertsACrashedKagentSession(t *testing.T) {
	t.Run("Given a crashed main thread Session, when a message is sent, assert that the revert heals it and the message is delivered on the same Session", func(t *testing.T) {
		f := newRevertFixture(t, kagenttest.RevertHeals)
		f.GivenTheMainThreadSessionCrashed()
		id := f.e.send("are you there?")
		f.e.waitState(id, dbstate.DeliveryCompleted)
		f.AssertThatRevertsWere(1)
		f.AssertThatTheSessionWasKept()
	})

	t.Run("Given a crashed Session whose revert keeps failing transiently, when it is made ready, assert that it stops after the bound with the Session's failure", func(t *testing.T) {
		f := newRevertFixture(t, kagenttest.RevertUnavailable)
		f.GivenTheMainThreadSessionCrashed()
		err := f.e.svc.ReadySession(context.Background(), "s1", nil, f.ksid)
		if err == nil || err.Error() != "the kagent session has failed" {
			t.Fatalf("err = %v, want the Session's failure unchanged", err)
		}
		f.AssertThatRevertsWere(3)
	})

	t.Run("Given a crashed Session kagent says cannot be reverted, when a message is sent, assert that delivery fails at once as unrecoverable", func(t *testing.T) {
		f := newRevertFixture(t, kagenttest.RevertUnrecoverable)
		f.GivenTheMainThreadSessionCrashed()
		d := f.e.waitState(f.e.send("are you there?"), dbstate.DeliveryFailed)
		if detail := deref(d.Detail); !strings.Contains(detail, "the kagent session has failed") ||
			!strings.Contains(detail, "cannot revert") {
			t.Fatalf("detail = %q, want the failure and why it is unrecoverable", detail)
		}
		f.AssertThatRevertsWere(1)
	})

	t.Run("Given a FAILED Session with no revert operation, when it is made ready, assert that it fails without a revert", func(t *testing.T) {
		f := newRevertFixture(t, kagenttest.RevertHeals)
		f.e.srv.SetRuntime(f.ksid, kagentapi.RuntimeState_RUNTIME_STATE_FAILED, kagentapi.RuntimeOperation_RUNTIME_OPERATION_NONE)
		err := f.e.svc.ReadySession(context.Background(), "s1", nil, f.ksid)
		if err == nil || err.Error() != "the kagent session has failed" {
			t.Fatalf("err = %v, want the Session's failure", err)
		}
		f.AssertThatRevertsWere(0)
	})

	t.Run("Given a crashed Session whose revert is aborted by another operation, when it is made ready, assert that it is retried until the bound", func(t *testing.T) {
		f := newRevertFixture(t, kagenttest.RevertAborted)
		f.GivenTheMainThreadSessionCrashed()
		if err := f.e.svc.ReadySession(context.Background(), "s1", nil, f.ksid); err == nil {
			t.Fatal("a Session that stayed crashed was ready")
		}
		f.AssertThatRevertsWere(3)
	})
}

// revertFixture is a main thread with one delivered message, so it has a kagent Session.
type revertFixture struct {
	t    *testing.T
	e    *env
	ksid string
}

func newRevertFixture(t *testing.T, revert kagenttest.RevertOutcome) *revertFixture {
	t.Helper()
	e := newEnv(t, func(o *sessions.Options, _ *kagent.Config) { o.ReadyWait = 2 * time.Second })
	e.srv.Revert = revert
	e.run()
	e.waitState(e.send("one"), dbstate.DeliveryCompleted)
	return &revertFixture{t: t, e: e, ksid: e.kagentSessionID()}
}

func (f *revertFixture) GivenTheMainThreadSessionCrashed() { f.e.srv.Crash(f.ksid) }

func (f *revertFixture) AssertThatRevertsWere(want int) {
	f.t.Helper()
	if got := f.e.srv.Count(kagent.ProcRevertSession); got != want {
		f.t.Fatalf("%d RevertSession calls, want %d", got, want)
	}
}

func (f *revertFixture) AssertThatTheSessionWasKept() {
	f.t.Helper()
	if got := f.e.kagentSessionID(); got != f.ksid || f.e.srv.Count(kagent.ProcCreateSession) != 1 {
		f.t.Fatalf("kagent session %s (%d creates), want %s kept", got, f.e.srv.Count(kagent.ProcCreateSession), f.ksid)
	}
}
