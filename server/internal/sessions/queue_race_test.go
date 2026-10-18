package sessions_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

// waitForBlockedQuery polls pg_stat_activity until a query matching like is waiting on a lock, or
// fails the test. It is how these tests pin a transaction at a precise point without a sleep: a
// trigger makes the statement wait on an advisory lock the test holds, and this confirms the
// statement actually got there before the test acts on that assumption.
func waitForBlockedQuery(t *testing.T, pool *pgxpool.Pool, like string) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE $1`, like).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no query matching %q ever blocked on a lock", like)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForBlockedOrDone is waitForBlockedQuery for an operation that is expected to block but may,
// if the code under test is wrong, finish instead. It reports whether the operation finished first, and its result if so.
func waitForBlockedOrDone(t *testing.T, pool *pgxpool.Pool, like string, done <-chan error) (finished bool, err error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case doneErr := <-done:
			return true, doneErr
		default:
		}
		var n int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE $1`, like).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return false, nil
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation neither blocked on a query matching %q nor finished", like)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Reproduces the race #224's reviewer found: a send decides the session is busy and queues the
// message, under mainloop:send:<session>; if the open turn it queued behind ends in between that
// decision and the queued row's commit, and the ending does not take the same lock, the ending's
// promotion looks for a queued row before this one is visible, finds none, and the message is
// stranded in 'queued' with nothing left to promote it (deliver.go's claimable states, and the
// reconcile table, both exclude 'queued'). Fails on main: the stop races ahead of the paused send
// and the new message never leaves 'queued'.
func TestASendRacingTurnCompletionIsPromotedNotStuckQueued(t *testing.T) {
	e := newEnv(t)
	e.withProviders()
	ctx := context.Background()
	id := e.newSession("first")
	e.run()
	e.waitState(e.firstDelivery(id), dbstate.DeliveryCompleted)
	mid := e.openTurn(id, "long") // an open turn that hangs until it is stopped

	// A trigger makes the queued insert wait for an advisory lock this test holds, pausing a send's
	// transaction exactly after it has decided the session is busy but before the queued row
	// commits.
	if _, err := e.pool.Exec(ctx, `CREATE FUNCTION hold_queued_insert() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN PERFORM pg_advisory_lock(900001); PERFORM pg_advisory_unlock(900001); RETURN NEW; END $$;
		CREATE TRIGGER hold_queued_insert BEFORE INSERT ON deliveries
		FOR EACH ROW WHEN (NEW.state = 'queued') EXECUTE FUNCTION hold_queued_insert()`); err != nil {
		t.Fatal(err)
	}
	holder, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock(900001)`); err != nil {
		t.Fatal(err)
	}

	newID := uuid.NewString()
	sendErr := make(chan error, 1)
	go func() {
		_, err := e.svc.SendTo(ctx, id, newID, "two")
		sendErr <- err
	}()
	waitForBlockedQuery(t, e.pool, `%INSERT INTO deliveries (id, idempotency_key, session_id, source, state)%`)

	// End the open turn while the send is paused mid-decision. On unfixed code nothing stops this
	// from completing (and looking for a queued row to promote) before the paused send ever commits
	// its own. The fixed code cannot finish early, since it is blocked on the same lock the send
	// holds; so wait until the stop is either blocked on that lock (the fixed code) or has already
	// finished (the race the test exists to catch), rather than guessing how long that takes.
	stopDone := make(chan error, 1)
	go func() {
		_, err := e.svc.Stop(ctx, id)
		stopDone <- err
	}()
	stopFinished, stopErr := waitForBlockedOrDone(t, e.pool, `SELECT pg_advisory_xact_lock(%`, stopDone)

	if _, err := holder.Exec(ctx, `SELECT pg_advisory_unlock(900001)`); err != nil {
		t.Fatal(err)
	}
	if err := <-sendErr; err != nil {
		t.Fatalf("send: %v", err)
	}
	if !stopFinished {
		stopErr = <-stopDone
	}
	if stopErr != nil {
		t.Fatalf("stop: %v", stopErr)
	}
	e.waitState(mid, dbstate.DeliveryCancelled)
	// The queued message must be promoted once the turn it waited behind ended, not stranded.
	e.waitState(newID, dbstate.DeliveryCompleted)
}

// The admission query used to ignore an existing backlog once the turn that first blocked it went
// past its uncertainty hold, so a later send jumped straight to 'recorded' ahead of messages still
// waiting their turn. Fails on main: the new send is recorded instead of queued, and the backlog
// stays behind it forever.
func TestSendQueuesBehindAnExistingBacklogOnceTheBlockingHoldHasExpired(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	oldID := e.send("one") // the open turn everything else queues behind

	q1ID := uuid.NewString()
	if sent, err := e.svc.Send(ctx, q1ID, "", "two"); err != nil || !sent.Queued {
		t.Fatalf("first queued send: %+v, %v", sent, err)
	}

	// The open turn goes uncertain and its hold expires, but nothing has rechecked it yet (the
	// delivery loop is not running in this test): it no longer blocks a send, but the backlog
	// behind it was never released.
	if _, err := e.pool.Exec(ctx,
		`UPDATE deliveries SET state = 'uncertain', updated_at = now() - interval '1 hour' WHERE id = $1`, oldID,
	); err != nil {
		t.Fatal(err)
	}

	newID := uuid.NewString()
	sent, err := e.svc.Send(ctx, newID, "", "three")
	if err != nil {
		t.Fatal(err)
	}
	if !sent.Queued {
		t.Fatalf("a send behind an unreleased backlog was recorded instead of queued: %+v", sent)
	}
	if got := e.delivery(newID).State; got != dbstate.DeliveryQueued {
		t.Fatalf("new send state = %s", got)
	}
	if got := e.delivery(q1ID).State; got != dbstate.DeliveryRecorded {
		t.Fatalf("the oldest queued message was not released once nothing blocked it any more: %s", got)
	}
}

// PromoteNextQueuedDelivery's NOT EXISTS used to check only recorded/sending/delivered, so settling
// an unrelated older delivery could release a message behind a newer delivery that is uncertain and
// still inside its own hold -- a turn that may still be running. Fails on main: the backlog is
// released while the newer turn is still held.
func TestSettlingAnOlderDeliveryHonorsANewerUncertainHold(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	warm := e.send("warm up")
	stop := e.run()
	e.waitState(warm, dbstate.DeliveryCompleted)
	stop()
	ksid := e.kagentSessionID()

	// The older delivery: sent, then uncertain, well past its hold -- it no longer blocks a send,
	// but it has not settled yet.
	oldID := e.send("one")
	if _, err := e.pool.Exec(ctx,
		`UPDATE deliveries SET state = 'uncertain', updated_at = now() - interval '1 hour', kagent_session_id = $2 WHERE id = $1`,
		oldID, ksid,
	); err != nil {
		t.Fatal(err)
	}

	// The newer delivery: admitted because the older one no longer blocks, then uncertain itself,
	// freshly -- a turn that may still be running.
	newID := uuid.NewString()
	if sent, err := e.svc.Send(ctx, newID, "", "two"); err != nil || sent.Queued {
		t.Fatalf("newer send: %+v, %v", sent, err)
	}
	if _, err := e.pool.Exec(ctx,
		`UPDATE deliveries SET state = 'uncertain', updated_at = now(), kagent_session_id = $2 WHERE id = $1`,
		newID, ksid,
	); err != nil {
		t.Fatal(err)
	}

	// The backlog behind the newer turn.
	qID := uuid.NewString()
	if sent, err := e.svc.Send(ctx, qID, "", "three"); err != nil || !sent.Queued {
		t.Fatalf("queued send: %+v, %v", sent, err)
	}

	// The older delivery finally settles: a late lookup finds it after all.
	e.srv.AddTask(completedTask("t-old", ksid, oldID, "late reply"))
	e.run()
	e.waitState(oldID, dbstate.DeliveryCompleted)

	if got := e.delivery(qID).State; got != dbstate.DeliveryQueued {
		t.Fatalf("settling an older delivery released the backlog behind a newer uncertain turn: %s", got)
	}

	// Once the newer turn is actually resolved too, the backlog is released normally.
	e.srv.AddTask(completedTask("t-new", ksid, newID, "second reply"))
	e.waitState(newID, dbstate.DeliveryCompleted)
	e.waitState(qID, dbstate.DeliveryCompleted)
}

// The recovery sweep is the safety net for a queued row left behind with nothing to release it
// (the normal path, exercised above, is a send or a settle releasing its own session's backlog as
// it goes). This writes that stranded state directly, as a crash between the admission decision
// and the promotion would leave it, rather than racing to reach it.
func TestPromoteStrandedQueuedSweepReleasesABacklogNothingIsBlocking(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	oldID := e.send("one")
	qID := uuid.NewString()
	if sent, err := e.svc.Send(ctx, qID, "", "two"); err != nil || !sent.Queued {
		t.Fatalf("queued send: %+v, %v", sent, err)
	}
	// The turn it queued behind ended (or went stale) without ever releasing it.
	if _, err := e.pool.Exec(ctx,
		`UPDATE deliveries SET state = 'failed', updated_at = now() - interval '1 hour' WHERE id = $1`, oldID,
	); err != nil {
		t.Fatal(err)
	}

	if err := e.svc.PromoteStrandedQueued(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.delivery(qID).State; got != dbstate.DeliveryRecorded {
		t.Fatalf("stranded queued delivery state = %s, want recorded", got)
	}
}
