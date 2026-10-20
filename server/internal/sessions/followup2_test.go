package sessions_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/sessions"
)

// Cancel tells open clients about every turn it ends, as a settle does.
func TestCancelPublishesAnEndedEventPerClosedDelivery(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	id := e.newSession("first")
	e.run()
	e.waitState(e.firstDelivery(id), dbstate.DeliveryCompleted)
	mid := e.openTurn(id, "long")
	before := len(e.sessionEvents())

	if _, err := e.svc.Cancel(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	evs := e.sessionEvents()
	if len(evs) != before+1 {
		t.Fatalf("%d session:message events after the cancel, %d before", len(evs), before)
	}
	if last := evs[len(evs)-1]; last.MessageId != mid || last.SessionId != id {
		t.Fatalf("event for %+v, want message %s", last, mid)
	}
}

// A cancel that finds the session finished meanwhile rolls back: nothing is published and the
// deliveries are left as they are.
func TestCancelOfAFinishedSessionPublishesNothing(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	id := e.newSession("first")
	e.run()
	e.waitState(e.firstDelivery(id), dbstate.DeliveryCompleted)
	before := len(e.sessionEvents())
	if _, err := e.pool.Exec(context.Background(), `UPDATE sessions SET status = 'completed' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	r, err := e.svc.Cancel(context.Background(), id)
	if err != nil || r.Status != dbstate.SessionCompleted {
		t.Fatalf("%+v %v", r, err)
	}
	if len(e.sessionEvents()) != before {
		t.Fatal("a cancel that changed nothing published an event")
	}
}

// Mined from the reviewer's TestProbeCancelVersusSettleLockOrder: a settle holds the delivery row
// and then locks the session row; Cancel must take them in the same order or Postgres aborts one.
func TestCancelVersusSettleLockOrder(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	id := e.newSession("first")
	e.run()
	e.waitState(e.firstDelivery(id), dbstate.DeliveryCompleted)
	mid := e.openTurn(id, "long")
	ctx := context.Background()

	// The settle: delivery row first.
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM deliveries WHERE id = $1 FOR UPDATE`, mid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := e.svc.Cancel(ctx, id)
		done <- err
	}()
	// Cancel is now waiting for the delivery row. Take the session row, as the settle does next.
	time.Sleep(300 * time.Millisecond)
	lockSession := func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sessions SET title = title WHERE id = $1`, id)
		return err
	}
	if err := lockSession(tx); err != nil {
		t.Fatalf("the settle was aborted: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancel: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel never returned")
	}
	if e.sessionStatus(id) != dbstate.SessionCancelled {
		t.Fatalf("status %s", e.sessionStatus(id))
	}
}

// Mined from TestProbeStopBehindAnOldUncertainDelivery: an uncertain row past its hold no longer
// blocks sends, so a newer turn may be running behind it. Stop must still reach that turn.
func TestStopReachesATurnBehindAnOldUncertainDelivery(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	ctx := context.Background()
	id := e.newSession("first")
	e.run()
	e.waitState(e.firstDelivery(id), dbstate.DeliveryCompleted)
	old := e.leftBehind2(id, dbstate.DeliveryUncertain, "")
	if _, err := e.pool.Exec(ctx, `UPDATE deliveries SET updated_at = now() - interval '2 hours' WHERE id = $1`, old); err != nil {
		t.Fatal(err)
	}
	mid := e.openTurn(id, "long")

	out, err := e.svc.Stop(ctx, id)
	if err != nil || out != sessions.Stopped {
		t.Fatalf("%v %v", out, err)
	}
	e.waitState(mid, dbstate.DeliveryCancelled)
	if e.srv.Count("CancelTask") != 1 {
		t.Fatalf("%d CancelTask calls", e.srv.Count("CancelTask"))
	}
	if e.delivery(old).State != dbstate.DeliveryUncertain {
		t.Fatalf("the old row was %s: stop touched what the loop owns", e.delivery(old).State)
	}
}
