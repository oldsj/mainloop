package schedules_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/hitl"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/sessions"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

// adoptedSession creates a real fake-backed native Session, then adopts a running execution on
// an Agent absent from every provider profile. The scheduled turn has no Mainloop delivery.
func adoptedSession(t *testing.T) (*env, *sessions.Service, *kagent.Client, string, string) {
	t.Helper()
	e := newEnv(t)
	client, err := kagent.New(kagent.Config{GatewayURL: e.srv.URL, UserID: "mainloop"})
	if err != nil {
		t.Fatal(err)
	}
	ks, err := client.CreateSession(ctx, &kagentapi.CreateSessionRequest{
		Agent: &kagentapi.ResourceReference{Namespace: "kagent", Name: "claude"}, RequestId: uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	sc := create(t, e, "req-1")
	x := fire(e, sc.Id)
	reserve(e, x.GetId(), ks.GetId(), running)
	if err := e.svc.Adopt(ctx); err != nil {
		t.Fatal(err)
	}
	sid, _ := e.sessionOf(t, x.GetId())
	svc := &sessions.Service{Pool: e.pool, Kagent: client, OwnerID: owner,
		Agent: kagent.AgentRef{Namespace: "kagent", Name: "another-agent"},
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return e, svc, client, sid, ks.GetId()
}

func TestCancelAdoptedScheduledSessionStopsNativeTasks(t *testing.T) {
	e, svc, _, sid, ksid := adoptedSession(t)
	e.srv.AddTask(&a2a.Task{ID: "scheduled-turn", ContextID: ksid, Status: a2a.TaskStatus{State: a2a.TaskStateWorking}})
	e.srv.EditExecution(scheduledExecutionID(t, e, sid), func(x *kagentapi.ScheduledRunExecution) { x.TaskId = "scheduled-turn" })
	e.srv.AddTask(&a2a.Task{ID: "finished-turn", ContextID: ksid, Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}})
	e.srv.AddTask(&a2a.Task{ID: "foreign-turn", ContextID: "another-context", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}})
	if n := e.count(t, `SELECT count(*) FROM deliveries WHERE session_id = $1`, sid); n != 0 {
		t.Fatalf("scheduled turn unexpectedly has %d deliveries", n)
	}
	// This query during the network call proves the cancelled intent committed first.
	e.srv.TamperTask = func(_ string, _ *a2a.Task) {
		if n := e.count(t, `SELECT count(*) FROM sessions WHERE id = $1 AND status = 'cancelled' AND agent_cancel_outcome = 'unknown'`, sid); n != 1 {
			t.Error("cancel intent was not committed before the native cancel")
		}
	}
	result, err := svc.Cancel(ctx, sid)
	if err != nil || result.Agent != sessions.AgentStopped || result.Status != "cancelled" {
		t.Fatalf("cancel %+v, %v", result, err)
	}
	for id, want := range map[a2a.TaskID]a2a.TaskState{
		"scheduled-turn": a2a.TaskStateCanceled, "finished-turn": a2a.TaskStateCompleted, "foreign-turn": a2a.TaskStateWorking,
	} {
		task, _ := e.srv.Task(id)
		if task.Status.State != want {
			t.Fatalf("task %s = %s, want %s", id, task.Status.State, want)
		}
	}
	if e.srv.Count("CancelTask") != 1 {
		t.Fatal("terminal or foreign task was cancelled")
	}
	again, err := svc.Cancel(ctx, sid)
	if err != nil || again.Agent != sessions.AgentStopped || e.srv.Count("CancelTask") != 1 {
		t.Fatalf("repeat cancel %+v, %v", again, err)
	}
}

func TestCancelAdoptedScheduledSessionReportsUnknown(t *testing.T) {
	for _, failure := range []string{"cancel-error", "list-error", "still-working", "wrong-task", "wrong-context"} {
		t.Run(failure, func(t *testing.T) {
			e, svc, _, sid, ksid := adoptedSession(t)
			e.srv.AddTask(&a2a.Task{ID: "scheduled-turn", ContextID: ksid, Status: a2a.TaskStatus{State: a2a.TaskStateWorking}})
			switch failure {
			case "cancel-error":
				e.srv.FailNext("CancelTask", kagenttest.Fault{Status: 500})
			case "list-error":
				e.srv.FailNext("ListTasks", kagenttest.Fault{Status: 500})
			default:
				e.srv.TamperTask = func(_ string, task *a2a.Task) {
					switch failure {
					case "still-working":
						task.Status.State = a2a.TaskStateWorking
					case "wrong-task":
						task.ID = "other-task"
					case "wrong-context":
						task.ContextID = "other-context"
					}
				}
			}
			result, err := svc.Cancel(ctx, sid)
			if err != nil || result.Agent != sessions.AgentUnknown || result.Status != "cancelled" {
				t.Fatalf("cancel %+v, %v", result, err)
			}
			calls := e.srv.Count("CancelTask")
			again, err := svc.Cancel(ctx, sid)
			if err != nil || again.Agent != sessions.AgentUnknown || e.srv.Count("CancelTask") != calls {
				t.Fatalf("repeat cancel %+v, %v", again, err)
			}
		})
	}
}

func TestAdoptedScheduledSessionResolvesAgentForHITLAndFollowup(t *testing.T) {
	e, svc, client, sid, ksid := adoptedSession(t)
	q := dbq.New(e.pool)
	b, err := q.GetOwnedSessionBinding(ctx, dbq.GetOwnedSessionBindingParams{ID: sid, UserID: owner})
	if err != nil {
		t.Fatal(err)
	}
	want := kagent.AgentRef{Namespace: "kagent", Name: "claude"}
	if b.Kind != "scheduled" || b.KagentCreateRequestID != nil || b.KagentCreateAgentNamespace != nil || b.KagentCreateAgentName != nil ||
		b.KagentAgentNamespace == nil || *b.KagentAgentNamespace != want.Namespace || b.KagentAgentName == nil || *b.KagentAgentName != want.Name {
		t.Fatalf("adopted binding %+v", b)
	}
	got, err := svc.ResolveAgent(&b.ResolvedAgentNamespace, &b.ResolvedAgentName, &b.Kind, b.Role)
	if err != nil || got != want {
		t.Fatalf("ResolveAgent = %+v, %v", got, err)
	}
	// Storing an adopted reference does not register observed Agents as create targets, even if
	// the binding later loses its native Session. Remove the owned-Agent fixture to detect a row.
	if _, err := e.pool.Exec(ctx, `DELETE FROM kagent_agents`); err != nil {
		t.Fatal(err)
	}
	if err := q.SetBindingAgentRef(ctx, dbq.SetBindingAgentRefParams{
		SessionID: sid, KagentAgentNamespace: &want.Namespace, KagentAgentName: &want.Name,
	}); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `SELECT count(*) FROM kagent_agents`); n != 0 {
		t.Fatalf("adoption registered %d create Agents", n)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE agent_bindings SET kagent_session_id = NULL WHERE session_id = $1`, sid); err != nil {
		t.Fatal(err)
	}
	if n, err := q.CountUnresolvedBindings(ctx); err != nil || n != 0 {
		t.Fatalf("unresolved bindings = %d, %v", n, err)
	}
	if _, err := q.SetBindingKagentSession(ctx, dbq.SetBindingKagentSessionParams{SessionID: sid, KagentSessionID: &ksid}); err != nil {
		t.Fatal(err)
	}
	msg := &a2a.Message{ID: "ask-message", Role: a2a.MessageRoleAgent, Extensions: []string{kagent.HITLExtension},
		Metadata: map[string]any{kagent.HITLExtension: map[string]any{
			"type": "ask_user_request", "id": "ask-1",
			"questions": []any{map[string]any{"question": "Which branch?", "choices": []any{"main", "dev"}, "multiple": false}},
		}},
	}
	e.srv.AddTask(&a2a.Task{ID: "scheduled-turn", ContextID: ksid,
		Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired, Message: msg}})
	observer := &hitl.Service{Pool: e.pool, OwnerID: owner, Kagent: hitl.FromClient(client), Agents: svc, Log: svc.Log}
	if err := observer.Observe(ctx); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `SELECT count(*) FROM hitl_requests WHERE session_id = $1 AND agent_namespace = 'kagent' AND agent_name = 'claude'`, sid); n != 1 {
		t.Fatalf("observer adopted %d asks", n)
	}
	// Finish the native turn, then send through the real delivery loop to the stored Agent.
	e.srv.AddTask(&a2a.Task{ID: "scheduled-turn", ContextID: ksid, Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}})
	mid := uuid.NewString()
	if _, err := svc.SendTo(ctx, sid, mid, "follow up"); err != nil {
		t.Fatal(err)
	}
	runner := reconcile.NewRunner(e.pool, svc.Log, nil)
	loop := svc.DeliveryLoop()
	loop.Interval = 10 * time.Millisecond
	runner.Add(loop)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- runner.Run(runCtx) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(15 * time.Second)
	for {
		delivery, err := q.GetDelivery(ctx, mid)
		if err != nil {
			t.Fatal(err)
		}
		if delivery.State == "completed" {
			break
		}
		if delivery.State == "failed" || time.Now().After(deadline) {
			t.Fatalf("follow-up state %s, detail %v", delivery.State, delivery.Detail)
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, req := range e.srv.Requests() {
		if req.Key == "ListTasks" || req.Key == "SendStreamingMessage" {
			if req.AgentPath != kagenttest.AgentPath("kagent", "claude") {
				t.Fatalf("%s asked Agent %s", req.Key, req.AgentPath)
			}
		}
	}
	if e.srv.Count("SendStreamingMessage") != 1 || e.srv.Count(kagent.ProcCreateSession) != 1 {
		t.Fatal("follow-up did not reuse the adopted native Session")
	}
}
