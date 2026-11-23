package sessions_test

import (
	"strings"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	"github.com/oldsj/mainloop/server/internal/sessions"
)

// A send kagent does not accept (Substrate has no free worker for the actor) is put back and tried
// again while the session's task waits for runtime capacity, and delivered once kagent takes it.
// Without the wait, the same refusal fails the delivery as before.
func TestNotAcceptedSendWaitsWhileTheTaskWaitsForCapacity(t *testing.T) {
	e := newEnv(t, func(so *sessions.Options, kc *kagent.Config) {
		kc.SendRetryBudget = 200 * time.Millisecond
		so.CapacityRecheck = 20 * time.Millisecond
	})
	e.withProviders()
	e.run()
	sid := e.taskSession()
	if _, err := e.pool.Exec(t.Context(),
		`UPDATE tasks SET capacity_tier = 'overflow', runtime_wait_since = now(), current_attempt_id = 'a1' WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	before := e.sends()
	const refusals = 8
	for range refusals {
		e.srv.FailNext("SendStreamingMessage", kagenttest.Fault{RPC: kagenttest.NotAccepted(100)})
	}
	id := e.sendTo(sid, "next step")
	waited := false
	deadline := time.Now().Add(15 * time.Second)
	for {
		d := e.delivery(id)
		if strings.HasPrefix(deref(d.Detail), "waiting for runtime capacity:") {
			waited = true
		}
		if d.State == dbstate.DeliveryFailed {
			t.Fatalf("delivery failed while the task waited: %q", deref(d.Detail))
		}
		if d.State == dbstate.DeliveryCompleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("delivery is %s (detail %q)", d.State, deref(d.Detail))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !waited || e.sends()-before != refusals+1 {
		t.Fatalf("waited %v, %d sends", waited, e.sends()-before)
	}

	// The wait over, a refusal fails the delivery: nothing changed for other sends.
	if _, err := e.pool.Exec(t.Context(), `UPDATE tasks SET runtime_wait_since = NULL WHERE id = 't1'`); err != nil {
		t.Fatal(err)
	}
	for range refusals {
		e.srv.FailNext("SendStreamingMessage", kagenttest.Fault{RPC: kagenttest.NotAccepted(100)})
	}
	id = e.sendTo(sid, "another")
	if d := e.waitState(id, dbstate.DeliveryFailed); !strings.HasPrefix(deref(d.Detail), "not sent:") {
		t.Fatalf("detail %q", deref(d.Detail))
	}
}

// fastCapacity is an env whose kagent client gives up on a refused send at once and whose capacity
// waits recheck quickly.
func fastCapacity(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, func(so *sessions.Options, kc *kagent.Config) {
		kc.SendRetryBudget = 200 * time.Millisecond
		so.CapacityRecheck = 20 * time.Millisecond
	})
	e.withProviders()
	return e
}

// waitThroughRefusals waits for a delivery kagent refuses a few times before taking it, and fails if
// it failed meanwhile or was never put back to wait.
func (e *env) waitThroughRefusals(id string) {
	e.t.Helper()
	waited := false
	deadline := time.Now().Add(15 * time.Second)
	for {
		d := e.delivery(id)
		if strings.HasPrefix(deref(d.Detail), "waiting for runtime capacity:") {
			waited = true
		}
		switch {
		case d.State == dbstate.DeliveryFailed:
			e.t.Fatalf("delivery failed while waiting for capacity: %q", deref(d.Detail))
		case d.State == dbstate.DeliveryCompleted:
			if !waited {
				e.t.Fatal("the delivery never waited")
			}
			return
		case time.Now().After(deadline):
			e.t.Fatalf("delivery is %s (detail %q)", d.State, deref(d.Detail))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A new session's first send wakes its actor; on a full worker kagent refuses it until an idle actor
// is suspended. The send waits for room instead of failing.
func TestANewSessionWaitsForItsFirstRuntime(t *testing.T) {
	e := fastCapacity(t)
	e.run()
	for range 8 {
		e.srv.FailNext("SendStreamingMessage", kagenttest.Fault{RPC: kagenttest.NotAccepted(100)})
	}
	sid := e.newSession("the brief")
	e.waitThroughRefusals(e.firstDelivery(sid))
}

// A session that started longer ago than the wait, with nothing accepted, no longer waits.
func TestANewSessionStopsWaitingAfterTheWindow(t *testing.T) {
	e := fastCapacity(t)
	sid := e.newSession("the brief")
	if _, err := e.pool.Exec(t.Context(), `UPDATE sessions SET created_at = now() - interval '1 hour' WHERE id = $1`, sid); err != nil {
		t.Fatal(err)
	}
	for range 8 {
		e.srv.FailNext("SendStreamingMessage", kagenttest.Fault{RPC: kagenttest.NotAccepted(100)})
	}
	e.run()
	if d := e.waitState(e.firstDelivery(sid), dbstate.DeliveryFailed); !strings.HasPrefix(deref(d.Detail), "not sent:") {
		t.Fatalf("detail %q", deref(d.Detail))
	}
}

// A send to a running session whose actor was suspended and finds no worker with room, which kagent
// says (cause runtime_capacity), waits too; a plain refusal there still fails (above).
func TestANoCapacityRefusalWaits(t *testing.T) {
	e := fastCapacity(t)
	e.run()
	sid := e.taskSession()
	for range 8 {
		e.srv.FailNext("SendStreamingMessage", kagenttest.Fault{RPC: kagenttest.NoCapacity(100)})
	}
	e.waitThroughRefusals(e.sendTo(sid, "next step"))
}
