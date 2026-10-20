package kagent_test

import (
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	api "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1alpha1"
)

var schedAgent = &api.ResourceReference{Namespace: "kagent", Name: "claude"}

func schedConfig(prompt string) *api.ScheduledRunConfig {
	return &api.ScheduledRunConfig{Name: "daily", Schedule: "0 9 * * *", Prompt: prompt}
}

func newSchedule(t *testing.T, c *kagent.Client, srv interface{ AddAgent(string, string) }, req string) *api.ScheduledRun {
	t.Helper()
	srv.AddAgent("kagent", "claude")
	sc, err := c.CreateScheduledRun(ctx, &api.CreateScheduledRunRequest{
		Agent: schedAgent, RequestId: req, Config: schedConfig("report"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

func grpcCode(t *testing.T, err error) connect.Code {
	t.Helper()
	return asType[*kagent.SessionError](t, err).Code
}

func TestScheduleCreateReplayAndDefaults(t *testing.T) {
	c, srv, _ := setup(t)
	a := newSchedule(t, c, srv, "req-1")
	if a.GetConfig().GetTimeZone() != "UTC" || a.GetConfig().GetExecutionTimeout().AsDuration() != 15*time.Minute {
		t.Fatalf("defaults not applied: %v", a.GetConfig())
	}
	b, err := c.CreateScheduledRun(ctx, &api.CreateScheduledRunRequest{
		Agent: schedAgent, RequestId: "req-1", Config: schedConfig("report"),
	})
	if err != nil || b.GetId() != a.GetId() || srv.ScheduleCount() != 1 {
		t.Fatalf("replay: %v, %v, %d schedules", b, err, srv.ScheduleCount())
	}
	_, err = c.CreateScheduledRun(ctx, &api.CreateScheduledRunRequest{
		Agent: schedAgent, RequestId: "req-1", Config: schedConfig("another prompt"),
	})
	if got := grpcCode(t, err); got != connect.CodeAlreadyExists {
		t.Fatalf("a different body under the same request id: code %v", got)
	}
}

func TestScheduleCreateErrors(t *testing.T) {
	c, srv, _ := setup(t)
	_, err := c.CreateScheduledRun(ctx, &api.CreateScheduledRunRequest{
		Agent: schedAgent, RequestId: "r", Config: schedConfig("x"),
	})
	if got := grpcCode(t, err); got != connect.CodeFailedPrecondition {
		t.Fatalf("a missing Agent: code %v", got)
	}
	srv.AddAgent("kagent", "claude")
	_, err = c.CreateScheduledRun(ctx, &api.CreateScheduledRunRequest{
		Agent: schedAgent, RequestId: "r", Config: &api.ScheduledRunConfig{Schedule: "@daily", Prompt: "x"},
	})
	if got := grpcCode(t, err); got != connect.CodeInvalidArgument {
		t.Fatalf("a macro schedule: code %v", got)
	}
}

func TestScheduleCreateRejectsAReplyForAnotherAgent(t *testing.T) {
	c, srv, _ := setup(t)
	srv.AddAgent("kagent", "claude")
	srv.TamperSchedule = func(_ string, s *api.ScheduledRun) { s.Agent.Name = "other" }
	_, err := c.CreateScheduledRun(ctx, &api.CreateScheduledRunRequest{
		Agent: schedAgent, RequestId: "r", Config: schedConfig("x"),
	})
	_ = asType[*kagent.OutcomeUnknown](t, err)
}

func TestScheduleUpdateEtag(t *testing.T) {
	c, srv, _ := setup(t)
	sc := newSchedule(t, c, srv, "r")
	cfg := schedConfig("new prompt")
	cfg.ExecutionTimeout = durationpb.New(time.Minute)
	up, err := c.UpdateScheduledRun(ctx, sc.GetId(), sc.GetEtag(), cfg)
	if err != nil || up.GetEtag() == sc.GetEtag() || up.GetConfig().GetPrompt() != "new prompt" {
		t.Fatalf("update: %v, %v", up, err)
	}
	// A stale etag is a definite refusal, not an unknown outcome.
	_, err = c.UpdateScheduledRun(ctx, sc.GetId(), sc.GetEtag(), cfg)
	notType[*kagent.OutcomeUnknown](t, err)
	if got := grpcCode(t, err); got != connect.CodeAborted {
		t.Fatalf("stale etag: code %v", got)
	}
	if _, err := c.DeleteScheduledRun(ctx, sc.GetId()); err != nil {
		t.Fatal(err)
	}
	_, err = c.UpdateScheduledRun(ctx, sc.GetId(), up.GetEtag(), cfg)
	if got := grpcCode(t, err); got != connect.CodeFailedPrecondition {
		t.Fatalf("a deleted schedule: code %v", got)
	}
	// An Aborted that is not an update's etag conflict stays an unknown outcome.
	srv.FailNext(kagent.ProcGetScheduledRun, kagenttest.Fault{GRPC: codePtr(connect.CodeAborted)})
	_, err = c.GetScheduledRun(ctx, sc.GetId())
	_ = asType[*kagent.OutcomeUnknown](t, err)
}

func TestScheduleGetListDelete(t *testing.T) {
	c, srv, _ := setup(t)
	var ids []string
	for _, r := range []string{"a", "b", "c"} {
		ids = append(ids, newSchedule(t, c, srv, r).GetId())
	}
	got, err := c.GetScheduledRun(ctx, ids[0])
	if err != nil || got.GetId() != ids[0] {
		t.Fatalf("get: %v, %v", got, err)
	}
	if _, err := c.GetScheduledRun(ctx, "missing"); grpcCode(t, err) != connect.CodeNotFound {
		t.Fatalf("a missing schedule: %v", err)
	}
	page, next, err := c.ListScheduledRunsPage(ctx, "", 2)
	if err != nil || len(page) != 2 || next == "" {
		t.Fatalf("page %d, next %q, err %v", len(page), next, err)
	}
	if page[0].GetId() >= page[1].GetId() {
		t.Fatal("the listing is not in id order")
	}
	if _, err := c.DeleteScheduledRun(ctx, ids[1]); err != nil {
		t.Fatal(err)
	}
	if again, err := c.DeleteScheduledRun(ctx, ids[1]); err != nil || again.GetDeletedAt() == nil {
		t.Fatalf("deleting twice: %v, %v", again, err)
	}
	all, err := c.ListScheduledRuns(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("a deleted schedule is not listed: %d, %v", len(all), err)
	}
	tomb, err := c.GetScheduledRun(ctx, ids[1])
	if err != nil || tomb.GetDeletedAt() == nil {
		t.Fatalf("a deleted schedule still reads as a tombstone: %v, %v", tomb, err)
	}
}

func TestScheduleTriggerAndExecutions(t *testing.T) {
	c, srv, _ := setup(t)
	sc := newSchedule(t, c, srv, "r")
	e1, err := c.TriggerScheduledRun(ctx, sc.GetId(), "now-1")
	if err != nil || e1.GetManualRequestId() != "now-1" || e1.GetPrompt() != "report" {
		t.Fatalf("trigger: %v, %v", e1, err)
	}
	again, err := c.TriggerScheduledRun(ctx, sc.GetId(), "now-1")
	if err != nil || again.GetId() != e1.GetId() {
		t.Fatalf("a repeated request id returns the same execution: %v, %v", again, err)
	}
	e2 := srv.Fire(sc.GetId(), time.Now())
	list, _, err := c.ListScheduledRunExecutionsPage(ctx, sc.GetId(), "", 10)
	if err != nil || len(list) != 2 || list[0].GetId() != e2.GetId() {
		t.Fatalf("executions are newest first: %v, %v", list, err)
	}
	got, err := c.GetScheduledRunExecution(ctx, e1.GetId())
	if err != nil || got.GetId() != e1.GetId() {
		t.Fatalf("get execution: %v, %v", got, err)
	}
	if _, err := c.TriggerScheduledRun(ctx, sc.GetId(), ""); grpcCode(t, err) != connect.CodeInvalidArgument {
		t.Fatalf("an empty request id: %v", err)
	}
	// After deletion, history stays and a repeated trigger returns its execution, but a new one is refused.
	if _, err := c.DeleteScheduledRun(ctx, sc.GetId()); err != nil {
		t.Fatal(err)
	}
	if again, err := c.TriggerScheduledRun(ctx, sc.GetId(), "now-1"); err != nil || again.GetId() != e1.GetId() {
		t.Fatalf("a repeated trigger after delete: %v, %v", again, err)
	}
	if _, err := c.TriggerScheduledRun(ctx, sc.GetId(), "now-2"); grpcCode(t, err) != connect.CodeFailedPrecondition {
		t.Fatalf("a new trigger on a deleted schedule: %v", err)
	}
	if list, _, err := c.ListScheduledRunExecutionsPage(ctx, sc.GetId(), "", 10); err != nil || len(list) != 2 {
		t.Fatalf("history survives deletion: %v, %v", list, err)
	}
}

func TestScheduleTriggerRejectsAnotherRequestsExecution(t *testing.T) {
	c, srv, _ := setup(t)
	sc := newSchedule(t, c, srv, "r")
	if _, err := c.TriggerScheduledRun(ctx, sc.GetId(), "one"); err != nil {
		t.Fatal(err)
	}
	// A reply that is for some other trigger must not be taken as this one's.
	srv.TamperExecution = func(e *api.ScheduledRunExecution) {
		e.Trigger = &api.ScheduledRunExecution_ManualRequestId{ManualRequestId: "other"}
	}
	_, err := c.TriggerScheduledRun(ctx, sc.GetId(), "one")
	_ = asType[*kagent.OutcomeUnknown](t, err)
}

func TestScheduleEmptyIDsNeverLeave(t *testing.T) {
	c, srv, _ := setup(t)
	if _, err := c.GetScheduledRun(ctx, ""); grpcCode(t, err) != connect.CodeInvalidArgument {
		t.Fatal(err)
	}
	if _, err := c.GetScheduledRunExecution(ctx, ""); grpcCode(t, err) != connect.CodeInvalidArgument {
		t.Fatal(err)
	}
	if _, _, err := c.ListScheduledRunExecutionsPage(ctx, "", "", 1); grpcCode(t, err) != connect.CodeInvalidArgument {
		t.Fatal(err)
	}
	if n := len(srv.Requests()); n != 0 {
		t.Fatalf("%d requests left", n)
	}
}
