package health_test

import (
	"context"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/health"
)

// Review P1-1: more terminal deliveries than one discovery batch must not hide newer ones. The
// first 500 get receipts and every later pass moves on to the rest.
func TestDiscoveryReachesPastTheBatch(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.exec(t, `UPDATE deliveries SET created_at = now()`)
	e.exec(t, `INSERT INTO deliveries (id, idempotency_key, session_id, state, task_id, kagent_session_id, created_at)
		SELECT 'old-'||n, 'old-key-'||n, 'session-1', 'completed', 'old-task-'||n, $1, now() - interval '2 hours' - n * interval '1 second'
		FROM generate_series(1, 500) AS n`, runtimeSession)

	for range 3 {
		if err := e.svc.Recover(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var fresh, total int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE a2a_task_id IN ($1, $2)), count(*) FROM health_collection`, task1, task2).Scan(&fresh, &total); err != nil {
		t.Fatal(err)
	}
	if fresh != 2 || total != 502 {
		t.Fatalf("receipts for the new deliveries = %d, total = %d, want 2 and 502", fresh, total)
	}
}

// partial is a copy of a fixture task that has produced only its first observations.
func partial(task *a2a.Task, n int) *a2a.Task {
	c := *task
	c.Artifacts = task.Artifacts[:n]
	return &c
}

// Review P2-1: a running turn's observations are collected while it runs, re-read on the backoff
// schedule, and the read after it ends closes the receipt without counting anything twice.
func TestRunningTurnIsCollectedWhileItRuns(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	whole := e.tasks[task2]
	if len(whole.Artifacts) < 3 {
		t.Fatalf("fixture task has %d artifacts", len(whole.Artifacts))
	}
	e.exec(t, `UPDATE deliveries SET state = 'delivered' WHERE id = 'msg-2'`)
	e.kc.tasks[task2] = partial(whole, 1)

	receipt := func() (state string, attempts int, wait time.Duration) {
		t.Helper()
		if err := e.pool.QueryRow(ctx, `SELECT state, attempts, next_attempt_at - now() FROM health_collection WHERE a2a_task_id = $1`, task2).
			Scan(&state, &attempts, &wait); err != nil {
			t.Fatal(err)
		}
		return
	}
	events := func() int {
		var n int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM health_events WHERE a2a_task_id = $1`, task2).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if err := e.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	state, attempts, wait := receipt()
	if state != string(dbstate.HealthPending) || attempts != 0 || wait < 5*time.Second || events() == 0 {
		t.Fatalf("running turn: %s %d %s, %d events", state, attempts, wait, events())
	}
	midEvents := events()
	if view := e.view(t, health.Filter{}); len(view.Rows) == 0 || view.Rows[0].Totals.Turns == 0 {
		t.Fatalf("the page shows nothing of a running turn: %+v", view.Rows)
	} else if view.Rows[0].Totals.Coverage != "partial" {
		// The receipt is still open, so the page stays partial even with observations in hand.
		t.Fatalf("a running turn shows %q coverage, want partial", view.Rows[0].Totals.Coverage)
	}

	// Inside the delay it is not read again.
	calls := e.kc.callCount()
	if err := e.svc.Recover(ctx); err != nil || e.kc.callCount() != calls {
		t.Fatalf("read again inside its delay: %d -> %d %v", calls, e.kc.callCount(), err)
	}

	// Due again, more has happened, still running: the new observations are added once.
	e.kc.tasks[task2] = partial(whole, 2)
	e.exec(t, `UPDATE health_collection SET next_attempt_at = now() WHERE a2a_task_id = $1`, task2)
	if err := e.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := receipt(); state != string(dbstate.HealthPending) || events() <= midEvents {
		t.Fatalf("second read: %s, %d events (was %d)", state, events(), midEvents)
	}

	// The turn ends: the last read closes the receipt.
	e.kc.tasks[task2] = whole
	e.exec(t, `UPDATE deliveries SET state = 'completed' WHERE id = 'msg-2'`)
	e.exec(t, `UPDATE health_collection SET next_attempt_at = now() WHERE a2a_task_id = $1`, task2)
	if err := e.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := receipt(); state != string(dbstate.HealthCollected) {
		t.Fatalf("after the turn ended: %s", state)
	}
	calls = e.kc.callCount()
	if err := e.svc.Recover(ctx); err != nil || e.kc.callCount() != calls {
		t.Fatalf("a closed receipt was read: %v", err)
	}

	// The result is exactly what reading the finished turn once would have stored.
	once := newEnv(t)
	if err := once.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if got, want := e.dump(t), once.dump(t); got != want {
		t.Fatalf("running reads changed the totals:\n%s\n%s", got, want)
	}
}

// A late read of a running turn never takes a collected receipt back.
func TestOpenIngestDoesNotReopenACollectedReceipt(t *testing.T) {
	e := newEnv(t)
	e.ingest(t, e.tasks[task1], nil)
	before := e.dump(t)
	e.ingest(t, e.tasks[task1], func(in *health.Input) { in.Open, in.RereadAfter = true, time.Minute })
	if after := e.dump(t); after != before {
		t.Fatalf("an open read changed a collected receipt:\n%s\n%s", before, after)
	}
}

