package tasks_test

import (
	"context"
	"errors"
	"iter"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// Adapted from the second S4 review probes: pause after the real stop observer has listed
// native work, before the confirmation statement takes its snapshot. No production hooks.
type stopConfirmationTrace struct {
	used            atomic.Bool
	paused, release chan struct{}
}

func (tr *stopConfirmationTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "-- name: ConfirmAttemptStop") && tr.used.CompareAndSwap(false, true) {
		close(tr.paused)
		select {
		case <-tr.release:
		case <-ctx.Done():
		}
	}
	return ctx
}

func (*stopConfirmationTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func pauseStopConfirmation(t *testing.T, r *rig) (<-chan error, func()) {
	t.Helper()
	tr := &stopConfirmationTrace{paused: make(chan struct{}), release: make(chan struct{})}
	pc := r.pool.Config()
	pc.ConnConfig.Tracer = tr
	pool, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		t.Fatal("open confirmation tracing pool")
	}
	t.Cleanup(pool.Close)
	r.svc.Pool = pool
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	var once sync.Once
	release := func() { once.Do(func() { close(tr.release) }) }
	t.Cleanup(release)
	done := make(chan error, 1)
	go func() { done <- r.svc.SettleSweep().Run(ctx) }()
	select {
	case <-tr.paused:
	case err := <-done:
		t.Fatalf("sweep returned before confirmation: %v", err)
	case <-ctx.Done():
		t.Fatal("confirmation boundary was not reached")
	}
	return done, release
}

// The real client streams a working task instead of the fake server's immediately completed
// default. It becomes visible only when the admitted native send starts.
type lateWorkingClient struct {
	*kagent.Client
	fake *kagenttest.Server
}

func (c *lateWorkingClient) SendMessage(ctx context.Context, agent kagent.AgentRef, p kagent.SendParams) iter.Seq2[a2a.Event, error] {
	native := &a2a.Task{ID: "late-working-task", ContextID: p.ContextID,
		Status:  a2a.TaskStatus{State: a2a.TaskStateWorking},
		History: []*a2a.Message{{ID: p.MessageID, ContextID: p.ContextID, Role: a2a.MessageRoleUser}}}
	c.fake.AddTask(native)
	c.fake.Script(kagenttest.Emit{Event: native})
	return c.Client.SendMessage(ctx, agent, p)
}

func lateDeliveredStop(t *testing.T) (*rig, *kagenttest.Server, string) {
	t.Helper()
	r, f, attempt, _, mid := sendOrderProbe(t)
	r.ses.Kagent = &lateWorkingClient{Client: r.ses.Kagent.(*kagent.Client), fake: f}
	sendTrace := &sendDecisionTrace{afterCommit: true}
	sendDone, releaseSend := traceDispatch(t, r, mid, sendTrace)
	waitDecisionPause(t, sendTrace, sendDone)
	if err := requestSendOrderStop(context.Background(), dbq.New(r.pool), attempt); err != nil {
		t.Fatal(err)
	}
	// Expire the absent-message hold while the admitted send has not yet reached native I/O.
	r.exec(`UPDATE deliveries SET updated_at=now()-interval '31 minutes' WHERE id=$1`, mid)
	stopDone, releaseConfirmation := pauseStopConfirmation(t, r)
	if got := r.str(`SELECT state FROM deliveries WHERE id=$1`, mid); got != "sending" || f.Count("SendStreamingMessage") != 0 {
		t.Fatalf("observer boundary: delivery=%s sends=%d", got, f.Count("SendStreamingMessage"))
	}
	if f.Count("ListTasks") == 0 {
		t.Fatal("observer did not list native work before confirmation")
	}
	// The native listing was empty. Let the admitted send land before SQL confirmation starts.
	releaseSend()
	waitDispatch(t, sendDone)
	if got := r.str(`SELECT state FROM deliveries WHERE id=$1`, mid); got != "delivered" || f.Count("SendStreamingMessage") != 1 {
		t.Fatalf("late send: delivery=%s sends=%d", got, f.Count("SendStreamingMessage"))
	}
	releaseConfirmation()
	waitDispatch(t, stopDone)
	assertSendOrderStopPending(t, r, attempt)
	native, exists := f.Task("late-working-task")
	if !exists || native.Status.State != a2a.TaskStateWorking {
		t.Fatal("late send did not leave a working native task")
	}
	return r, f, attempt
}

func TestLateDeliveredTaskCannotBypassConfirmation(t *testing.T) {
	t.Parallel()
	r, f, attempt := lateDeliveredStop(t)
	// The next native observation must list the task even though its delivery is now delivered
	// and no longer appears in the possibly-landing-deliveries query.
	cancels := f.Count("CancelTask")
	r.eventually("next observation finds and cancels the late delivered task", func() bool {
		r.sweep(r.svc.SettleSweep())
		return f.Count("CancelTask") > cancels
	})
	assertSendOrderStopPending(t, r, attempt)
	native, exists := f.Task("late-working-task")
	if !exists || native.Status.State != a2a.TaskStateCanceled {
		t.Fatal("next observation did not cancel the late task")
	}
	// A cancellation response is not confirmation: one more observation sees it native-final.
	finishSendOrderStop(t, r, attempt)
	if f.Count("SendStreamingMessage") != 1 {
		t.Fatal("late delivery was replayed")
	}
}

