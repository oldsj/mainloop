package tasks_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/tasks"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1alpha1"
)

type sendDecisionTraceKey struct{}

// Adapted from the S4 review probes: pause the real handler at the decision's fence or after
// its commit, and observe PostgreSQL lock waits. No production hooks or timing assumptions.
type sendDecisionTrace struct {
	beforeFence, afterCommit bool
	begun, marked            atomic.Bool
	pid                      atomic.Uint32
	started, paused, release chan struct{}
}

func (tr *sendDecisionTrace) pause(ctx context.Context) {
	close(tr.paused)
	select {
	case <-tr.release:
	case <-ctx.Done():
	}
}

func (tr *sendDecisionTrace) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.EqualFold(data.SQL, "begin") {
		tr.begun.Store(true)
	}
	if tr.beforeFence && strings.Contains(data.SQL, "-- name: LockSessionTaskAttempt") && tr.begun.CompareAndSwap(true, false) {
		tr.pause(ctx)
	}
	if strings.Contains(data.SQL, "-- name: MarkDeliverySending") && tr.marked.CompareAndSwap(false, true) {
		tr.pid.Store(conn.PgConn().PID())
		close(tr.started)
	}
	if tr.afterCommit && strings.EqualFold(data.SQL, "commit") && tr.marked.CompareAndSwap(true, false) {
		return context.WithValue(ctx, sendDecisionTraceKey{}, true)
	}
	return ctx
}

func (tr *sendDecisionTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx.Value(sendDecisionTraceKey{}) == true && data.Err == nil {
		tr.pause(ctx)
	}
}

func traceDispatch(t *testing.T, r *rig, mid string, tr *sendDecisionTrace) (<-chan error, func()) {
	t.Helper()
	tr.started, tr.paused, tr.release = make(chan struct{}), make(chan struct{}), make(chan struct{})
	pc := r.pool.Config()
	pc.ConnConfig.Tracer = tr
	pool, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		t.Fatal("open tracing pool")
	}
	t.Cleanup(pool.Close)
	r.ses.Pool = pool
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	var once sync.Once
	release := func() { once.Do(func() { close(tr.release) }) }
	t.Cleanup(release)
	done := make(chan error, 1)
	go func() {
		_, err := r.ses.DeliveryLoop().Handle(ctx, reconcile.Claim{ID: mid, State: "recorded", Owner: "send-order", Attempts: 1})
		done <- err
	}()
	return done, release
}

func waitDispatch(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("delivery handler did not finish")
	}
}

