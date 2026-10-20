package sessions_test

import (
	"context"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/hitl"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
)

// acceptingAnswer stands in for kagent taking the owner's answer: the task moves to the state next gives,
// with the response message in its history. Everything else is the real client.
type acceptingAnswer struct {
	hitl.Kagent
	srv  *kagenttest.Server
	next func(*a2a.Task)
}

func (a *acceptingAnswer) SendHITL(_ context.Context, _ kagent.AgentRef, s hitl.Send) error {
	task, ok := a.srv.Task(a2a.TaskID(s.TaskID))
	if !ok {
		return &kagent.Unreachable{Err: context.Canceled}
	}
	moved := *task
	moved.History = append(append([]*a2a.Message(nil), task.History...), &a2a.Message{
		ID: s.MessageID, Role: a2a.MessageRoleUser, Extensions: []string{kagent.HITLExtension},
		Metadata: map[string]any{kagent.HITLExtension: s.Native},
	})
	a.next(&moved)
	a.srv.AddTask(&moved)
	return nil
}

// parkAnAsk parks a turn on ask-1 and waits for its pending request. It then pushes the turn's
// delivery an hour out, as a long parked backoff would, and keeps it there against a pass that was
// already running. Only a wake can bring the delivery back before the test ends.
func parkAnAsk(t *testing.T, p *parkEnv) (requestID, deliveryID string) {
	t.Helper()
	ksid := p.firstTurn()
	parked := parkedTask("t-park", ksid, "ask-1", a2a.TaskStateInputRequired)
	p.srv.Script(kagenttest.Emit{Event: parked})
	p.srv.AddTask(parked)
	deliveryID = p.send("two")
	p.waitCount(1, "a pending request for the parked turn",
		`SELECT count(*) FROM hitl_requests WHERE task_id = 't-park' AND availability = 'pending'`)
	if err := p.pool.QueryRow(context.Background(),
		`SELECT id FROM hitl_requests WHERE task_id = 't-park' AND availability = 'pending'`).Scan(&requestID); err != nil {
		t.Fatal(err)
	}
	pushOut(t, p.pool, deliveryID)
	return requestID, deliveryID
}