func TestRecoveredWorkingTaskCannotBypassConfirmation(t *testing.T) {
	t.Parallel()
	r, f, attempt, session, mid := sendOrderProbe(t)
	nativeID := r.str(`SELECT kagent_session_id FROM agent_bindings WHERE session_id=$1`, session)
	r.exec(`UPDATE deliveries SET state='uncertain', kagent_session_id=$2, updated_at=now()-interval '31 minutes' WHERE id=$1`, mid, nativeID)
	if err := requestSendOrderStop(context.Background(), dbq.New(r.pool), attempt); err != nil {
		t.Fatal(err)
	}
	stopDone, releaseConfirmation := pauseStopConfirmation(t, r)
	if got := r.str(`SELECT state FROM deliveries WHERE id=$1`, mid); got != "uncertain" {
		t.Fatalf("before recovery: delivery=%s", got)
	}
	// Previously unknown native work appears after the empty listing. Recovery adopts the
	// original message's working task, without sending it again.
	f.AddTask(&a2a.Task{ID: "recovered-working-task", ContextID: nativeID,
		Status:  a2a.TaskStatus{State: a2a.TaskStateWorking},
		History: []*a2a.Message{{ID: mid, ContextID: nativeID, Role: a2a.MessageRoleUser}}})
	if _, err := r.ses.DeliveryLoop().Handle(context.Background(), reconcile.Claim{ID: mid, State: "uncertain", Owner: "send-order", Attempts: 1}); err != nil {
		t.Fatal(err)
	}
	if got := r.str(`SELECT state FROM deliveries WHERE id=$1`, mid); got != "delivered" || f.Count("SendStreamingMessage") != 0 {
		t.Fatalf("after recovery: delivery=%s replay sends=%d", got, f.Count("SendStreamingMessage"))
	}
	releaseConfirmation()
	waitDispatch(t, stopDone)
	assertSendOrderStopPending(t, r, attempt)
	native, exists := f.Task("recovered-working-task")
	if !exists || native.Status.State != a2a.TaskStateWorking {
		t.Fatal("recovery did not leave a working native task")
	}
	cancels := f.Count("CancelTask")
	r.eventually("next observation finds the recovered working task", func() bool {
		r.sweep(r.svc.SettleSweep())
		return f.Count("CancelTask") > cancels
	})
	assertSendOrderStopPending(t, r, attempt)
	finishSendOrderStop(t, r, attempt)
	if f.Count("SendStreamingMessage") != 0 {
		t.Fatal("uncertain delivery was replayed")
	}
}

func TestStopConfirmsOnceLateDeliveredTaskFinishes(t *testing.T) {
	t.Parallel()
	r, f, attempt := lateDeliveredStop(t)
	native, _ := f.Task("late-working-task")
	native.Status.State = a2a.TaskStateCompleted
	f.AddTask(native)
	lists := f.Count("ListTasks")
	finishSendOrderStop(t, r, attempt)
	if f.Count("ListTasks") <= lists || f.Count("SendStreamingMessage") != 1 {
		t.Fatal("confirmation did not observe the finished task, or replayed its message")
	}
}

// A transaction that starts before observation can commit an outcome after it. Its now()
// timestamp is stale, but the committed version must still invalidate that observation.
func TestStopObservationRejectsLateFinalOutcome(t *testing.T) {
	t.Parallel()
	for _, state := range []dbstate.DeliveryState{dbstate.DeliveryCompleted, dbstate.DeliveryFailed, dbstate.DeliveryCancelled} {
		t.Run(string(state), func(t *testing.T) {
			r, _, attempt, _, mid := sendOrderProbe(t)
			ctx := context.Background()
			if err := requestSendOrderStop(ctx, dbq.New(r.pool), attempt); err != nil {
				t.Fatal(err)
			}
			tx, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			q := dbq.New(r.pool)
			version, err := q.GetAttemptDeliveryOutcomeVersion(ctx, attempt)
			if err != nil {
				t.Fatal(err)
			}
			leaseOwner := "send-order"
			if n, err := dbq.New(tx).FinishDelivery(ctx, dbq.FinishDeliveryParams{
				ID: mid, LeaseOwner: &leaseOwner, Attempts: 1, State: state,
			}); err != nil || n != 1 {
				t.Fatalf("finish late outcome: rows=%d err=%v", n, err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := q.ConfirmAttemptStop(ctx, dbq.ConfirmAttemptStopParams{ID: attempt, OutcomeVersion: version}); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("stale confirmation = %v, want no rows", err)
			}
			assertSendOrderStopPending(t, r, attempt)
			finishSendOrderStop(t, r, attempt)
		})
	}
}