// Review P3-3: a receipt still pending after the event window is closed, and not read.
func TestStalePendingReceiptIsClosed(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.kc.fail[task1] = context.DeadlineExceeded
	if err := e.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	e.exec(t, `UPDATE health_collection SET receipt_at = now() - interval '30 days', next_attempt_at = now() WHERE a2a_task_id = $1`, task1)
	calls := e.kc.callCount()
	if err := e.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := e.pool.QueryRow(ctx, `SELECT state FROM health_collection WHERE a2a_task_id = $1`, task1).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != string(dbstate.HealthUnavailable) || e.kc.callCount() != calls {
		t.Fatalf("stale receipt: %s, reads %d -> %d", state, calls, e.kc.callCount())
	}
}

// Review P3-1 and P3-2: a session with no task keeps its title, and a delivery the collector can
// never discover (no kagent session) is not expected.
func TestUnassignedSessionTitleAndExpectedReceipts(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.exec(t, `INSERT INTO deliveries (id, idempotency_key, session_id, state, task_id) VALUES ('msg-3', 'key-3', 'session-1', 'completed', 'no-session-task')`)
	if err := e.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	v := e.view(t, health.Filter{})
	if len(v.Rows) != 1 || len(v.Rows[0].Sessions) != 1 {
		t.Fatalf("rows: %+v", v.Rows)
	}
	row := v.Rows[0]
	if s := row.Sessions[0]; s.Title == nil || *s.Title != "Fix the build" || s.TaskTitle != nil {
		t.Fatalf("session entry: %+v", s)
	}
	if row.ExpectedReceipts != 2 || row.Totals.Coverage == "partial" {
		t.Fatalf("expected %d receipts, coverage %q", row.ExpectedReceipts, row.Totals.Coverage)
	}
}

// Review P2-1: an open receipt keeps its turn partial even when every observation read so far looks
// complete, because the turn may still gain more.
func TestOpenReceiptKeepsCoveragePartial(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.exec(t, `UPDATE deliveries SET state = 'delivered' WHERE id = 'msg-2'`)
	if err := e.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	row := e.view(t, health.Filter{}).Rows[0]
	if len(row.Turns) != 2 || row.ExpectedReceipts != 2 || row.Totals.Coverage != "partial" || row.Sessions[0].Totals.Coverage != "partial" {
		t.Fatalf("turns %d, expected %d, row %q, session %q: an open receipt must keep coverage partial",
			len(row.Turns), row.ExpectedReceipts, row.Totals.Coverage, row.Sessions[0].Totals.Coverage)
	}
}

// Review P2-1: a running delivery is an expected receipt even before it has any observations. With
// its snapshot still empty, the row and its session are partial, not complete. The read that
// finishes the turn then gives the same coverage as reading the finished turn once.
func TestExpectedReceiptsIncludeRunning(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	whole := e.tasks[task2] // e.tasks is the fake's map: capture the whole task before it is replaced
	e.exec(t, `UPDATE deliveries SET state = 'delivered' WHERE id = 'msg-2'`)
	e.kc.tasks[task2] = partial(whole, 0)
	if err := e.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, "health_collection"); n != 2 {
		t.Fatalf("receipts = %d, want 2", n)
	}
	v := e.view(t, health.Filter{})
	if len(v.Rows) != 1 || len(v.Rows[0].Sessions) != 1 || len(v.Rows[0].Turns) != 1 {
		t.Fatalf("rows: %+v", v.Rows)
	}
	row := v.Rows[0]
	if row.ExpectedReceipts != 2 || row.Totals.Coverage != "partial" || row.Sessions[0].Totals.Coverage != "partial" {
		t.Fatalf("expected 2 receipts and partial coverage, got %d, row %q, session %q",
			row.ExpectedReceipts, row.Totals.Coverage, row.Sessions[0].Totals.Coverage)
	}

	e.kc.tasks[task2] = whole
	e.exec(t, `UPDATE deliveries SET state = 'completed' WHERE id = 'msg-2'`)
	e.exec(t, `UPDATE health_collection SET next_attempt_at = now() WHERE a2a_task_id = $1`, task2)
	if err := e.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	row = e.view(t, health.Filter{}).Rows[0]

	once := newEnv(t)
	if err := once.svc.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	want := once.view(t, health.Filter{}).Rows[0]
	if row.ExpectedReceipts != 2 || len(row.Gaps) != 0 || row.Totals.Coverage != want.Totals.Coverage || row.Sessions[0].Totals.Coverage != want.Sessions[0].Totals.Coverage {
		t.Fatalf("finished turn: expected %d, gaps %v, row %q, session %q; read once: row %q, session %q",
			row.ExpectedReceipts, row.Gaps, row.Totals.Coverage, row.Sessions[0].Totals.Coverage,
			want.Totals.Coverage, want.Sessions[0].Totals.Coverage)
	}
}
