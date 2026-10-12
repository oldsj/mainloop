package health_test

import (
	"context"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
)

// The terminal path owns the delivery before it ingests health in a savepoint. Discovery must
// skip that delivery without first inserting its receipt, even while its transaction stays open.
func TestReviewS1DiscoveryAndTerminalLockOrder(t *testing.T) {
	e := newEnv(t)
	e.exec(t, `UPDATE deliveries SET state = 'delivered', lease_owner = 'terminal' WHERE id = 'msg-1'`)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	terminal, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = terminal.Rollback(context.Background()) }()
	if n, err := dbq.New(terminal).FinishDelivery(ctx, dbq.FinishDeliveryParams{
		ID: "msg-1", LeaseOwner: ptr("terminal"), State: dbstate.DeliveryCompleted, TaskID: ptr(task1),
	}); err != nil || n != 1 {
		t.Fatalf("FinishDelivery: rows %d, error %v", n, err)
	}
	discovery, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = discovery.Rollback(context.Background()) }()
	var discoveryPID int
	if err := discovery.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&discoveryPID); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		rows int64
		err  error
	}
	discovered := make(chan outcome, 1)
	go func() {
		n, err := dbq.New(discovery).DiscoverHealthReceipts(ctx, dbq.DiscoverHealthReceiptsParams{
			OwnerID: owner, Gateway: gateway, Since: time.Now().Add(-7 * 24 * time.Hour), MaxRows: 500,
		})
		discovered <- outcome{rows: n, err: err}
	}()
	var result outcome
	var done bool
waitForDiscovery:
	for {
		select {
		case result = <-discovered:
			done = true
			break waitForDiscovery
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
		var blocked bool
		if err := e.pool.QueryRow(ctx, `SELECT cardinality(pg_blocking_pids($1)) > 0`, discoveryPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			// With the old query, proceed with ingest to expose the receipt/delivery lock cycle.
			break
		}
	}
	if done && result.err == nil {
		var heldReceipt bool
		if err := discovery.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM health_collection WHERE a2a_task_id = $1)`, task1).
			Scan(&heldReceipt); err != nil {
			t.Fatal(err)
		}
		if heldReceipt {
			t.Error("discovery inserted a receipt for the delivery owned by terminal ingest")
		}
	}
	sp, err := terminal.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ingestErr := e.svc.IngestDelivery(ctx, sp, "session-1", "msg-1", runtimeSession, time.Now(), e.tasks[task1])
	if ingestErr != nil {
		if err := sp.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
	} else if err := sp.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := terminal.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if !done {
		result = <-discovered
		t.Error("discovery waited on a locked delivery instead of skipping it")
	}
	if ingestErr != nil || result.err != nil {
		t.Fatalf("terminal ingest: %v; discovery: %v", ingestErr, result.err)
	}
	if result.rows != 1 {
		t.Fatalf("discovered %d receipts, want only the unlocked delivery", result.rows)
	}
	if err := discovery.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var final string
	var tracked bool
	if err := e.pool.QueryRow(ctx, `SELECT state, health_tracked FROM deliveries WHERE id = 'msg-1'`).Scan(&final, &tracked); err != nil || final != "completed" || !tracked {
		t.Fatalf("terminal state %s, tracked %t: %v", final, tracked, err)
	}
	if err := e.svc.Recover(ctx, fakeAgents{}); err != nil {
		t.Fatal(err)
	}
	if events, receipts := e.count(t, "health_events"), e.count(t, "health_collection"); events != 12 || receipts != 2 {
		t.Fatalf("recovery lost data: events %d, receipts %d", events, receipts)
	}
}

// One discovery can keep its batch locked without stopping another. A rollback leaves both
// its receipt and tracking flag absent, so the next pass can discover the delivery again.
func TestDiscoverySkipsLockedBatchAndRetriesAfterRollback(t *testing.T) {
	e := newEnv(t)
	e.exec(t, `UPDATE deliveries SET created_at = $1`, e.at)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	params := dbq.DiscoverHealthReceiptsParams{
		OwnerID: owner, Gateway: gateway, Since: time.Now().Add(-7 * 24 * time.Hour), MaxRows: 1,
	}
	first, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Rollback(context.Background()) }()
	if n, err := dbq.New(first).DiscoverHealthReceipts(ctx, params); err != nil || n != 1 {
		t.Fatalf("first discovery: rows %d, error %v", n, err)
	}
	var delivery string
	if err := first.QueryRow(ctx, `SELECT delivery_id FROM health_collection`).Scan(&delivery); err != nil || delivery != "msg-1" {
		t.Fatalf("equal timestamps must use ID order, discovered %q: %v", delivery, err)
	}
	q := dbq.New(e.pool)
	if n, err := q.DiscoverHealthReceipts(ctx, params); err != nil || n != 1 {
		t.Fatalf("concurrent discovery: rows %d, error %v", n, err)
	}
	if err := e.pool.QueryRow(ctx, `SELECT delivery_id FROM health_collection`).Scan(&delivery); err != nil || delivery != "msg-2" {
		t.Fatalf("concurrent discovery must skip msg-1 and track msg-2, got %q: %v", delivery, err)
	}
	if err := first.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var tracked bool
	if err := e.pool.QueryRow(ctx, `SELECT health_tracked FROM deliveries WHERE id = 'msg-1'`).Scan(&tracked); err != nil || tracked {
		t.Fatalf("rollback left msg-1 tracked %t: %v", tracked, err)
	}
	if n, err := q.DiscoverHealthReceipts(ctx, params); err != nil || n != 1 {
		t.Fatalf("discovery after rollback: rows %d, error %v", n, err)
	}
	if n, err := q.DiscoverHealthReceipts(ctx, params); err != nil || n != 0 {
		t.Fatalf("repeat discovery: rows %d, error %v", n, err)
	}
	var receipts, flags int
	if err := e.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM health_collection),
		(SELECT count(*) FROM deliveries WHERE health_tracked)`).Scan(&receipts, &flags); err != nil || receipts != 2 || flags != 2 {
		t.Fatalf("receipts %d, tracking flags %d: %v", receipts, flags, err)
	}
}

// The flag means a delivery has been tracked, even after its receipt is pruned. An unflagged
// delivery whose receipt already exists is marked without changing that receipt's collection state.
func TestDiscoveryUsesTrackingFlagAndPreservesExistingReceipt(t *testing.T) {
	e := newEnv(t)
	e.ingest(t, e.tasks[task1], nil)
	e.ingest(t, e.tasks[task2], nil)
	e.exec(t, `DELETE FROM health_collection WHERE a2a_task_id = $1`, task1)
	e.exec(t, `UPDATE deliveries SET health_tracked = false WHERE id = 'msg-2'`)
	before := e.dumpAll(t)
	if n, err := dbq.New(e.pool).DiscoverHealthReceipts(context.Background(), dbq.DiscoverHealthReceiptsParams{
		OwnerID: owner, Gateway: gateway, Since: time.Now().Add(-7 * 24 * time.Hour), MaxRows: 500,
	}); err != nil || n != 0 {
		t.Fatalf("discovery must preserve existing and pruned receipts: rows %d, error %v", n, err)
	}
	if after := e.dumpAll(t); after != before {
		t.Fatal("discovery recreated a pruned receipt or changed an existing receipt")
	}
	var flags int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM deliveries WHERE health_tracked`).Scan(&flags); err != nil || flags != 2 {
		t.Fatalf("tracking flags %d, want both deliveries tracked: %v", flags, err)
	}
}
