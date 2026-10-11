package tasks_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/sessions"
	"github.com/oldsj/mainloop/server/internal/tasks"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// Port of G8b review-4 P2-7: a completed activity status cannot restore a stopped writer's
// authority after generation two starts. The completed status is seeded, as in the probe.
func TestReview4CompletedSessionCannotRestartAfterFencing(t *testing.T) {
	r, _, sup, child, attempt, session := stopProbeProvision(t)
	f := kagenttest.New()
	t.Cleanup(f.Close)
	c, err := kagent.New(kagent.Config{GatewayURL: f.URL, UserID: "test-owner", RequestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	nativeSession, err := c.CreateSession(context.Background(), &kagentapi.CreateSessionRequest{
		Agent:     &kagentapi.ResourceReference{Namespace: "kagent", Name: "agent"},
		RequestId: uuid.NewString(), Name: "first code writer",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.AddTask(&a2a.Task{ID: "first-completed-turn", ContextID: nativeSession.Id,
		Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}})
	r.exec(`UPDATE agent_bindings SET kagent_session_id=$2 WHERE session_id=$1`, session, nativeSession.Id)
	r.ses.Kagent = c
	r.svc.Sessions = r.ses.Service
	r.svc.StopObserver = &tasks.KagentStops{Pool: r.pool, OwnerID: owner, Sessions: r.ses.Service, Kagent: c}
	r.briefRow(attempt, `state='completed', task_id='first-completed-turn', kagent_session_id=$2`, nativeSession.Id)
	r.exec(`UPDATE sessions SET status='completed' WHERE id=$1`, session)
	if _, err := r.svc.Cancel(context.Background(), r.own, action(child, r.view(r.own, child), uuid.NewString())); err != nil {
		t.Fatal(err)
	}
	r.run()
	r.eventually("first attempt fenced and cancelled", func() bool { return r.attemptState(child) == "cancelled" })
	second := r.codeChild(sup, "feat")
	r.eventually("second writer", func() bool { return r.opState(second) == "completed" })
	slot, claim := r.held(attempt)
	if slot != 0 || claim != 0 || r.attemptState(second) != "active" ||
		r.count(`SELECT writer_generation FROM task_attempts WHERE task_id=$1`, second) != 2 {
		t.Fatal("second writer must hold generation two after the first releases its resources")
	}
	if got := r.str(`SELECT status FROM sessions WHERE id=$1`, session); got != "completed" {
		t.Fatalf("first session status = %s, want completed", got)
	}
	mid := uuid.NewString()
	if _, err := r.ses.SendTo(context.Background(), session, mid, "resume writing on the released branch"); !errors.Is(err, sessions.ErrConflict) {
		t.Fatalf("continuation after fencing = %v, want conflict", err)
	}
	if r.count(`SELECT count(*) FROM deliveries WHERE id=$1`, mid) != 0 ||
		r.count(`SELECT count(*) FROM session_messages WHERE id=$1`, mid) != 0 || f.Count("SendStreamingMessage") != 0 {
		t.Fatal("a fenced attempt recorded or sent a new instruction")
	}
}

func TestStoppedTaskSessionDoesNotDispatchRecordedMessage(t *testing.T) {
	for _, stage := range []string{"requested", "unresolved", "confirmed", "settled"} {
		t.Run(stage, func(t *testing.T) {
			r, _, _, _, attempt, session := stopProbeProvision(t)
			f := kagenttest.New()
			t.Cleanup(f.Close)
			c, err := kagent.New(kagent.Config{GatewayURL: f.URL, UserID: "test-owner"})
			if err != nil {
				t.Fatal(err)
			}
			r.ses.Kagent = c
			r.briefRow(attempt, `state='completed'`)
			mid := uuid.NewString()
			if _, err := r.ses.SendTo(context.Background(), session, mid, "recorded before the stop"); err != nil {
				t.Fatal(err)
			}
			state, stopState := "draining", stage
			if stage == "settled" {
				state, stopState = "cancelled", "confirmed"
			}
			r.exec(`UPDATE task_attempts SET state=$2, stop_state=$3 WHERE id=$1`, attempt, state, stopState)
			r.exec(`UPDATE sessions SET status='completed' WHERE id=$1`, session)
			// Retain the recorded row and its lease: session cancellation must not be what protects it.
			r.exec(`UPDATE deliveries SET attempts=1, lease_owner='stopped-task', lease_until=now()+interval '1 hour' WHERE id=$1`, mid)
			ksid := "native-context"
			n, err := dbq.New(r.pool).MarkDeliverySending(context.Background(), dbq.MarkDeliverySendingParams{
				ID: mid, LeaseOwner: ptr("stopped-task"), Attempts: 1, KagentSessionID: &ksid,
			})
			if err != nil || n != 0 {
				t.Fatalf("MarkDeliverySending = %d, %v; want zero rows", n, err)
			}
			if _, err := r.ses.DeliveryLoop().Handle(context.Background(), reconcile.Claim{
				ID: mid, State: "recorded", Owner: "stopped-task", Attempts: 1,
			}); err != nil {
				t.Fatal(err)
			}
			if got := r.str(`SELECT state FROM deliveries WHERE id=$1`, mid); got != "cancelled" {
				t.Fatalf("delivery = %s, want cancelled", got)
			}
			for _, method := range []string{kagent.ProcGetSession, kagent.ProcCreateSession, kagent.ProcResumeSession, "SendStreamingMessage"} {
				if n := f.Count(method); n != 0 {
					t.Fatalf("native %s calls = %d, want zero", method, n)
				}
			}
			if _, err := r.ses.SendTo(context.Background(), session, uuid.NewString(), "new after stop"); !errors.Is(err, sessions.ErrConflict) {
				t.Fatalf("new message = %v, want conflict", err)
			}
		})
	}
}

type stopDuringPreparationClient struct {
	*kagent.Client
	stop func()
}

func (c *stopDuringPreparationClient) GetSession(ctx context.Context, id string) (*kagentapi.Session, error) {
	sess, err := c.Client.GetSession(ctx, id)
	if err == nil {
		c.stop()
	}
	return sess, err
}

func TestTaskStopDuringPreparationPreventsDispatch(t *testing.T) {
	r, _, _, _, attempt, session := stopProbeProvision(t)
	f := kagenttest.New()
	t.Cleanup(f.Close)
	c, err := kagent.New(kagent.Config{GatewayURL: f.URL, UserID: "test-owner"})
	if err != nil {
		t.Fatal(err)
	}
	native, err := c.CreateSession(context.Background(), &kagentapi.CreateSessionRequest{
		Agent:     &kagentapi.ResourceReference{Namespace: "kagent", Name: "agent"},
		RequestId: uuid.NewString(), Name: "preparing writer",
	})
	if err != nil {
		t.Fatal(err)
	}
	r.exec(`UPDATE agent_bindings SET kagent_session_id=$2 WHERE session_id=$1`, session, native.Id)
	r.briefRow(attempt, `state='completed'`)
	mid := uuid.NewString()
	if _, err := r.ses.SendTo(context.Background(), session, mid, "waiting for native readiness"); err != nil {
		t.Fatal(err)
	}
	r.exec(`UPDATE deliveries SET attempts=1, lease_owner='preparing-task', lease_until=now()+interval '1 hour' WHERE id=$1`, mid)
	r.ses.Kagent = &stopDuringPreparationClient{Client: c, stop: func() {
		// Commit a stop while the handler's native readiness read is in flight. The completed
		// session keeps its recorded delivery, so only the fresh attempt fence can prevent sending.
		r.exec(`UPDATE task_attempts SET state='draining', stop_state='requested' WHERE id=$1`, attempt)
		r.exec(`UPDATE sessions SET status='completed' WHERE id=$1`, session)
	}}
	if _, err := r.ses.DeliveryLoop().Handle(context.Background(), reconcile.Claim{
		ID: mid, State: "recorded", Owner: "preparing-task", Attempts: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if got := r.str(`SELECT state FROM deliveries WHERE id=$1`, mid); got != "cancelled" {
		t.Fatalf("delivery = %s, want cancelled", got)
	}
	if f.Count(kagent.ProcGetSession) != 1 || f.Count("SendStreamingMessage") != 0 {
		t.Fatal("preparation must read once, then observe the committed fence before dispatch")
	}
}
