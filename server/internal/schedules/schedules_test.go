package schedules_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/httpx"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/kagent/kagenttest"
	"github.com/oldsj/mainloop/server/internal/schedules"
	"github.com/oldsj/mainloop/server/internal/testdb"
	kagentapi "github.com/oldsj/mainloop/server/third_party/kagentapi"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

const owner = "owner-1"

var ctx = context.Background()

type env struct {
	pool *pgxpool.Pool
	srv  *kagenttest.Server
	svc  *schedules.Service
	api  *schedules.SchedulesAPI
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := testdb.New(t).Pool
	srv := kagenttest.New()
	t.Cleanup(srv.Close)
	srv.AddAgent("kagent", "claude")
	kc, err := kagent.New(kagent.Config{GatewayURL: srv.URL, UserID: "mainloop"})
	if err != nil {
		t.Fatal(err)
	}
	svc := &schedules.Service{Pool: pool, Kagent: kc, OwnerID: owner}
	return &env{pool: pool, srv: srv, svc: svc, api: schedules.NewAPI(svc)}
}

func (e *env) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (e *env) scheduleEvents(t *testing.T) int {
	return e.count(t, `SELECT count(*) FROM events WHERE type = 'schedule:updated'`)
}

func create(t *testing.T, e *env, request string) api.ScheduledRunOutput {
	t.Helper()
	name := "daily"
	resp, err := e.api.CreateSchedule(ctx, api.CreateScheduleRequestObject{Body: &api.ScheduledRunCreate{
		Agent:     api.ScheduledRunAgentRef{Namespace: "kagent", Name: "claude"},
		RequestId: request,
		Config:    api.ScheduledRunConfigInput{Name: &name, Schedule: "0 9 * * *", Prompt: "report"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return resp.(api.CreateSchedule201JSONResponse).Body
}

func status(t *testing.T, err error) int {
	t.Helper()
	var he *httpx.Error
	if !errors.As(err, &he) {
		t.Fatalf("want an httpx error, got %v", err)
	}
	return he.Status
}

func TestCRUDEtagAndTrigger(t *testing.T) {
	e := newEnv(t)
	sc := create(t, e, "req-1")
	if e.scheduleEvents(t) != 1 {
		t.Fatalf("create published %d schedule:updated", e.scheduleEvents(t))
	}
	etag := `"` + sc.Etag + `"`
	resp, err := e.api.GetSchedule(ctx, api.GetScheduleRequestObject{ScheduleId: sc.Id})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.(api.GetSchedule200JSONResponse); got.Headers.ETag == nil || *got.Headers.ETag != etag {
		t.Fatalf("get ETag %v, want %s", got.Headers.ETag, etag)
	}

	body := api.ScheduledRunConfigInput{Schedule: "0 10 * * *", Prompt: "report, later"}
	// No If-Match is 428 and changes nothing.
	_, err = e.api.UpdateSchedule(ctx, api.UpdateScheduleRequestObject{ScheduleId: sc.Id, Body: &body})
	if status(t, err) != http.StatusPreconditionRequired {
		t.Fatalf("missing If-Match: %v", err)
	}
	bad := "not-an-etag"
	_, err = e.api.UpdateSchedule(ctx, api.UpdateScheduleRequestObject{
		ScheduleId: sc.Id, Body: &body, Params: api.UpdateScheduleParams{IfMatch: &bad}})
	if status(t, err) != http.StatusUnprocessableEntity {
		t.Fatalf("malformed If-Match: %v", err)
	}
	// A stale etag is 412 and does not apply.
	stale := `"` + uuid.NewString() + `"`
	_, err = e.api.UpdateSchedule(ctx, api.UpdateScheduleRequestObject{
		ScheduleId: sc.Id, Body: &body, Params: api.UpdateScheduleParams{IfMatch: &stale}})
	if status(t, err) != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match: %v", err)
	}
	if got, _ := e.srv.ScheduledRun(sc.Id); got.GetConfig().GetSchedule() != "0 9 * * *" {
		t.Fatal("a stale update was applied")
	}
	// The current etag applies, and the reply carries the next one.
	up, err := e.api.UpdateSchedule(ctx, api.UpdateScheduleRequestObject{
		ScheduleId: sc.Id, Body: &body, Params: api.UpdateScheduleParams{IfMatch: &etag}})
	if err != nil {
		t.Fatal(err)
	}
	next := up.(api.UpdateSchedule200JSONResponse)
	if next.Body.Config.Schedule != "0 10 * * *" || next.Body.Etag == sc.Etag || *next.Headers.ETag != `"`+next.Body.Etag+`"` {
		t.Fatalf("update reply: %+v", next)
	}
	if e.scheduleEvents(t) != 2 {
		t.Fatalf("update published %d events", e.scheduleEvents(t))
	}

	// Trigger is idempotent per request id.
	trig := func(id string) api.ScheduledRunExecutionOutput {
		r, err := e.api.TriggerSchedule(ctx, api.TriggerScheduleRequestObject{
			ScheduleId: sc.Id, Body: &api.ScheduledRunTriggerRequest{RequestId: id}})
		if err != nil {
			t.Fatal(err)
		}
		return api.ScheduledRunExecutionOutput(r.(api.TriggerSchedule202JSONResponse))
	}
	a, b := trig("run-1"), trig("run-1")
	if a.Id != b.Id || a.State != api.ScheduledRunExecutionOutputStatePending || a.SessionId != nil {
		t.Fatalf("trigger replay: %+v / %+v", a, b)
	}
	lr, err := e.api.ListScheduleExecutions(ctx, api.ListScheduleExecutionsRequestObject{ScheduleId: sc.Id})
	if err != nil || len(lr.(api.ListScheduleExecutions200JSONResponse).Items) != 1 {
		t.Fatalf("executions: %v %v", lr, err)
	}

	// List shows it; delete is idempotent and the schedule then reads as missing, history stays.
	ls, _ := e.api.ListSchedules(ctx, api.ListSchedulesRequestObject{})
	if len(ls.(api.ListSchedules200JSONResponse).Items) != 1 {
		t.Fatal("list does not show the schedule")
	}
	for range 2 {
		if _, err := e.api.DeleteSchedule(ctx, api.DeleteScheduleRequestObject{ScheduleId: sc.Id}); err != nil {
			t.Fatal(err)
		}
	}
	_, err = e.api.GetSchedule(ctx, api.GetScheduleRequestObject{ScheduleId: sc.Id})
	if status(t, err) != http.StatusNotFound {
		t.Fatalf("get after delete: %v", err)
	}
	// An update or a trigger of a deleted schedule is 409.
	_, err = e.api.UpdateSchedule(ctx, api.UpdateScheduleRequestObject{
		ScheduleId: sc.Id, Body: &body, Params: api.UpdateScheduleParams{IfMatch: &etag}})
	if s := status(t, err); s != http.StatusConflict && s != http.StatusPreconditionFailed {
		t.Fatalf("update after delete: %d", s)
	}
	_, err = e.api.TriggerSchedule(ctx, api.TriggerScheduleRequestObject{
		ScheduleId: sc.Id, Body: &api.ScheduledRunTriggerRequest{RequestId: "run-2"}})
	if status(t, err) != http.StatusConflict {
		t.Fatalf("trigger after delete: %v", err)
	}
	lr, err = e.api.ListScheduleExecutions(ctx, api.ListScheduleExecutionsRequestObject{ScheduleId: sc.Id})
	if err != nil || len(lr.(api.ListScheduleExecutions200JSONResponse).Items) != 1 {
		t.Fatalf("history after delete: %v %v", lr, err)
	}
}

func TestCreateReplayAndRefusals(t *testing.T) {
	e := newEnv(t)
	a, b := create(t, e, "req-1"), create(t, e, "req-1")
	if a.Id != b.Id || e.srv.ScheduleCount() != 1 {
		t.Fatalf("replay made a second schedule: %s %s", a.Id, b.Id)
	}
	if e.scheduleEvents(t) != 1 {
		t.Fatalf("a replay published again: %d events", e.scheduleEvents(t))
	}
	if e.count(t, `SELECT count(*) FROM scheduled_run_schedules WHERE request_id = 'req-1'`) != 1 {
		t.Fatal("the registry does not hold the request id")
	}

	mk := func(agent, prompt, req string) error {
		_, err := e.api.CreateSchedule(ctx, api.CreateScheduleRequestObject{Body: &api.ScheduledRunCreate{
			Agent: api.ScheduledRunAgentRef{Namespace: "kagent", Name: agent}, RequestId: req,
			Config: api.ScheduledRunConfigInput{Schedule: "0 9 * * *", Prompt: prompt},
		}})
		return err
	}
	if s := status(t, mk("claude", "other prompt", "req-1")); s != http.StatusConflict {
		t.Fatalf("request id reused with another body: %d", s)
	}
	if s := status(t, mk("nobody", "x", "req-2")); s != http.StatusNotFound {
		t.Fatalf("unknown Agent: %d", s)
	}
	if s := status(t, mk("claude", strings.Repeat("x", 32769), "req-3")); s != http.StatusUnprocessableEntity {
		t.Fatalf("oversized prompt: %d", s)
	}
	_, err := e.api.GetSchedule(ctx, api.GetScheduleRequestObject{ScheduleId: "nope"})
	if status(t, err) != http.StatusUnprocessableEntity {
		t.Fatalf("bad id: %v", err)
	}
	_, err = e.api.GetSchedule(ctx, api.GetScheduleRequestObject{ScheduleId: uuid.NewString()})
	if status(t, err) != http.StatusNotFound {
		t.Fatalf("unknown schedule: %v", err)
	}
}

func TestUnavailableWithoutKagent(t *testing.T) {
	a := schedules.NewAPI(&schedules.Service{Pool: testdb.New(t).Pool, OwnerID: owner})
	_, err := a.ListSchedules(ctx, api.ListSchedulesRequestObject{})
	if status(t, err) != http.StatusServiceUnavailable {
		t.Fatal(err)
	}
	if err := a.Svc.Adopt(ctx); err != nil {
		t.Fatalf("the sweep without kagent: %v", err)
	}
}

// fire makes a cron execution for a schedule, as kagent's scheduler would.
func fire(e *env, scheduleID string) *kagentapi.ScheduledRunExecution {
	return e.srv.Fire(scheduleID, time.Now())
}

func reserve(e *env, id, session string, state kagentapi.ScheduledRunExecutionState) {
	e.srv.EditExecution(id, func(x *kagentapi.ScheduledRunExecution) {
		x.SessionId, x.State = session, state
		if state != kagentapi.ScheduledRunExecutionState_SCHEDULED_RUN_EXECUTION_STATE_RUNNING &&
			state != kagentapi.ScheduledRunExecutionState_SCHEDULED_RUN_EXECUTION_STATE_PENDING {
			x.CompletedAt = timestamppb.Now()
		}
	})
}

const (
	running   = kagentapi.ScheduledRunExecutionState_SCHEDULED_RUN_EXECUTION_STATE_RUNNING
	succeeded = kagentapi.ScheduledRunExecutionState_SCHEDULED_RUN_EXECUTION_STATE_SUCCEEDED
	failedSt  = kagentapi.ScheduledRunExecutionState_SCHEDULED_RUN_EXECUTION_STATE_FAILED
)

func (e *env) sessionOf(t *testing.T, execution string) (id, status string) {
	t.Helper()
	err := e.pool.QueryRow(ctx, `SELECT s.id, s.status FROM scheduled_run_links l JOIN sessions s ON s.id = l.session_id
		WHERE l.execution_id = $1`, execution).Scan(&id, &status)
	if err != nil {
		t.Fatalf("no session for execution %s: %v", execution, err)
	}
	return id, status
}

func TestAdoptionTurnsAnExecutionIntoOneSession(t *testing.T) {
	e := newEnv(t)
	sc := create(t, e, "req-1")
	x := fire(e, sc.Id)

	// A pending execution with no kagent session is linked but has no session yet.
	if err := e.svc.Adopt(ctx); err != nil {
		t.Fatal(err)
	}
	if e.count(t, `SELECT count(*) FROM scheduled_run_links WHERE execution_id = $1`, x.GetId()) != 1 ||
		e.count(t, `SELECT count(*) FROM sessions`) != 0 {
		t.Fatal("expected a link and no session")
	}
	before := e.scheduleEvents(t)

	reserve(e, x.GetId(), "ks-1", running)
	for range 3 { // repeated passes change nothing after the first
		if err := e.svc.Adopt(ctx); err != nil {
			t.Fatal(err)
		}
	}
	sid, st := e.sessionOf(t, x.GetId())
	if st != "active" || sid != "sched_"+x.GetId() {
		t.Fatalf("session %s is %s", sid, st)
	}
	if e.count(t, `SELECT count(*) FROM sessions`) != 1 ||
		e.count(t, `SELECT count(*) FROM agent_bindings WHERE session_id = $1 AND kagent_session_id = 'ks-1'`, sid) != 1 {
		t.Fatal("expected exactly one session bound to the kagent session")
	}
	if got := e.scheduleEvents(t); got != before+1 {
		t.Fatalf("schedule:updated: %d, want %d (one for the session, none for the replays)", got, before+1)
	}

	// The session follows the execution to its end.
	reserve(e, x.GetId(), "ks-1", succeeded)
	if err := e.svc.Adopt(ctx); err != nil {
		t.Fatal(err)
	}
	if _, st := e.sessionOf(t, x.GetId()); st != "completed" {
		t.Fatalf("session status %s after success", st)
	}
	if e.count(t, `SELECT count(*) FROM attention_items WHERE source_id LIKE 'sched_exec_%'`) != 0 {
		t.Fatal("a success opened an inbox item")
	}

	// The REST list now names the Mainloop session.
	lr, err := e.api.ListScheduleExecutions(ctx, api.ListScheduleExecutionsRequestObject{ScheduleId: sc.Id})
	if err != nil {
		t.Fatal(err)
	}
	items := lr.(api.ListScheduleExecutions200JSONResponse).Items
	if len(items) != 1 || items[0].SessionId == nil || *items[0].SessionId != sid {
		t.Fatalf("execution list: %+v", items)
	}
}

func TestFailedExecutionOpensOneInboxItem(t *testing.T) {
	e := newEnv(t)
	sc := create(t, e, "req-1")
	x := fire(e, sc.Id)
	e.srv.EditExecution(x.GetId(), func(x *kagentapi.ScheduledRunExecution) {
		x.SessionId, x.State, x.FailureReason = "ks-1", failedSt, "the agent crashed"
		x.CompletedAt = timestamppb.Now()
	})
	for range 3 {
		if err := e.svc.Adopt(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM attention_items WHERE source_id = $1`, "sched_exec_"+x.GetId()); n != 1 {
		t.Fatalf("%d inbox items", n)
	}
	if e.count(t, `SELECT count(*) FROM events WHERE type = 'inbox:updated'`) == 0 {
		t.Fatal("the inbox item published no inbox event")
	}
	var statusv, why string
	if err := e.pool.QueryRow(ctx, `SELECT status, coalesce(error, '') FROM sessions`).Scan(&statusv, &why); err != nil {
		t.Fatal(err)
	}
	if statusv != "failed" || why != "the agent crashed" {
		t.Fatalf("session %s / %q", statusv, why)
	}
	// The attention sweep must not resolve it with the session: it is about the ending.
	if e.count(t, `SELECT count(*) FROM attention_items WHERE source_id = $1 AND session_id IS NOT NULL`, "sched_exec_"+x.GetId()) != 0 {
		t.Fatal("the item is tied to the session it reports on")
	}
}

func TestConcurrentSweepsAdoptOnce(t *testing.T) {
	e := newEnv(t)
	sc := create(t, e, "req-1")
	var ids []string
	for range 4 {
		x := fire(e, sc.Id)
		reserve(e, x.GetId(), "ks-"+x.GetId(), failedSt)
		ids = append(ids, x.GetId())
	}
	var wg sync.WaitGroup
	for range 4 { // four replicas, no runner lock: the link is the claim
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = e.svc.Adopt(ctx)
		}()
	}
	wg.Wait()
	if err := e.svc.Adopt(ctx); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t, `SELECT count(*) FROM sessions`); n != len(ids) {
		t.Fatalf("%d sessions for %d executions", n, len(ids))
	}
	if n := e.count(t, `SELECT count(*) FROM attention_items WHERE source_id LIKE 'sched_exec_%'`); n != len(ids) {
		t.Fatalf("%d inbox items for %d executions", n, len(ids))
	}
	if n := e.count(t, `SELECT count(*) FROM scheduled_run_links`); n != len(ids) {
		t.Fatalf("%d links", n)
	}
}

func TestExecutionsBeforeTheScheduleWasAdoptedAreIgnored(t *testing.T) {
	e := newEnv(t)
	sc := create(t, e, "req-1")
	old := fire(e, sc.Id)
	reserve(e, old.GetId(), "ks-old", failedSt)
	e.srv.EditExecution(old.GetId(), func(x *kagentapi.ScheduledRunExecution) {
		x.CreatedAt = timestamppb.New(time.Now().Add(-48 * time.Hour))
	})
	if err := e.svc.Adopt(ctx); err != nil {
		t.Fatal(err)
	}
	if e.count(t, `SELECT count(*) FROM scheduled_run_links`) != 0 || e.count(t, `SELECT count(*) FROM sessions`) != 0 {
		t.Fatal("history from before the schedule's creation was adopted")
	}
}

func TestDeletedScheduleIsDrainedOnceThenSettled(t *testing.T) {
	e := newEnv(t)
	sc := create(t, e, "req-1")
	x := fire(e, sc.Id)
	reserve(e, x.GetId(), "ks-1", succeeded)
	if _, err := e.api.DeleteSchedule(ctx, api.DeleteScheduleRequestObject{ScheduleId: sc.Id}); err != nil {
		t.Fatal(err)
	}
	// The execution that ended around the delete is still adopted.
	if err := e.svc.Adopt(ctx); err != nil {
		t.Fatal(err)
	}
	if _, st := e.sessionOf(t, x.GetId()); st != "completed" {
		t.Fatalf("session status %s", st)
	}
	if e.count(t, `SELECT count(*) FROM scheduled_run_schedules WHERE settled_at IS NOT NULL`) != 1 {
		t.Fatal("the deleted schedule was not settled")
	}
	// Settled schedules are not read again: a new execution on it would be missed by design, and no
	// call is made for it.
	if err := e.svc.Adopt(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSweepFindsASchedulePersistedOnlyInKagent(t *testing.T) {
	e := newEnv(t)
	// A create whose reply was lost: kagent has the schedule, the registry does not.
	sc, err := e.svc.Kagent.CreateScheduledRun(ctx, &kagentapi.CreateScheduledRunRequest{
		Agent: &kagentapi.ResourceReference{Namespace: "kagent", Name: "claude"}, RequestId: "lost",
		Config: &kagentapi.ScheduledRunConfig{Schedule: "0 9 * * *", Prompt: "p"},
	})
	if err != nil {
		t.Fatal(err)
	}
	x := fire(e, sc.GetId())
	reserve(e, x.GetId(), "ks-1", running)
	if err := e.svc.Adopt(ctx); err != nil {
		t.Fatal(err)
	}
	if _, st := e.sessionOf(t, x.GetId()); st != "active" {
		t.Fatalf("status %s", st)
	}
	// The retried create now finds the same schedule and attaches the request id.
	if _, err := e.api.CreateSchedule(ctx, api.CreateScheduleRequestObject{Body: &api.ScheduledRunCreate{
		Agent: api.ScheduledRunAgentRef{Namespace: "kagent", Name: "claude"}, RequestId: "lost",
		Config: api.ScheduledRunConfigInput{Schedule: "0 9 * * *", Prompt: "p"},
	}}); err != nil {
		t.Fatal(err)
	}
	if e.srv.ScheduleCount() != 1 || e.count(t, `SELECT count(*) FROM scheduled_run_schedules`) != 1 {
		t.Fatal("the retry made a second schedule")
	}
}