func waitDecisionPause(t *testing.T, tr *sendDecisionTrace, done <-chan error) {
	t.Helper()
	select {
	case <-tr.paused:
	case err := <-done:
		t.Fatalf("handler returned before the decision pause: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("decision pause not reached")
	}
}

func sendOrderProbe(t *testing.T) (*rig, *kagenttest.Server, string, string, string) {
	t.Helper()
	r, _, _, _, attempt, session := stopProbeProvision(t)
	f := kagenttest.New()
	f.AddAgent("kagent", "agent")
	t.Cleanup(f.Close)
	c, err := kagent.New(kagent.Config{GatewayURL: f.URL, UserID: "test-owner", RequestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	native, err := c.CreateSession(context.Background(), &kagentapi.CreateSessionRequest{
		Agent: &kagentapi.ResourceReference{Namespace: "kagent", Name: "agent"}, RequestId: uuid.NewString(), Name: "send order",
	})
	if err != nil {
		t.Fatal(err)
	}
	r.exec(`UPDATE agent_bindings SET kagent_session_id=$2 WHERE session_id=$1`, session, native.Id)
	r.briefRow(attempt, `state='completed'`)
	mid := uuid.NewString()
	if _, err := r.ses.SendTo(context.Background(), session, mid, "queued before the stop"); err != nil {
		t.Fatal(err)
	}
	r.exec(`UPDATE deliveries SET attempts=1, lease_owner='send-order', lease_until=now()+interval '1 hour' WHERE id=$1`, mid)
	r.ses.Kagent = c
	r.svc.Sessions = r.ses.Service
	r.svc.StopObserver = &tasks.KagentStops{Pool: r.pool, OwnerID: owner, Sessions: r.ses.Service, Kagent: c}
	return r, f, attempt, session, mid
}

func requestSendOrderStop(ctx context.Context, q *dbq.Queries, attempt string) error {
	_, err := q.MoveAttempt(ctx, dbq.MoveAttemptParams{
		ID: attempt, FromState: dbstate.AttemptActive, ToState: dbstate.AttemptDraining, CapacityHeld: true,
	})
	return err
}

func assertSendOrderStopPending(t *testing.T, r *rig, attempt string) {
	t.Helper()
	if got := r.str(`SELECT stop_state FROM task_attempts WHERE id=$1`, attempt); got != "requested" {
		t.Fatalf("stop = %s, want requested", got)
	}
	if slot, claim := r.held(attempt); slot != 1 || claim != 1 {
		t.Fatalf("pending stop holds slot=%d claim=%d, want both", slot, claim)
	}
	if r.count(`SELECT count(*) FROM task_attempts WHERE id=$1 AND 'runtime-stop-confirmed'=ANY(evidence_refs)`, attempt) != 0 {
		t.Fatal("a blocked confirmation appended stop-confirmed evidence")
	}
}

func finishSendOrderStop(t *testing.T, r *rig, attempt string) {
	t.Helper()
	r.eventually("stop confirms after the delivery settles", func() bool {
		r.sweep(r.svc.SettleSweep())
		return r.str(`SELECT stop_state FROM task_attempts WHERE id=$1`, attempt) == "confirmed"
	})
	if slot, claim := r.held(attempt); slot != 0 || claim != 0 {
		t.Fatalf("settled stop holds slot=%d claim=%d, want neither", slot, claim)
	}
}

func TestTaskStopWinsBeforeSendingDecision(t *testing.T) {
	r, f, attempt, _, mid := sendOrderProbe(t)
	tr := &sendDecisionTrace{beforeFence: true}
	done, release := traceDispatch(t, r, mid, tr)
	waitDecisionPause(t, tr, done)
	if got := r.str(`SELECT state FROM deliveries WHERE id=$1`, mid); got != "recorded" {
		t.Fatalf("delivery before decision = %s", got)
	}
	if err := requestSendOrderStop(context.Background(), dbq.New(r.pool), attempt); err != nil {
		t.Fatal(err)
	}
	release()
	waitDispatch(t, done)
	if got := r.str(`SELECT state FROM deliveries WHERE id=$1`, mid); got != "cancelled" || tr.marked.Load() {
		t.Fatalf("stop-first delivery = %s; sending statement reached=%v", got, tr.marked.Load())
	}
	if f.Count("SendStreamingMessage") != 0 {
		t.Fatal("stop-first dispatch made a native send")
	}
}

// Port of the reviewer's last-fence probe: a decision committed before stop must still send.
// Cancellation and an expired absent-message hold cannot erase its in-flight reservation.
func TestSendingDecisionBeforeStopStillSendsAndDelaysConfirmation(t *testing.T) {
	r, f, attempt, session, mid := sendOrderProbe(t)
	tr := &sendDecisionTrace{afterCommit: true}
	done, release := traceDispatch(t, r, mid, tr)
	waitDecisionPause(t, tr, done)
	if got := r.str(`SELECT state FROM deliveries WHERE id=$1`, mid); got != "sending" || f.Count("SendStreamingMessage") != 0 {
		t.Fatalf("committed decision: delivery=%s native sends=%d", got, f.Count("SendStreamingMessage"))
	}
	if err := requestSendOrderStop(context.Background(), dbq.New(r.pool), attempt); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ses.Cancel(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	if got := r.str(`SELECT state FROM deliveries WHERE id=$1`, mid); got != "sending" {
		t.Fatalf("cancel erased the sending decision: %s", got)
	}
	r.exec(`UPDATE deliveries SET updated_at=now()-interval '31 minutes' WHERE id=$1`, mid)
	r.sweep(r.svc.SettleSweep())
	assertSendOrderStopPending(t, r, attempt)
	release()
	waitDispatch(t, done)
	if got := r.str(`SELECT state FROM deliveries WHERE id=$1`, mid); got != "completed" || f.Count("SendStreamingMessage") != 1 {
		t.Fatalf("send-first result: delivery=%s native sends=%d", got, f.Count("SendStreamingMessage"))
	}
	finishSendOrderStop(t, r, attempt)
}

// Port of the reviewer's delivery-lock probe: while admission waits on the delivery, its shared
// attempt lock forces stop to wait. Neither action can decide from an old unfenced snapshot.
func TestSendingDecisionLocksAttemptBeforeDelivery(t *testing.T) {
	r, f, attempt, _, mid := sendOrderProbe(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	hold, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(context.Background()) }()
	var id string
	if err := hold.QueryRow(ctx, `SELECT id FROM deliveries WHERE id=$1 FOR UPDATE`, mid).Scan(&id); err != nil {
		t.Fatal(err)
	}
	tr := &sendDecisionTrace{afterCommit: true}
	done, release := traceDispatch(t, r, mid, tr)
	select {
	case <-tr.started:
	case <-ctx.Done():
		t.Fatal("sending decision was not reached")
	}
	r.eventually("sending statement waiting on the delivery row", func() bool {
		return r.count(`SELECT count(*) FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock'`, tr.pid.Load()) == 1
	})
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	stopPID := conn.Conn().PgConn().PID()
	stopDone := make(chan error, 1)
	go func() { stopDone <- requestSendOrderStop(ctx, dbq.New(conn), attempt) }()
	r.eventually("stop blocked by the sending decision's attempt lock", func() bool {
		return r.count(`SELECT count(*) FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock' AND $2=ANY(pg_blocking_pids(pid))`, stopPID, tr.pid.Load()) == 1
	})
	if got := r.str(`SELECT stop_state FROM task_attempts WHERE id=$1`, attempt); got != "none" {
		t.Fatalf("stop committed before admission: %s", got)
	}
	if err := hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	waitDecisionPause(t, tr, done)
	waitDispatch(t, stopDone)
	if got := r.str(`SELECT state FROM deliveries WHERE id=$1`, mid); got != "sending" {
		t.Fatalf("delivery after decision = %s", got)
	}
	q := dbq.New(r.pool)
	version, err := q.GetAttemptDeliveryOutcomeVersion(ctx, attempt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.ConfirmAttemptStop(ctx, dbq.ConfirmAttemptStopParams{ID: attempt, OutcomeVersion: version}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("confirmation with sending delivery = %v, want no rows", err)
	}
	assertSendOrderStopPending(t, r, attempt)
	release()
	waitDispatch(t, done)
	if f.Count("SendStreamingMessage") != 1 {
		t.Fatalf("native sends = %d, want one", f.Count("SendStreamingMessage"))
	}
	finishSendOrderStop(t, r, attempt)
}

func TestTaskStopWaitsForUnknownDeliveryReconciliation(t *testing.T) {
	for _, state := range []string{"sending", "uncertain"} {
		t.Run(state, func(t *testing.T) {
			r, f, attempt, session, mid := sendOrderProbe(t)
			native := r.str(`SELECT kagent_session_id FROM agent_bindings WHERE session_id=$1`, session)
			r.exec(`UPDATE deliveries SET state=$2, kagent_session_id=$3, updated_at=now()-interval '31 minutes' WHERE id=$1`, mid, state, native)
			if err := requestSendOrderStop(context.Background(), dbq.New(r.pool), attempt); err != nil {
				t.Fatal(err)
			}
			if _, err := r.ses.Cancel(context.Background(), session); err != nil {
				t.Fatal(err)
			}
			if got := r.str(`SELECT state FROM deliveries WHERE id=$1`, mid); got != state {
				t.Fatalf("cancel erased unknown outcome: %s", got)
			}
			r.sweep(r.svc.SettleSweep())
			assertSendOrderStopPending(t, r, attempt)
			// Recovery discovers the original message's final task; it must never replay the send.
			f.AddTask(&a2a.Task{ID: "recovered-task", ContextID: native,
				Status:  a2a.TaskStatus{State: a2a.TaskStateCompleted},
				History: []*a2a.Message{{ID: mid, ContextID: native, Role: a2a.MessageRoleUser}},
			})
			if _, err := r.ses.DeliveryLoop().Handle(context.Background(), reconcile.Claim{
				ID: mid, State: state, Owner: "send-order", Attempts: 1,
			}); err != nil {
				t.Fatal(err)
			}
			if got := r.str(`SELECT state FROM deliveries WHERE id=$1`, mid); got != "completed" || f.Count("SendStreamingMessage") != 0 {
				t.Fatalf("recovered delivery=%s native sends=%d", got, f.Count("SendStreamingMessage"))
			}
			finishSendOrderStop(t, r, attempt)
		})
	}
}