// pushOut moves a delivered turn's next attempt an hour away, repeating until the move held over
// a pass in flight (one that returns after the update writes its own delay over it).
func pushOut(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for stable := 0; stable < 10; {
		if time.Now().After(deadline) {
			t.Fatal("could not hold the delivery's next attempt an hour away")
		}
		tag, err := pool.Exec(context.Background(), `UPDATE deliveries SET next_attempt_at = now() + interval '1 hour'
			WHERE id = $1 AND state = 'delivered' AND (lease_until IS NULL OR lease_until <= now())`, id)
		if err != nil {
			t.Fatal(err)
		}
		var far bool
		if err := pool.QueryRow(context.Background(),
			`SELECT next_attempt_at > now() + interval '30 minutes' FROM deliveries WHERE id = $1`, id).Scan(&far); err != nil {
			t.Fatal(err)
		}
		if tag.RowsAffected() == 1 && far {
			stable++
		} else {
			stable = 0
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func approve(t *testing.T, p *parkEnv, requestID, action string) {
	t.Helper()
	if _, err := p.hs.Respond(context.Background(), requestID, api.HITLDecisionInput{
		ActionId: action,
		Response: toolApproval(t, "t1"),
	}); err != nil {
		t.Fatal(err)
	}
}

func toolApproval(t *testing.T, id string) api.HITLDecisionInput_Response {
	t.Helper()
	var r api.HITLDecisionInput_Response
	if err := r.FromToolApprovalResponse(api.ToolApprovalResponse{
		Type: api.ToolApprovalResponseTypeToolApprovalResponse, Approvals: []api.ToolApproval{{Id: id, Approved: true}},
	}); err != nil {
		t.Fatal(err)
	}
	return r
}

// A slow answer: the delivery sleeps on a long parked backoff and the observer on a long scan
// period. When kagent takes the answer, the delivery is woken, reads the task parked on its next
// ask, and the hint shows that ask within a sweep.
func TestAnAcceptedAnswerWakesTheTurnSoTheNextAskAppearsAtOnce(t *testing.T) {
	t.Parallel()
	p := newParkEnv(t, time.Hour, true, func(k hitl.Kagent, srv *kagenttest.Server) hitl.Kagent {
		return &acceptingAnswer{Kagent: k, srv: srv, next: func(task *a2a.Task) {
			next := parkedTask(string(task.ID), task.ContextID, "ask-2", a2a.TaskStateInputRequired)
			task.Status = next.Status
		}}
	})
	requestID, deliveryID := parkAnAsk(t, p)

	approve(t, p, requestID, "act-1")

	p.waitCount(1, "a pending request for the second ask, though the delivery was parked for an hour and ScanEvery is an hour",
		`SELECT count(*) FROM hitl_requests WHERE task_id = 't-park' AND availability = 'pending' AND id <> $1`, requestID)
	if got := p.count(`SELECT count(*) FROM hitl_requests WHERE task_id = 't-park'`); got != 2 {
		t.Fatalf("requests for the task = %d, want 2 (the answered one and the new ask)", got)
	}
	if d := p.delivery(deliveryID); d.State != dbstate.DeliveryDelivered {
		t.Fatalf("the turn is still open on the second ask, but its delivery is %s", d.State)
	}
}

// The turn's final reply after an answer is mirrored to the conversation when kagent takes the
// answer, not after the parked backoff.
func TestAnAcceptedAnswerMirrorsTheFinalReplyWithoutWaitingForTheParkedBackoff(t *testing.T) {
	t.Parallel()
	p := newParkEnv(t, time.Hour, true, func(k hitl.Kagent, srv *kagenttest.Server) hitl.Kagent {
		return &acceptingAnswer{Kagent: k, srv: srv, next: func(task *a2a.Task) {
			task.Status = a2a.TaskStatus{State: a2a.TaskStateCompleted}
			task.Artifacts = []*a2a.Artifact{{ID: "a1", Parts: a2a.ContentParts{a2a.NewTextPart("all done")}}}
		}}
	})
	requestID, deliveryID := parkAnAsk(t, p)
	before := len(p.messages())

	approve(t, p, requestID, "act-1")

	p.waitState(deliveryID, dbstate.DeliveryCompleted)
	msgs := p.messages()
	if len(msgs) != before+1 || msgs[len(msgs)-1].Role != "assistant" || msgs[len(msgs)-1].Content != "all done" {
		t.Fatalf("messages after the answer = %+v, want the reply %q appended", msgs[before:], "all done")
	}
}

// WakeTurn makes only the delivery that follows that task due, and notifies the delivery loop.
func TestWakeTurnMakesOnlyThatTasksDeliveryDueAndNotifies(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	first := e.send("one")
	if _, err := e.pool.Exec(ctx, `UPDATE deliveries SET state = 'delivered', kagent_session_id = 'ks-1',
		task_id = 'task-1', next_attempt_at = now() + interval '1 hour', lease_owner = NULL, lease_until = NULL WHERE id = $1`, first); err != nil {
		t.Fatal(err)
	}

	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN mainloop_deliveries"); err != nil {
		t.Fatal(err)
	}

	// Another task of the session, or another session, is not this delivery.
	for _, other := range [][2]string{{"ks-1", "task-2"}, {"ks-2", "task-1"}} {
		if err := e.svc.WakeTurn(ctx, other[0], other[1]); err != nil {
			t.Fatal(err)
		}
	}
	var rested bool
	if err := e.pool.QueryRow(ctx, `SELECT next_attempt_at > now() + interval '30 minutes' FROM deliveries WHERE id = $1`, first).Scan(&rested); err != nil || !rested {
		t.Fatalf("a wake for another task moved the delivery: %v, %v", rested, err)
	}

	if err := e.svc.WakeTurn(ctx, "ks-1", "task-1"); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	n, err := conn.Conn().WaitForNotification(waitCtx)
	if err != nil || n.Payload != first {
		t.Fatalf("notification = %+v, %v; want payload %s", n, err, first)
	}
	var woke bool
	if err := e.pool.QueryRow(ctx, `SELECT next_attempt_at <= now() FROM deliveries WHERE id = $1`, first).Scan(&woke); err != nil || !woke {
		t.Fatalf("the delivery was not made due: %v, %v", woke, err)
	}

	// A delivery a claimer holds is left to that claimer.
	if _, err := e.pool.Exec(ctx, `UPDATE deliveries SET next_attempt_at = now() + interval '1 hour',
		lease_owner = 'w', lease_until = now() + interval '1 minute' WHERE id = $1`, first); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.WakeTurn(ctx, "ks-1", "task-1"); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx,
		`SELECT next_attempt_at > now() + interval '30 minutes' FROM deliveries WHERE id = $1`, first).Scan(&rested); err != nil || !rested {
		t.Fatalf("a leased delivery was moved: %v, %v", rested, err)
	}
}
