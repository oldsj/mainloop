package sessions_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/health"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
)

const healthGateway = "http://kagent.test"

// healthTask is a finished task for the given Session that carries one health observation.
func healthTask(taskID, ctxID string) *a2a.Task {
	const epoch = "22222222-2222-4222-8222-222222222222"
	return &a2a.Task{
		ID: a2a.TaskID(taskID), ContextID: ctxID,
		Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
		Artifacts: []*a2a.Artifact{{
			ID: epoch + ":1", Name: health.Schema,
			Parts: a2a.ContentParts{a2a.NewDataPart(map[string]any{
				"schema_version": health.Schema, "event_id": epoch + ":1", "producer_epoch": epoch, "sequence": 1,
				"kind": "turn", "occurred_at": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
				"time_source": "adapter_observed", "time_precision": "nanosecond", "provider": "claude",
				"runtime_version": "2.1.260", "source": "claude_result_stream", "turn_key": epoch,
				"runtime_session_id": ctxID, "a2a_task_id": taskID,
				"turn": map[string]any{"state": "started", "coverage": "partial"},
			})},
		}},
	}
}

func (e *env) healthService() *health.Service {
	return &health.Service{
		Pool: e.pool, OwnerID: owner, Gateway: healthGateway, EventRetention: 7 * 24 * time.Hour,
		SummaryRetention: 90 * 24 * time.Hour,
	}
}

// settleHealthTurn completes a first turn (which creates the Session), then ends a second turn
// with a task that carries a health observation, and returns the second delivery's id.
func (e *env) settleHealthTurn(task func(ctxID string) *a2a.Task) string {
	e.t.Helper()
	stop := e.run()
	defer stop()
	e.waitState(e.send("first"), dbstate.DeliveryCompleted)
	m, err := e.svc.EnsureMain(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	done := task(e.bindingKsid(m.SessionID))
	e.srv.AddTask(done)
	e.srv.Script(kagenttest.Emit{Event: done})
	id := e.send("second")
	e.waitState(id, dbstate.DeliveryCompleted)
	return id
}

func (e *env) healthRows() (events, receipts, collected int, tracked bool, deliveryID string) {
	e.t.Helper()
	err := e.pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM health_events),
		(SELECT count(*) FROM health_collection),
		(SELECT count(*) FROM health_collection WHERE state = 'collected'),
		coalesce((SELECT d.health_tracked FROM deliveries d WHERE d.task_id = 't-health'), false),
		coalesce((SELECT delivery_id FROM health_collection LIMIT 1), '')`).Scan(&events, &receipts, &collected, &tracked, &deliveryID)
	if err != nil {
		e.t.Fatal(err)
	}
	return
}

// The turn's observations and the receipt that says they were read commit with the delivery's
// terminal state: nothing waits for the recovery sweep, and the delivery leaves its discovery walk.
func TestAFinishedTurnStoresItsHealthObservationsInTheSettlingTransaction(t *testing.T) {
	e := newEnv(t)
	e.svc.Health = e.healthService()
	id := e.settleHealthTurn(func(ctxID string) *a2a.Task { return healthTask("t-health", ctxID) })
	events, receipts, collected, tracked, delivery := e.healthRows()
	if events != 1 || receipts != 1 || collected != 1 || !tracked || delivery != id {
		t.Fatalf("events %d, receipts %d, collected %d, tracked %v, receipt delivery %q (want %q)",
			events, receipts, collected, tracked, delivery, id)
	}
	// A replay of the same task, as the sweep would do, is a no-op.
	d := e.delivery(id)
	var turns int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM health_turns`).Scan(&turns); err != nil || turns != 1 {
		t.Fatalf("turns %d, err %v", turns, err)
	}
	if d.State != dbstate.DeliveryCompleted {
		t.Fatalf("state %s", d.State)
	}
}

// A task with no health artifact stores nothing and marks nothing, so the sweep still discovers it.
func TestATurnWithoutHealthArtifactsLeavesTheDeliveryToTheSweep(t *testing.T) {
	e := newEnv(t)
	e.svc.Health = e.healthService()
	e.settleHealthTurn(func(ctxID string) *a2a.Task {
		task := healthTask("t-health", ctxID)
		task.Artifacts = nil
		return task
	})
	if events, receipts, _, tracked, _ := e.healthRows(); events != 0 || receipts != 0 || tracked {
		t.Fatalf("events %d, receipts %d, tracked %v", events, receipts, tracked)
	}
}

// brokenSink fails inside the transaction, after a statement error that would abort it without a
// savepoint.
type brokenSink struct{}

func (brokenSink) IngestDelivery(ctx context.Context, tx pgx.Tx, _, deliveryID, _ string, _ time.Time, _ *a2a.Task) error {
	if _, err := tx.Exec(ctx, `UPDATE deliveries SET health_tracked = true WHERE id = $1`, deliveryID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO no_such_table VALUES (1)`)
	return errors.Join(errors.New("ingest failed"), err)
}

// An ingest that fails does not undo the delivery's terminal state, the reply it mirrors or the
// event it publishes, and leaves nothing of its own behind (not even its earlier writes), so the
// sweep finds the delivery untracked.
func TestAFailedHealthIngestKeepsTheTerminalStateAndLeavesTheDeliveryToTheSweep(t *testing.T) {
	e := newEnv(t)
	e.svc.Health = brokenSink{}
	id := e.settleHealthTurn(func(ctxID string) *a2a.Task { return healthTask("t-health", ctxID) })
	if _, receipts, _, tracked, _ := e.healthRows(); receipts != 0 || tracked {
		t.Fatalf("receipts %d, tracked %v", receipts, tracked)
	}
	if msgs := e.messages(); len(msgs) != 3 || msgs[2].Content != "second" {
		t.Fatalf("messages %+v", msgs)
	}
	if evs := e.sessionEvents(); len(evs) != 2 {
		t.Fatalf("events %+v", evs)
	}
	if st := e.delivery(id).State; st != dbstate.DeliveryCompleted {
		t.Fatalf("state %s", st)
	}
}
