package schedules_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
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
	srv := kagenttest.New()
	t.Cleanup(srv.Close)
	srv.AddAgent("kagent", "claude")
	return newEnvOn(t, srv, "claude")
}

// newEnvOn is one Mainloop instance (its own database) on a shared kagent, owning one Agent. The
// safety window is off so executions made a moment ago settle; its own test turns it on.
func newEnvOn(t *testing.T, srv *kagenttest.Server, agent string) *env {
	t.Helper()
	pool := testdb.New(t).Pool
	if _, err := pool.Exec(ctx, `INSERT INTO kagent_agents (namespace, name) VALUES ('kagent', $1)`, agent); err != nil {
		t.Fatal(err)
	}
	kc, err := kagent.New(kagent.Config{GatewayURL: srv.URL, UserID: "mainloop"})
	if err != nil {
		t.Fatal(err)
	}
	svc := &schedules.Service{Pool: pool, Kagent: kc, OwnerID: owner, Limits: schedules.Limits{CursorSafetyWindow: -1}}
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

// expireLeases ends every create attempt's lease early, as if its time had run out.
func (e *env) expireLeases(t *testing.T) {
	t.Helper()
	if _, err := e.pool.Exec(ctx, `UPDATE scheduled_run_create_attempts SET expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
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
	if s := status(t, err); s != http.StatusConflict {
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
	// An Agent outside this deployment's set is refused before anything is written or sent.
	if s := status(t, mk("not-ours", "x", "req-2")); s != http.StatusForbidden {
		t.Fatalf("foreign Agent: %d", s)
	}
	if e.srv.ScheduleCount() != 1 || e.count(t, `SELECT count(*) FROM scheduled_run_schedules`) != 1 {
		t.Fatal("a refused create left state behind")
	}
	// One of ours that kagent does not have is a definite refusal. The route returns it and ends its
	// own lease but keeps the intent; the sweep's replay is refused too, with nothing else in flight,
	// and only that drops the intent.
	if _, err := e.pool.Exec(ctx, `INSERT INTO kagent_agents (namespace, name) VALUES ('kagent', 'ghost')`); err != nil {
		t.Fatal(err)
	}
	if s := status(t, mk("ghost", "x", "req-2")); s != http.StatusNotFound {
		t.Fatalf("Agent kagent does not have: %d", s)
	}
	if e.count(t, `SELECT count(*) FROM scheduled_run_schedules WHERE request_id = 'req-2'`) != 1 ||
		e.count(t, `SELECT count(*) FROM scheduled_run_create_attempts`) != 0 {
		t.Fatal("a route refusal must keep the intent and end its own lease")
	}
	if err := e.svc.Adopt(ctx); err != nil {
		t.Fatal(err)
	}
	if e.count(t, `SELECT count(*) FROM scheduled_run_schedules WHERE request_id = 'req-2'`) != 0 {
		t.Fatal("the sweep's refused replay did not drop the intent")
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

// lostReply sends a create to kagent and then reports the reply as lost, noting whether the intent
// row was already written when the create left.
type lostReply struct {
	schedules.Kagent
	pool       *pgxpool.Pool
	lose       bool
	intentSeen int
}

func (l *lostReply) CreateScheduledRun(c context.Context, req *kagentapi.CreateScheduledRunRequest) (*kagentapi.ScheduledRun, error) {
	var n int
	_ = l.pool.QueryRow(c, `SELECT count(*) FROM scheduled_run_schedules WHERE request_id = $1 AND state = 'intent'`,
		req.GetRequestId()).Scan(&n)
	l.intentSeen += n
	sc, err := l.Kagent.CreateScheduledRun(c, req)
	if err == nil && l.lose {
		return nil, &kagent.OutcomeUnknown{Op: "CreateScheduledRun", Detail: "reply lost"}
	}
	return sc, err
}

func createBody(request string) *api.ScheduledRunCreate {
	return &api.ScheduledRunCreate{
		Agent: api.ScheduledRunAgentRef{Namespace: "kagent", Name: "claude"}, RequestId: request,
		Config: api.ScheduledRunConfigInput{Schedule: "0 9 * * *", Prompt: "report"},
	}
}

func TestLostCreateReplyIsRecoveredByTheSweep(t *testing.T) {
	e := newEnv(t)
	l := &lostReply{Kagent: e.svc.Kagent, pool: e.pool, lose: true}
	svc := &schedules.Service{Pool: e.pool, Kagent: l, OwnerID: owner, Limits: schedules.Limits{CursorSafetyWindow: -1}}
	_, err := schedules.NewAPI(svc).CreateSchedule(ctx, api.CreateScheduleRequestObject{Body: createBody("lost")})
	if status(t, err) != http.StatusBadGateway {
		t.Fatalf("a lost reply: %v", err)
	}
	if l.intentSeen != 1 {
		t.Fatal("the create was sent before its intent was written")
	}
	if e.srv.ScheduleCount() != 1 || e.count(t, `SELECT count(*) FROM scheduled_run_schedules WHERE state = 'intent'`) != 1 {
		t.Fatal("expected kagent to hold the schedule and the registry only the intent")
	}
	// The attempt's lease is still live (the create may yet land), so the sweep leaves the intent be.
	l.lose = false
	if err := svc.Adopt(ctx); err != nil {
		t.Fatal(err)
	}
	if e.count(t, `SELECT count(*) FROM scheduled_run_schedules WHERE state = 'intent'`) != 1 {
		t.Fatal("the sweep replayed an intent whose lease was still live")
	}
	// Once the lease has run out the sweep replays it, and the replay is also the read.
	e.expireLeases(t)
	if err := svc.Adopt(ctx); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := e.pool.QueryRow(ctx, `SELECT schedule_id FROM scheduled_run_schedules WHERE request_id = 'lost' AND state = 'created'`).Scan(&id); err != nil {
		t.Fatalf("the sweep did not finish the create: %v", err)
	}
	if e.srv.ScheduleCount() != 1 || e.scheduleEvents(t) != 1 {
		t.Fatalf("recovery made %d schedules and %d events", e.srv.ScheduleCount(), e.scheduleEvents(t))
	}
	// The client's retry now finds the same schedule and publishes nothing.
	resp, err := schedules.NewAPI(svc).CreateSchedule(ctx, api.CreateScheduleRequestObject{Body: createBody("lost")})
	if err != nil || resp.(api.CreateSchedule201JSONResponse).Body.Id != id || e.scheduleEvents(t) != 1 {
		t.Fatalf("retry: %v %v", resp, err)
	}
}

func TestLostCreateReplyIsRecoveredByTheClientsRetry(t *testing.T) {
	e := newEnv(t)
	l := &lostReply{Kagent: e.svc.Kagent, pool: e.pool, lose: true}
	svc := &schedules.Service{Pool: e.pool, Kagent: l, OwnerID: owner}
	a := schedules.NewAPI(svc)
	if _, err := a.CreateSchedule(ctx, api.CreateScheduleRequestObject{Body: createBody("lost")}); status(t, err) != http.StatusBadGateway {
		t.Fatalf("a lost reply: %v", err)
	}
	l.lose = false
	resp, err := a.CreateSchedule(ctx, api.CreateScheduleRequestObject{Body: createBody("lost")})
	if err != nil {
		t.Fatal(err)
	}
	if e.srv.ScheduleCount() != 1 || e.count(t, `SELECT count(*) FROM scheduled_run_schedules WHERE state = 'created' AND schedule_id = $1`,
		resp.(api.CreateSchedule201JSONResponse).Body.Id) != 1 {
		t.Fatal("the retry did not settle the intent on the schedule the first send made")
	}
}

func TestCreateReplayOfADeletedScheduleIsConflict(t *testing.T) {
	e := newEnv(t)
	// The identical body twice, so the conflict can only come from the tombstone, not from a changed request.
	resp, err := e.api.CreateSchedule(ctx, api.CreateScheduleRequestObject{Body: createBody("req-1")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.api.DeleteSchedule(ctx, api.DeleteScheduleRequestObject{ScheduleId: resp.(api.CreateSchedule201JSONResponse).Body.Id}); err != nil {
		t.Fatal(err)
	}
	_, err = e.api.CreateSchedule(ctx, api.CreateScheduleRequestObject{Body: createBody("req-1")})
	if status(t, err) != http.StatusConflict {
		t.Fatalf("replay of a deleted schedule: %v", err)
	}
	if e.srv.ScheduleCount() != 1 || e.srv.ScheduleCreates() != 2 ||
		e.count(t, `SELECT count(*) FROM scheduled_run_schedules WHERE deleted_at IS NOT NULL`) != 1 {
		t.Fatal("the replay did not reach kagent and keep the deletion recorded")
	}
}

// Two Mainloop instances (separate databases) on one kagent principal, each owning its own Agent.
// B must not list, read, change, trigger or adopt what A made, and cannot make a schedule on A's Agent.
func TestAnotherInstancesSchedulesAreInvisibleAndUntouchable(t *testing.T) {
	srv := kagenttest.New()
	t.Cleanup(srv.Close)
	srv.AddAgent("kagent", "prod-agent")
	srv.AddAgent("kagent", "dev-agent")
	a, b := newEnvOn(t, srv, "prod-agent"), newEnvOn(t, srv, "dev-agent")

	resp, err := a.api.CreateSchedule(ctx, api.CreateScheduleRequestObject{Body: &api.ScheduledRunCreate{
		Agent: api.ScheduledRunAgentRef{Namespace: "kagent", Name: "prod-agent"}, RequestId: "prod-req",
		Config: api.ScheduledRunConfigInput{Schedule: "0 9 * * *", Prompt: "prod report"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	sc := resp.(api.CreateSchedule201JSONResponse).Body
	x := fire(a, sc.Id)
	reserve(a, x.GetId(), "ks-prod", failedSt)

	ls, err := b.api.ListSchedules(ctx, api.ListSchedulesRequestObject{})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(ls.(api.ListSchedules200JSONResponse).Items); n != 0 {
		t.Fatalf("B lists %d schedule(s) it did not create", n)
	}
	if al, _ := a.api.ListSchedules(ctx, api.ListSchedulesRequestObject{}); len(al.(api.ListSchedules200JSONResponse).Items) != 1 {
		t.Fatal("A no longer lists its own schedule")
	}
	etag := `"` + sc.Etag + `"`
	body := api.ScheduledRunConfigInput{Schedule: "0 10 * * *", Prompt: "hijacked"}
	calls := map[string]func() error{
		"get": func() error {
			_, err := b.api.GetSchedule(ctx, api.GetScheduleRequestObject{ScheduleId: sc.Id})
			return err
		},
		"update": func() error {
			_, err := b.api.UpdateSchedule(ctx, api.UpdateScheduleRequestObject{
				ScheduleId: sc.Id, Body: &body, Params: api.UpdateScheduleParams{IfMatch: &etag}})
			return err
		},
		"delete": func() error {
			_, err := b.api.DeleteSchedule(ctx, api.DeleteScheduleRequestObject{ScheduleId: sc.Id})
			return err
		},
		"trigger": func() error {
			_, err := b.api.TriggerSchedule(ctx, api.TriggerScheduleRequestObject{
				ScheduleId: sc.Id, Body: &api.ScheduledRunTriggerRequest{RequestId: "b-run"}})
			return err
		},
		"executions": func() error {
			_, err := b.api.ListScheduleExecutions(ctx, api.ListScheduleExecutionsRequestObject{ScheduleId: sc.Id})
			return err
		},
	}
	for name, call := range calls {
		if s := status(t, call()); s != http.StatusNotFound {
			t.Errorf("B %s on A's schedule: %d, want 404", name, s)
		}
	}
	if xs, _, err := a.svc.Kagent.ListScheduledRunExecutionsPage(ctx, sc.Id, "", 10); err != nil || len(xs) != 1 {
		t.Fatalf("B triggered a run on A's schedule: %v %d", err, len(xs))
	}
	got, _ := srv.ScheduledRun(sc.Id)
	if got.GetDeletedAt() != nil || got.GetConfig().GetPrompt() != "prod report" {
		t.Fatal("B changed A's schedule")
	}
	_, err = b.api.CreateSchedule(ctx, api.CreateScheduleRequestObject{Body: &api.ScheduledRunCreate{
		Agent: api.ScheduledRunAgentRef{Namespace: "kagent", Name: "prod-agent"}, RequestId: "b-req",
		Config: api.ScheduledRunConfigInput{Schedule: "0 9 * * *", Prompt: "x"},
	}})
	if status(t, err) != http.StatusForbidden {
		t.Fatalf("B creating on A's Agent: %v", err)
	}

	if err := b.svc.Adopt(ctx); err != nil {
		t.Fatal(err)
	}
	if b.count(t, `SELECT count(*) FROM sessions`) != 0 || b.count(t, `SELECT count(*) FROM scheduled_run_links`) != 0 ||
		b.count(t, `SELECT count(*) FROM attention_items WHERE source_id LIKE 'sched_exec_%'`) != 0 {
		t.Fatal("B adopted A's execution")
	}
	if err := a.svc.Adopt(ctx); err != nil {
		t.Fatal(err)
	}
	if a.count(t, `SELECT count(*) FROM sessions`) != 1 {
		t.Fatal("A did not adopt its own execution")
	}
}

// kagent fails the whole list on one malformed schedule. The sweep does not read the list, so the
// schedules Mainloop made are still adopted.
func TestAFailedScheduleListDoesNotStopAdoption(t *testing.T) {
	e := newEnv(t)
	sc := create(t, e, "req-1")
	x := fire(e, sc.Id)
	reserve(e, x.GetId(), "ks-1", running)
	code := connect.CodeInternal
	e.srv.FailNext(kagent.ProcListScheduledRuns, kagenttest.Fault{GRPC: &code})
	if err := e.svc.Adopt(ctx); err != nil {
		t.Fatal(err)
	}
	if e.count(t, `SELECT count(*) FROM scheduled_run_links`) != 1 ||
		e.count(t, `SELECT count(*) FROM agent_bindings WHERE kagent_session_id = 'ks-1'`) != 1 {
		t.Fatal("a failed list stopped adoption")
	}
}

// hiding wraps the client and hides one execution from listings until released, which is what a
// reader sees while that execution's insert has not committed yet.
type hiding struct {
	schedules.Kagent
	hide string
}

func (h *hiding) ListScheduledRunExecutionsPage(
	c context.Context, scheduleID, token string, limit int,
) ([]*kagentapi.ScheduledRunExecution, string, error) {
	list, next, err := h.Kagent.ListScheduledRunExecutionsPage(c, scheduleID, token, limit)
	out := list[:0]
	for _, e := range list {
		if e.GetId() != h.hide {
			out = append(out, e)
		}
	}
	return out, next, err
}

// An execution whose id sorts below the cursor but becomes visible after the cursor moved past it
// is still found, because the cursor stays behind executions created within the safety window.
func TestCursorStaysBehindRecentExecutions(t *testing.T) {
	e := newEnv(t)
	sc := create(t, e, "req-1")
	early := fire(e, sc.Id) // lower UUIDv7
	late := fire(e, sc.Id)  // higher UUIDv7
	reserve(e, early.GetId(), "ks-early", failedSt)
	reserve(e, late.GetId(), "ks-late", failedSt)
	h := &hiding{Kagent: e.svc.Kagent, hide: early.GetId()}
	svc := &schedules.Service{Pool: e.pool, Kagent: h, OwnerID: owner} // default window
	if err := svc.Adopt(ctx); err != nil {                             // early is not visible yet
		t.Fatal(err)
	}
	h.hide = ""
	for range 3 {
		if err := svc.Adopt(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM scheduled_run_links WHERE execution_id = $1`, early.GetId()); n != 1 {
		t.Fatalf("the late-visible execution has %d links", n)
	}
	if n := e.count(t, `SELECT count(*) FROM sessions`); n != 2 {
		t.Fatalf("%d sessions", n)
	}
	if e.count(t, `SELECT count(*) FROM scheduled_run_schedules WHERE cursor_execution_id = ''`) != 1 {
		t.Fatal("the cursor moved past executions inside the window")
	}

	// Once they are older than the window the cursor does move.
	for _, x := range []*kagentapi.ScheduledRunExecution{early, late} {
		e.srv.EditExecution(x.GetId(), func(x *kagentapi.ScheduledRunExecution) {
			x.CreatedAt = timestamppb.New(time.Now().Add(-time.Hour))
		})
	}
	if err := svc.Adopt(ctx); err != nil {
		t.Fatal(err)
	}
	if e.count(t, `SELECT count(*) FROM scheduled_run_schedules WHERE cursor_execution_id = $1`, late.GetId()) != 1 {
		t.Fatal("the cursor did not advance over settled executions")
	}
}

// The link keeps the session id after the session is deleted, and the session is never made again.
func TestDeletedSessionIsNotRecreated(t *testing.T) {
	e := newEnv(t)
	sc := create(t, e, "req-1")
	x := fire(e, sc.Id)
	reserve(e, x.GetId(), "ks-1", running)
	if err := e.svc.Adopt(ctx); err != nil {
		t.Fatal(err)
	}
	sid, _ := e.sessionOf(t, x.GetId())
	if _, err := e.pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, sid); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := e.svc.Adopt(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if e.count(t, `SELECT count(*) FROM sessions`) != 0 {
		t.Fatal("a deleted session was made again")
	}
	lr, err := e.api.ListScheduleExecutions(ctx, api.ListScheduleExecutionsRequestObject{ScheduleId: sc.Id})
	if err != nil {
		t.Fatal(err)
	}
	items := lr.(api.ListScheduleExecutions200JSONResponse).Items
	if len(items) != 1 || items[0].SessionId == nil || *items[0].SessionId != sid {
		t.Fatalf("the execution lost its session id: %+v", items)
	}
}

// repeating answers every execution page with the same next token.
type repeating struct{ schedules.Kagent }

func (r repeating) ListScheduledRunExecutionsPage(
	c context.Context, scheduleID, token string, limit int,
) ([]*kagentapi.ScheduledRunExecution, string, error) {
	list, _, err := r.Kagent.ListScheduledRunExecutionsPage(c, scheduleID, token, limit)
	return list, "same-token", err
}

func TestARepeatedExecutionPageTokenEndsThePass(t *testing.T) {
	e := newEnv(t)
	sc := create(t, e, "req-1")
	fire(e, sc.Id)
	svc := &schedules.Service{Pool: e.pool, Kagent: repeating{e.svc.Kagent}, OwnerID: owner, Limits: e.svc.Limits}
	done := make(chan error, 1)
	go func() { done <- svc.Adopt(ctx) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "repeated") {
			t.Fatalf("want a repeated-token error, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the pass followed a repeated page token for good")
	}
}

// A create that fails before kagent accepts it leaves an intent. If the Agent then leaves this
// deployment and another instance takes it, the sweep must not send the create: a replay is a fresh
// create on an Agent that is no longer ours.
func TestRecoveryDoesNotCreateOnAnAgentThatLeftTheDeployment(t *testing.T) {
	e := newEnv(t)
	code := connect.CodeUnavailable
	e.srv.FailNext(kagent.ProcCreateScheduledRun, kagenttest.Fault{GRPC: &code})
	_, err := e.api.CreateSchedule(ctx, api.CreateScheduleRequestObject{Body: createBody("ownership-change")})
	if status(t, err) != http.StatusBadGateway || e.srv.ScheduleCount() != 0 {
		t.Fatalf("setup: want an unaccepted create with a durable intent; err=%v schedules=%d", err, e.srv.ScheduleCount())
	}
	e.expireLeases(t)
	if _, err := e.pool.Exec(ctx, `DELETE FROM kagent_agents WHERE namespace = 'kagent' AND name = 'claude'`); err != nil {
		t.Fatal(err)
	}
	// A second database now owns the relinquished Agent, on the same kagent principal.
	other := newEnvOn(t, e.srv, "claude")
	create(t, other, "new-instance")
	before := e.srv.ScheduleCount()
	_, err = e.api.CreateSchedule(ctx, api.CreateScheduleRequestObject{Body: createBody("ownership-change")})
	if status(t, err) != http.StatusForbidden {
		t.Fatalf("create after ownership ended: %v", err)
	}
	if err := e.svc.Adopt(ctx); err != nil {
		t.Fatal(err)
	}
	if e.srv.ScheduleCount() != before {
		t.Fatalf("recovery sent a create on an Agent outside the owned set: schedules %d -> %d", before, e.srv.ScheduleCount())
	}
	if e.count(t, `SELECT count(*) FROM scheduled_run_schedules WHERE state = 'created'`) != 0 {
		t.Fatal("recovery registered a schedule on an Agent outside the owned set")
	}
	// The intent waits for the Agent to return; the Agent coming back lets the sweep finish it.
	if e.count(t, `SELECT count(*) FROM scheduled_run_schedules WHERE state = 'intent'`) != 1 {
		t.Fatal("the intent of an unowned Agent was dropped")
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO kagent_agents (namespace, name) VALUES ('kagent', 'claude')`); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Adopt(ctx); err != nil {
		t.Fatal(err)
	}
	if e.count(t, `SELECT count(*) FROM scheduled_run_schedules WHERE state = 'created'`) != 1 {
		t.Fatal("the sweep did not finish the intent once its Agent was owned again")
	}
}

// concurrentCreateReplies scripts two overlapping creates of one request id: the first reaches
// kagent and commits but its reply waits; the second is refused. The fork can do this because it
// reads the request key, checks the Agent and inserts as separate steps, so the Agent can vanish
// between the targets checks of two concurrent calls.
type concurrentCreateReplies struct {
	schedules.Kagent
	n                                                          atomic.Int32
	firstStarted, firstMaySend, firstCommitted, firstMayReturn chan struct{}
}

func (r *concurrentCreateReplies) CreateScheduledRun(c context.Context, req *kagentapi.CreateScheduledRunRequest) (*kagentapi.ScheduledRun, error) {
	switch r.n.Add(1) {
	case 1:
		close(r.firstStarted)
		select {
		case <-r.firstMaySend:
		case <-c.Done():
			return nil, c.Err()
		}
		sc, err := r.Kagent.CreateScheduledRun(c, req)
		close(r.firstCommitted)
		select {
		case <-r.firstMayReturn:
		case <-c.Done():
			return nil, c.Err()
		}
		return sc, err
	case 2:
		close(r.firstMaySend)
		select {
		case <-r.firstCommitted:
		case <-c.Done():
			return nil, c.Err()
		}
		return nil, &kagent.SessionError{Code: connect.CodeFailedPrecondition, Detail: "Agent disappeared between concurrent target checks"}
	default:
		return r.Kagent.CreateScheduledRun(c, req)
	}
}

func TestConcurrentRefusalKeepsSuccessfulCreateRegistered(t *testing.T) {
	e := newEnv(t)
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	r := &concurrentCreateReplies{Kagent: e.svc.Kagent, firstStarted: make(chan struct{}), firstMaySend: make(chan struct{}),
		firstCommitted: make(chan struct{}), firstMayReturn: make(chan struct{})}
	e.svc.Kagent = r
	type result struct {
		resp api.CreateScheduleResponseObject
		err  error
	}
	first := make(chan result, 1)
	go func() {
		resp, err := e.api.CreateSchedule(c, api.CreateScheduleRequestObject{Body: createBody("same-request")})
		first <- result{resp, err}
	}()
	select {
	case <-r.firstStarted:
	case <-c.Done():
		t.Fatal(c.Err())
	}
	if _, err := e.api.CreateSchedule(c, api.CreateScheduleRequestObject{Body: createBody("same-request")}); status(t, err) != http.StatusNotFound {
		t.Fatalf("second reply: %v", err)
	}
	close(r.firstMayReturn)
	var got result
	select {
	case got = <-first:
	case <-c.Done():
		t.Fatal(c.Err())
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	id := got.resp.(api.CreateSchedule201JSONResponse).Body.Id
	if e.count(t, `SELECT count(*) FROM scheduled_run_schedules WHERE schedule_id = $1 AND state = 'created'`, id) != 1 {
		t.Fatal("a concurrent refusal erased the successful create's registry row")
	}
	if _, err := e.api.GetSchedule(c, api.GetScheduleRequestObject{ScheduleId: id}); err != nil {
		t.Fatalf("the created schedule is not readable: %v", err)
	}
	if e.scheduleEvents(t) != 1 {
		t.Fatalf("schedule:updated events = %d, want 1", e.scheduleEvents(t))
	}
	x := fire(e, id)
	reserve(e, x.GetId(), "ks-concurrent", running)
	if err := e.svc.Adopt(c); err != nil {
		t.Fatal(err)
	}
	if e.count(t, `SELECT count(*) FROM scheduled_run_links`) != 1 {
		t.Fatal("the created schedule was not adopted")
	}
}

// A success whose registry row is gone is an error the client retries, never a silent 201.
func TestConfirmWithoutAnIntentIsAnErrorAndTheRetryHeals(t *testing.T) {
	e := newEnv(t)
	d := &deleteIntentBeforeReply{Kagent: e.svc.Kagent, pool: e.pool}
	e.svc.Kagent = d
	if _, err := e.api.CreateSchedule(ctx, api.CreateScheduleRequestObject{Body: createBody("vanished")}); err == nil {
		t.Fatal("create answered success while the registry has no row for it")
	}
	d.off = true
	resp, err := e.api.CreateSchedule(ctx, api.CreateScheduleRequestObject{Body: createBody("vanished")})
	if err != nil {
		t.Fatal(err)
	}
	id := resp.(api.CreateSchedule201JSONResponse).Body.Id
	if e.srv.ScheduleCount() != 1 || e.count(t, `SELECT count(*) FROM scheduled_run_schedules WHERE state = 'created' AND schedule_id = $1`, id) != 1 {
		t.Fatal("the retry did not register the schedule the first send made")
	}
}

type deleteIntentBeforeReply struct {
	schedules.Kagent
	pool *pgxpool.Pool
	off  bool
}

func (d *deleteIntentBeforeReply) CreateScheduledRun(c context.Context, req *kagentapi.CreateScheduledRunRequest) (*kagentapi.ScheduledRun, error) {
	sc, err := d.Kagent.CreateScheduledRun(c, req)
	if !d.off {
		if _, derr := d.pool.Exec(c, `DELETE FROM scheduled_run_schedules WHERE request_id = $1`, req.GetRequestId()); derr != nil {
			return nil, derr
		}
	}
	return sc, err
}

// slowSuccess starts the first of two overlapping creates of one request id (see
// concurrentCreateReplies): it reaches kagent and commits, but its reply is held back.
type slowSuccess struct {
	r     *concurrentCreateReplies
	first chan struct {
		resp api.CreateScheduleResponseObject
		err  error
	}
}

func startSlowSuccess(t *testing.T, c context.Context, e *env, request string) *slowSuccess {
	t.Helper()
	r := &concurrentCreateReplies{Kagent: e.svc.Kagent, firstStarted: make(chan struct{}), firstMaySend: make(chan struct{}),
		firstCommitted: make(chan struct{}), firstMayReturn: make(chan struct{})}
	e.svc.Kagent = r
	s := &slowSuccess{r: r, first: make(chan struct {
		resp api.CreateScheduleResponseObject
		err  error
	}, 1)}
	go func() {
		resp, err := e.api.CreateSchedule(c, api.CreateScheduleRequestObject{Body: createBody(request)})
		s.first <- struct {
			resp api.CreateScheduleResponseObject
			err  error
		}{resp, err}
	}()
	select {
	case <-r.firstStarted:
	case <-c.Done():
		t.Fatal(c.Err())
	}
	// The attempt stays in flight for a while, as a slow success does.
	time.Sleep(150 * time.Millisecond)
	return s
}

// finish lets the held-back success commit and return, and checks it was registered and adopted.
func (s *slowSuccess) finish(t *testing.T, c context.Context, e *env) {
	t.Helper()
	select {
	case <-s.r.firstMaySend:
	default:
		close(s.r.firstMaySend)
	}
	close(s.r.firstMayReturn)
	var got struct {
		resp api.CreateScheduleResponseObject
		err  error
	}
	select {
	case got = <-s.first:
	case <-c.Done():
		t.Fatal(c.Err())
	}
	if got.err != nil {
		t.Fatalf("the slow success failed: %v", got.err)
	}
	id := got.resp.(api.CreateSchedule201JSONResponse).Body.Id
	if e.count(t, `SELECT count(*) FROM scheduled_run_schedules WHERE schedule_id = $1 AND state = 'created'`, id) != 1 ||
		e.count(t, `SELECT count(*) FROM scheduled_run_create_attempts`) != 0 {
		t.Fatal("the slow success was not registered, or left a lease")
	}
	if _, err := e.api.GetSchedule(c, api.GetScheduleRequestObject{ScheduleId: id}); err != nil {
		t.Fatalf("the created schedule is not readable: %v", err)
	}
	if e.scheduleEvents(t) != 1 {
		t.Fatalf("schedule:updated events = %d, want 1", e.scheduleEvents(t))
	}
	x := fire(e, id)
	reserve(e, x.GetId(), "ks-slow-success", running)
	if err := e.svc.Adopt(c); err != nil {
		t.Fatal(err)
	}
	if e.count(t, `SELECT count(*) FROM scheduled_run_links`) != 1 {
		t.Fatal("the created schedule was not adopted")
	}
}

// An attempt that is slow but succeeds is not undone by a refusal from a route retry, however long
// the first has been running: the refusal ends only its own lease.
func TestSlowSuccessSurvivesARouteRefusal(t *testing.T) {
	e := newEnv(t)
	c, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	s := startSlowSuccess(t, c, e, "slow-route")
	if _, err := e.api.CreateSchedule(c, api.CreateScheduleRequestObject{Body: createBody("slow-route")}); status(t, err) != http.StatusNotFound {
		t.Fatalf("route refusal: %v", err)
	}
	if e.count(t, `SELECT count(*) FROM scheduled_run_schedules WHERE state = 'intent'`) != 1 ||
		e.count(t, `SELECT count(*) FROM scheduled_run_create_attempts`) != 1 {
		t.Fatal("a route refusal must keep the intent and end only its own lease")
	}
	s.finish(t, c, e)
}

// While an attempt holds an unexpired lease the sweep does not replay the intent at all, so it
// cannot refuse it either.
func TestSlowSuccessSurvivesTheRecoverySweep(t *testing.T) {
	e := newEnv(t)
	c, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	s := startSlowSuccess(t, c, e, "slow-recovery")
	if err := e.svc.Adopt(c); err != nil {
		t.Fatal(err)
	}
	if n := s.r.n.Load(); n != 1 {
		t.Fatalf("the sweep sent a create while an attempt's lease was live: %d creates", n)
	}
	if e.count(t, `SELECT count(*) FROM scheduled_run_schedules WHERE state = 'intent'`) != 1 {
		t.Fatal("the sweep dropped an intent with a live lease")
	}
	s.finish(t, c, e)
}

// refuseAfterLease is kagent refusing the sweep's replay while another attempt takes a lease.
type refuseAfterLease struct {
	schedules.Kagent
	pool *pgxpool.Pool
}

func (r *refuseAfterLease) CreateScheduledRun(c context.Context, req *kagentapi.CreateScheduledRunRequest) (*kagentapi.ScheduledRun, error) {
	if _, err := r.pool.Exec(c, `INSERT INTO scheduled_run_create_attempts (request_id, expires_at)
		VALUES ($1, now() + interval '1 minute')`, req.GetRequestId()); err != nil {
		return nil, err
	}
	return nil, &kagent.SessionError{Code: connect.CodeFailedPrecondition, Detail: "refused"}
}

// The recovery's refusal is final only when no other lease is live.
func TestRecoveryRefusalKeepsAnIntentWhileAnotherLeaseIsLive(t *testing.T) {
	e := newEnv(t)
	unavailable := connect.CodeUnavailable
	e.srv.FailNext(kagent.ProcCreateScheduledRun, kagenttest.Fault{GRPC: &unavailable})
	if _, err := e.api.CreateSchedule(ctx, api.CreateScheduleRequestObject{Body: createBody("contested")}); status(t, err) != http.StatusBadGateway {
		t.Fatalf("setup: %v", err)
	}
	e.expireLeases(t)
	e.svc.Kagent = &refuseAfterLease{Kagent: e.svc.Kagent, pool: e.pool}
	if err := e.svc.Adopt(ctx); err != nil {
		t.Fatal(err)
	}
	if e.count(t, `SELECT count(*) FROM scheduled_run_schedules WHERE state = 'intent'`) != 1 {
		t.Fatal("a refusal deleted an intent while another attempt's lease was live")
	}
	if e.count(t, `SELECT count(*) FROM scheduled_run_create_attempts`) != 1 {
		t.Fatal("the recovery's own lease should have ended, the other one stayed")
	}
}

// abandoned hangs a create until its context ends, as a kagent that never answers does.
type abandoned struct {
	schedules.Kagent
	hang     atomic.Bool
	calls    atomic.Int32
	deadline atomic.Pointer[time.Time]
}

func (a *abandoned) CreateScheduledRun(c context.Context, req *kagentapi.CreateScheduledRunRequest) (*kagentapi.ScheduledRun, error) {
	a.calls.Add(1)
	if !a.hang.Load() {
		return a.Kagent.CreateScheduledRun(c, req)
	}
	d, ok := c.Deadline()
	if !ok {
		return nil, errors.New("the create has no deadline")
	}
	a.deadline.Store(&d)
	<-c.Done()
	return nil, &kagent.OutcomeUnknown{Op: "CreateScheduledRun", Detail: "no answer", Err: c.Err()}
}

// An attempt cannot outlive its lease: its call is cut off a margin before the lease expires, and
// the sweep replays the intent only after that, finding the schedule through kagent's dedupe.
func TestAbandonedAttemptsLeaseExpiresThenRecoveryReplays(t *testing.T) {
	e := newEnv(t)
	e.svc.Limits = schedules.Limits{CursorSafetyWindow: -1, CreateAttemptBound: 2 * time.Second, CreateAttemptMargin: 400 * time.Millisecond}
	a := &abandoned{Kagent: e.svc.Kagent}
	a.hang.Store(true)
	e.svc.Kagent = a

	begun := time.Now()
	_, err := e.api.CreateSchedule(ctx, api.CreateScheduleRequestObject{Body: createBody("abandoned")})
	if status(t, err) != http.StatusBadGateway {
		t.Fatalf("abandoned create: %v", err)
	}
	var expires time.Time
	if err := e.pool.QueryRow(ctx, `SELECT expires_at FROM scheduled_run_create_attempts`).Scan(&expires); err != nil {
		t.Fatalf("an uncertain attempt must leave its lease to expire: %v", err)
	}
	d := *a.deadline.Load()
	if gap := expires.Sub(d); gap < 400*time.Millisecond {
		t.Fatalf("the call's deadline is %v before the lease expires, want at least the 400ms margin", gap)
	}
	if returned := time.Since(begun); returned >= 2*time.Second-400*time.Millisecond+300*time.Millisecond {
		t.Fatalf("the call outlived its deadline: returned after %v", returned)
	}

	// The lease is still live: the sweep does not replay.
	a.hang.Store(false)
	if err := e.svc.Adopt(ctx); err != nil {
		t.Fatal(err)
	}
	if a.calls.Load() != 1 || e.srv.ScheduleCount() != 0 {
		t.Fatalf("the sweep replayed under a live lease: %d calls", a.calls.Load())
	}
	// Once it has expired the replay goes out and confirms.
	for deadline := time.Now().Add(5 * time.Second); e.count(t, `SELECT count(*) FROM scheduled_run_create_attempts WHERE expires_at > now()`) > 0; {
		if time.Now().After(deadline) {
			t.Fatal("the lease did not expire")
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err := e.svc.Adopt(ctx); err != nil {
		t.Fatal(err)
	}
	if a.calls.Load() != 2 || e.srv.ScheduleCount() != 1 ||
		e.count(t, `SELECT count(*) FROM scheduled_run_schedules WHERE state = 'created'`) != 1 ||
		e.count(t, `SELECT count(*) FROM scheduled_run_create_attempts`) != 0 {
		t.Fatalf("the replay after expiry did not confirm: %d calls", a.calls.Load())
	}
}
