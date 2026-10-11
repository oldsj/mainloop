package hitl_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/hitl"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

const (
	owner   = "owner-1"
	session = "sess-1"
	ksid    = "ks-1"
)

var agent = kagent.AgentRef{Namespace: "kagent", Name: "mainloop-main"}

func quiet() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// fakeKagent is an in-memory kagent: tasks per context, scripted failures, and a record of sends.
type fakeKagent struct {
	mu      sync.Mutex
	tasks   map[string]*a2a.Task
	listErr error
	getErr  error
	findErr error
	sendErr error
	// onSend runs inside SendHITL after the send is recorded. Nil moves the task out of
	// input-required and adds the message to its history, like a kagent that accepts.
	onSend func(f *fakeKagent, s hitl.Send) error
	sends  []hitl.Send
	finds  int // FindTaskForMessage calls
}

func newFake() *fakeKagent { return &fakeKagent{tasks: map[string]*a2a.Task{}} }

func (f *fakeKagent) put(t *a2a.Task) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tasks[string(t.ID)] = t
}

func (f *fakeKagent) task(id string) *a2a.Task {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tasks[id]
}

func (f *fakeKagent) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func (f *fakeKagent) sendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sends)
}

func (f *fakeKagent) ListTasksPage(_ context.Context, _ kagent.AgentRef, contextID, token string, limit int) ([]*a2a.Task, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, "", f.listErr
	}
	var all []*a2a.Task
	for _, t := range f.tasks {
		if t.ContextID == contextID {
			all = append(all, clone(t))
		}
	}
	slices.SortFunc(all, func(a, b *a2a.Task) int {
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		}
		return 0
	})
	start := 0
	if token != "" {
		n, err := strconv.Atoi(token)
		if err != nil {
			return nil, "", &kagent.ProtocolError{Op: "list", Status: 400}
		}
		start = n
	}
	end := min(start+limit, len(all))
	next := ""
	if end < len(all) {
		next = strconv.Itoa(end)
	}
	return all[min(start, len(all)):end], next, nil
}

func (f *fakeKagent) GetTask(_ context.Context, _ kagent.AgentRef, taskID string) (*a2a.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	t, ok := f.tasks[taskID]
	if !ok {
		return nil, &kagent.TaskNotFound{}
	}
	return clone(t), nil
}

func (f *fakeKagent) FindTaskForMessage(_ context.Context, _ kagent.AgentRef, contextID, messageID string) (*a2a.Task, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finds++
	if f.findErr != nil {
		return nil, false, f.findErr
	}
	for _, t := range f.tasks {
		if t.ContextID != contextID {
			continue
		}
		for _, m := range t.History {
			if m.ID == messageID {
				return clone(t), true, nil
			}
		}
	}
	return nil, false, nil
}

func (f *fakeKagent) SendHITL(_ context.Context, _ kagent.AgentRef, s hitl.Send) error {
	f.mu.Lock()
	if f.sendErr != nil {
		err := f.sendErr
		f.mu.Unlock()
		return err
	}
	f.sends = append(f.sends, s)
	f.mu.Unlock()
	if f.onSend != nil {
		return f.onSend(f, s)
	}
	f.accept(s)
	return nil
}

// accept does what kagent does with a response: the task resumes and its history shows the message.
func (f *fakeKagent) accept(s hitl.Send) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.tasks[s.TaskID]
	t.History = append(t.History, &a2a.Message{ID: s.MessageID, Role: a2a.MessageRoleUser})
	t.Status = a2a.TaskStatus{State: a2a.TaskStateWorking}
}

func statusMessage(id string, request map[string]any) *a2a.Message {
	return &a2a.Message{
		ID: id, Role: a2a.MessageRoleAgent, Extensions: []string{kagent.HITLExtension},
		Metadata: map[string]any{kagent.HITLExtension: request},
	}
}

func toolRequest(toolIDs ...string) map[string]any {
	tools := []any{}
	for _, id := range toolIDs {
		tools = append(tools, map[string]any{
			"id": id, "call_id": "call-" + id, "name": "run_" + id, "args": map[string]any{"cmd": "ls " + id},
		})
	}
	return map[string]any{"type": "tool_approval_request", "hint": "needs a human", "tools": tools}
}

func askRequest(id string) map[string]any {
	return map[string]any{
		"type": "ask_user_request", "id": id,
		"questions": []any{
			map[string]any{"question": "Which branch?", "choices": []any{"main", "dev"}, "multiple": false},
		},
	}
}

func waitingTask(taskID, msgID string, request map[string]any) *a2a.Task {
	return &a2a.Task{
		ID: a2a.TaskID(taskID), ContextID: ksid,
		Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired, Message: statusMessage(msgID, request)},
	}
}

type env struct {
	t    *testing.T
	pool *pgxpool.Pool
	k    *fakeKagent
	svc  *hitl.Service
	api  *hitl.API
}

func newEnv(t *testing.T, mutate ...func(*hitl.Service)) *env {
	t.Helper()
	pool := testdb.New(t).Pool
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO sessions (id, user_id, title, description, prompt) VALUES ($1, $2, 'Fix the build', 'd', 'p')`,
		session, owner); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO agent_bindings (session_id, kind, role, kagent_session_id) VALUES ($1, 'claude', 'main', $2)`,
		session, ksid); err != nil {
		t.Fatal(err)
	}
	k := newFake()
	svc := &hitl.Service{
		Pool: pool, Kagent: k, OwnerID: owner, Agent: agent, WritesEnabled: true, Log: quiet(),
		Opts: hitl.Options{
			RefreshEvery: time.Millisecond, ScanEvery: time.Millisecond, StaleAfter: time.Hour,
			UncertainRecheck: 20 * time.Millisecond, UncertainHold: time.Hour, StaleRetry: 20 * time.Millisecond,
			Lease: 600 * time.Millisecond, Concurrency: 4,
		},
	}
	for _, m := range mutate {
		m(svc)
	}
	return &env{t: t, pool: pool, k: k, svc: svc, api: hitl.NewAPI(svc)}
}

func (e *env) observe() {
	e.t.Helper()
	time.Sleep(5 * time.Millisecond) // let scan and refresh intervals elapse
	if err := e.svc.Observe(context.Background()); err != nil {
		e.t.Fatalf("observe: %v", err)
	}
}

// run starts the response loop until the test ends.
func (e *env) run() {
	e.t.Helper()
	r := reconcile.NewRunner(e.pool, quiet(), nil)
	l := e.svc.ResponseLoop()
	l.Interval = 20 * time.Millisecond
	r.Add(l)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { _ = r.Run(ctx) })
	e.t.Cleanup(func() { cancel(); wg.Wait() })
}

func (e *env) requests() []dbq.HitlRequest {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT id FROM hitl_requests ORDER BY task_id, created_at`)
	if err != nil {
		e.t.Fatal(err)
	}
	ids, err := pgxCollect(rows)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []dbq.HitlRequest
	for _, id := range ids {
		r, err := dbq.New(e.pool).GetHITLRequest(context.Background(), dbq.GetHITLRequestParams{ID: id, OwnerID: owner})
		if err != nil {
			e.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func pgxCollect(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close()
}) ([]string, error) {
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) requestFor(taskID string) dbq.HitlRequest {
	e.t.Helper()
	var found []dbq.HitlRequest
	for _, r := range e.requests() {
		if r.TaskID == taskID {
			found = append(found, r)
		}
	}
	if len(found) == 0 {
		e.t.Fatalf("no request for task %s", taskID)
	}
	return found[len(found)-1]
}

func (e *env) response(id string) dbq.HitlResponse {
	e.t.Helper()
	r, err := dbq.New(e.pool).GetHITLResponse(context.Background(), dbq.GetHITLResponseParams{ID: id, OwnerID: owner})
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) waitResponse(id string, want dbstate.HITLResponseState) dbq.HitlResponse {
	e.t.Helper()
	var r dbq.HitlResponse
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		r = e.response(id)
		if r.State == want && r.LeaseOwner == nil {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("response %s is %s (detail %q), want %s", id, r.State, deref(r.Detail), want)
	return r
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func approveAll(ids ...string) api.HITLDecisionInput {
	approvals := []api.ToolApproval{}
	for _, id := range ids {
		approvals = append(approvals, api.ToolApproval{Id: id, Approved: true})
	}
	return decision("act-1", api.ToolApprovalResponse{Type: api.ToolApprovalResponseTypeToolApprovalResponse, Approvals: approvals})
}

func decision(actionID string, resp any) api.HITLDecisionInput {
	in := api.HITLDecisionInput{ActionId: actionID}
	var err error
	switch r := resp.(type) {
	case api.ToolApprovalResponse:
		err = in.Response.FromToolApprovalResponse(r)
	case api.AskUserResponse:
		err = in.Response.FromAskUserResponse(r)
	}
	if err != nil {
		panic(err)
	}
	return in
}

func answer(id string, choice string) api.AskUserResponse {
	return api.AskUserResponse{
		Type: api.AskUserResponseTypeAskUserResponse, Id: id,
		Answers: []api.AskUserAnswer{{Answer: []string{choice}}},
	}
}

func receiptJSON(t *testing.T, v api.HITLView) string {
	t.Helper()
	if v.Response == nil {
		t.Fatal("view has no response receipt")
	}
	b, err := json.Marshal(v.Response)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestObserverAdoptsToolApprovalAndQuestion(t *testing.T) {
	e := newEnv(t)
	e.k.put(waitingTask("task-a", "msg-a", toolRequest("t1", "t2")))
	e.k.put(waitingTask("task-b", "msg-b", askRequest("q1")))
	// A task that is not waiting, and one that waits for something that is not a HITL ask, are ignored.
	e.k.put(&a2a.Task{ID: "task-c", ContextID: ksid, Status: a2a.TaskStatus{State: a2a.TaskStateWorking}})
	e.k.put(&a2a.Task{ID: "task-d", ContextID: ksid, Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired, Message: &a2a.Message{ID: "m"}}})

	e.observe()

	reqs := e.requests()
	if len(reqs) != 2 {
		t.Fatalf("got %d requests, want 2", len(reqs))
	}
	a, b := e.requestFor("task-a"), e.requestFor("task-b")
	if a.Kind != dbstate.HITLToolApproval || b.Kind != dbstate.HITLAskUser {
		t.Fatalf("kinds = %s, %s", a.Kind, b.Kind)
	}
	for _, r := range reqs {
		if r.Availability != dbstate.HITLPending || r.SessionID != session || r.KagentSessionID != ksid {
			t.Fatalf("request = %+v", r)
		}
	}
	if n := e.count(`SELECT count(*) FROM attention_items WHERE state = 'open' AND source_kind = 'hitl_request' AND session_id = $1`, session); n != 2 {
		t.Fatalf("open attention rows = %d, want 2", n)
	}
	var kinds []string
	rows, err := e.pool.Query(context.Background(), `SELECT kind FROM attention_items ORDER BY kind`)
	if err != nil {
		t.Fatal(err)
	}
	kinds, err = pgxCollect(rows)
	if err != nil || !slices.Equal(kinds, []string{"approval", "question"}) {
		t.Fatalf("attention kinds = %v, %v", kinds, err)
	}
	events := e.count(`SELECT count(*) FROM events WHERE user_id = $1 AND type = 'inbox:updated'`, owner)
	if events < 2 {
		t.Fatalf("inbox:updated events = %d, want at least 2", events)
	}
	var lastCount int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT (payload->>'unread_count')::int FROM events WHERE type = 'inbox:updated' ORDER BY id DESC LIMIT 1`).Scan(&lastCount); err != nil || lastCount != 2 {
		t.Fatalf("last unread_count = %d, %v", lastCount, err)
	}

	// The same ask seen again is not a new request and publishes nothing.
	e.observe()
	if n := len(e.requests()); n != 2 {
		t.Fatalf("requests after second pass = %d", n)
	}
	if n := e.count(`SELECT count(*) FROM events WHERE type = 'inbox:updated'`); n != events {
		t.Fatalf("events grew from %d to %d on a no-op pass", events, n)
	}

	view, err := e.svc.View(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Answerable || !view.WritesEnabled || view.Request.Availability != api.HITLProjectionAvailabilityPending {
		t.Fatalf("view = %+v", view)
	}
	if view.Context.Title == nil || *view.Context.Title != "Fix the build" {
		t.Fatalf("context = %+v", view.Context)
	}
	ids, err := e.svc.SessionRequests(context.Background(), session)
	if err != nil || len(ids) != 2 {
		t.Fatalf("session requests = %v, %v", ids, err)
	}
	if _, err := e.svc.SessionRequests(context.Background(), "nope"); !errors.Is(err, hitl.ErrNotFound) {
		t.Fatalf("unknown session: %v", err)
	}
	st, err := e.svc.Status(context.Background())
	if err != nil || st.PendingTaskCount != 2 || st.OldestTaskCheck == nil {
		t.Fatalf("status = %+v, %v", st, err)
	}
}

func TestRespondSendsOnceAndDuplicateReturnsSameReceipt(t *testing.T) {
	e := newEnv(t)
	e.k.put(waitingTask("task-a", "msg-a", toolRequest("t1", "t2")))
	e.observe()
	req := e.requestFor("task-a")
	e.run()

	ctx := context.Background()
	first, err := e.svc.Respond(ctx, req.ID, approveAll("t1", "t2"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Response == nil || first.Answerable {
		t.Fatalf("view after respond = %+v", first)
	}
	if len(first.Response.Calls) != 2 || first.Response.Calls[0].Approved == nil || !*first.Response.Calls[0].Approved {
		t.Fatalf("receipt calls = %+v", first.Response.Calls)
	}
	e.waitResponse("act-1", dbstate.HITLAccepted)
	if n := e.k.sendCount(); n != 1 {
		t.Fatalf("sends = %d, want 1", n)
	}
	sent := e.k.sends[0]
	if sent.MessageID != first.Response.OutboundMessageId || sent.TaskID != "task-a" || sent.ContextID != ksid {
		t.Fatalf("send = %+v, receipt = %+v", sent, first.Response)
	}
	if _, ok := sent.Native["reviewed_context"]; ok {
		t.Fatalf("native response carries reviewed_context: %v", sent.Native)
	}

	dup, err := e.svc.Respond(ctx, req.ID, approveAll("t1", "t2"))
	if err != nil {
		t.Fatal(err)
	}
	if receiptJSON(t, dup) != receiptJSON(t, first) {
		t.Fatalf("duplicate receipt differs:\n%s\n%s", receiptJSON(t, dup), receiptJSON(t, first))
	}
	time.Sleep(100 * time.Millisecond)
	if n := e.k.sendCount(); n != 1 {
		t.Fatalf("sends after duplicate = %d, want 1", n)
	}
	if n := e.count(`SELECT count(*) FROM hitl_responses`); n != 1 {
		t.Fatalf("responses = %d, want 1", n)
	}

	// The request is answered once the next pass reads the resumed task, or at once when accepted.
	e.observe()
	if got := e.requestFor("task-a").Availability; got != dbstate.HITLAnswered {
		t.Fatalf("request availability = %s, want answered", got)
	}
	if n := e.count(`SELECT count(*) FROM attention_items WHERE state = 'open'`); n != 0 {
		t.Fatalf("open attention rows after answer = %d", n)
	}
	// A repeat after the request is final still reads back the receipt.
	again, err := e.svc.Respond(ctx, req.ID, approveAll("t1", "t2"))
	if err != nil || receiptJSON(t, again) != receiptJSON(t, first) {
		t.Fatalf("repeat after answer: %v", err)
	}
	if again.TransportState == nil || *again.TransportState != "accepted" {
		t.Fatalf("transport state = %v", again.TransportState)
	}
}

func TestRespondAnswersQuestion(t *testing.T) {
	e := newEnv(t)
	e.k.put(waitingTask("task-b", "msg-b", askRequest("q1")))
	e.observe()
	req := e.requestFor("task-b")
	e.run()
	if _, err := e.svc.Respond(context.Background(), req.ID, decision("act-q", answer("q1", "dev"))); err != nil {
		t.Fatal(err)
	}
	e.waitResponse("act-q", dbstate.HITLAccepted)
	native := e.k.sends[0].Native
	if native["type"] != "ask_user_response" || native["id"] != "q1" {
		t.Fatalf("native = %v", native)
	}
}

func TestSendTimeoutIsUncertainAndNeverResent(t *testing.T) {
	e := newEnv(t)
	e.k.onSend = func(*fakeKagent, hitl.Send) error { return context.DeadlineExceeded }
	e.k.put(waitingTask("task-a", "msg-a", toolRequest("t1")))
	e.observe()
	req := e.requestFor("task-a")
	e.run()

	if _, err := e.svc.Respond(context.Background(), req.ID, approveAll("t1")); err != nil {
		t.Fatal(err)
	}
	r := e.waitResponse("act-1", dbstate.HITLUncertain)
	if r.Detail == nil {
		t.Fatal("uncertain response has no detail")
	}
	// Several rechecks pass; kagent never shows the message, so the row stays uncertain and nothing
	// is sent again.
	time.Sleep(250 * time.Millisecond)
	if n := e.k.sendCount(); n != 1 {
		t.Fatalf("sends = %d, want exactly 1", n)
	}
	if got := e.response("act-1").State; got != dbstate.HITLUncertain {
		t.Fatalf("state = %s, want uncertain", got)
	}
	view, err := e.svc.View(context.Background(), req.ID)
	if err != nil || view.Answerable || view.TransportState == nil || *view.TransportState != "uncertain" {
		t.Fatalf("view = %+v, %v", view, err)
	}
	// Another answer is refused while the first one is unresolved.
	_, err = e.svc.Respond(context.Background(), req.ID, decision("act-2", api.ToolApprovalResponse{
		Type: api.ToolApprovalResponseTypeToolApprovalResponse, Approvals: []api.ToolApproval{{Id: "t1", Approved: false}},
	}))
	if !errors.Is(err, hitl.ErrConflict) {
		t.Fatalf("second answer err = %v, want conflict", err)
	}

	// The message turns out to have arrived: the next pass sees it and accepts, without a send.
	e.k.set(func() {
		task := e.k.tasks["task-a"]
		task.History = append(task.History, &a2a.Message{ID: e.k.sends[0].MessageID})
		task.Status = a2a.TaskStatus{State: a2a.TaskStateWorking}
	})
	e.waitResponse("act-1", dbstate.HITLAccepted)
	if n := e.k.sendCount(); n != 1 {
		t.Fatalf("sends after recovery = %d, want 1", n)
	}
}

func TestUncertainHoldFailsOnlyWhileTheAskStillWaits(t *testing.T) {
	e := newEnv(t, func(s *hitl.Service) { s.Opts.UncertainHold = 50 * time.Millisecond })
	e.k.onSend = func(*fakeKagent, hitl.Send) error { return context.DeadlineExceeded }
	e.k.put(waitingTask("task-a", "msg-a", toolRequest("t1")))
	e.observe()
	req := e.requestFor("task-a")
	e.run()
	if _, err := e.svc.Respond(context.Background(), req.ID, approveAll("t1")); err != nil {
		t.Fatal(err)
	}
	// The ask still waits and the message never shows: the answer failed, and the owner may answer again.
	r := e.waitResponse("act-1", dbstate.HITLRejectedTransport)
	if r.Detail == nil {
		t.Fatal("no detail")
	}
	if n := e.k.sendCount(); n != 1 {
		t.Fatalf("sends = %d", n)
	}
	view, err := e.svc.View(context.Background(), req.ID)
	if err != nil || !view.Answerable {
		t.Fatalf("request should be answerable again: %+v, %v", view, err)
	}
	e.k.onSend = nil
	// The same action id reads back the failed receipt and sends nothing.
	again, err := e.svc.Respond(context.Background(), req.ID, approveAll("t1"))
	if err != nil || again.TransportState == nil || *again.TransportState != "rejected_transport" {
		t.Fatalf("repeat of the failed action: %+v, %v", again, err)
	}
	// A fresh action id sends again, because the first answer is known not to have landed.
	e.run()
	retry := approveAll("t1")
	retry.ActionId = "act-2"
	if _, err := e.svc.Respond(context.Background(), req.ID, retry); err != nil {
		t.Fatal(err)
	}
	e.waitResponse("act-2", dbstate.HITLAccepted)
	if n := e.k.sendCount(); n != 2 {
		t.Fatalf("sends = %d, want 2", n)
	}
}

func TestNotSentStaysRecordedAndIsRetried(t *testing.T) {
	e := newEnv(t)
	e.k.sendErr = &kagent.Unreachable{Err: errors.New("dial")}
	e.k.put(waitingTask("task-a", "msg-a", toolRequest("t1")))
	e.observe()
	req := e.requestFor("task-a")
	e.run()
	if _, err := e.svc.Respond(context.Background(), req.ID, approveAll("t1")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if got := e.response("act-1").State; got != dbstate.HITLRecorded {
		t.Fatalf("state = %s, want recorded", got)
	}
	e.k.set(func() { e.k.sendErr = nil })
	e.waitResponse("act-1", dbstate.HITLAccepted)
	if n := e.k.sendCount(); n != 1 {
		t.Fatalf("sends = %d, want 1", n)
	}
}

func TestResponseNotSentWhenTheAskMovedOn(t *testing.T) {
	e := newEnv(t)
	e.k.put(waitingTask("task-a", "msg-a", toolRequest("t1")))
	e.observe()
	req := e.requestFor("task-a")
	// The task is answered elsewhere between the observer's last look and the dispatch.
	e.k.set(func() { e.k.tasks["task-a"].Status = a2a.TaskStatus{State: a2a.TaskStateWorking} })
	if _, err := e.svc.Respond(context.Background(), req.ID, approveAll("t1")); err != nil {
		t.Fatal(err)
	}
	e.run()
	r := e.waitResponse("act-1", dbstate.HITLRejectedTransport)
	if r.Detail == nil {
		t.Fatal("no detail")
	}
	if n := e.k.sendCount(); n != 0 {
		t.Fatalf("sends = %d, want 0", n)
	}
}

func TestRespondFlagOff(t *testing.T) {
	e := newEnv(t, func(s *hitl.Service) { s.WritesEnabled = false })
	e.k.put(waitingTask("task-a", "msg-a", toolRequest("t1")))
	e.observe()
	req := e.requestFor("task-a")
	body := approveAll("t1")
	got, err := e.api.RespondHITL(context.Background(), api.RespondHITLRequestObject{RequestId: req.ID, Body: &body})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.(api.RespondHITL503JSONResponse); !ok {
		t.Fatalf("response = %T, want 503", got)
	}
	if n := e.count(`SELECT count(*) FROM hitl_responses`); n != 0 {
		t.Fatalf("responses = %d", n)
	}
	g, err := e.api.GetHITL(context.Background(), api.GetHITLRequestObject{RequestId: req.ID})
	if err != nil {
		t.Fatal(err)
	}
	v, ok := g.(api.GetHITL200JSONResponse)
	if !ok || v.WritesEnabled || v.Answerable {
		t.Fatalf("view = %T %+v", g, v)
	}
}

func TestRespondValidation(t *testing.T) {
	e := newEnv(t)
	e.k.put(waitingTask("task-a", "msg-a", toolRequest("t1", "t2")))
	e.k.put(waitingTask("task-b", "msg-b", askRequest("q1")))
	e.observe()
	a, b := e.requestFor("task-a"), e.requestFor("task-b")
	ctx := context.Background()

	respond := func(reqID string, in api.HITLDecisionInput) api.RespondHITLResponseObject {
		got, err := e.api.RespondHITL(ctx, api.RespondHITLRequestObject{RequestId: reqID, Body: &in})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	tools := func(actionID string, ids ...string) api.HITLDecisionInput {
		in := approveAll(ids...)
		in.ActionId = actionID
		return in
	}
	for name, tc := range map[string]struct {
		req string
		in  api.HITLDecisionInput
	}{
		"missing a tool":         {a.ID, tools("v1", "t1")},
		"unknown tool":           {a.ID, tools("v2", "t1", "t2", "t9")},
		"duplicate tool":         {a.ID, tools("v3", "t1", "t1", "t2")},
		"empty action id":        {a.ID, tools("", "t1", "t2")},
		"answer to an approval":  {a.ID, decision("v4", answer("q1", "main"))},
		"approval to a question": {b.ID, tools("v5", "t1")},
		"wrong question id":      {b.ID, decision("v6", answer("zz", "main"))},
		"not a choice":           {b.ID, decision("v7", answer("q1", "elsewhere"))},
		"approved with reason": {a.ID, decision("v8", api.ToolApprovalResponse{
			Type: api.ToolApprovalResponseTypeToolApprovalResponse,
			Approvals: []api.ToolApproval{
				{Id: "t1", Approved: true, RejectionReason: new("no")}, {Id: "t2", Approved: true},
			},
		})},
	} {
		if _, ok := respond(tc.req, tc.in).(api.RespondHITL422JSONResponse); !ok {
			t.Errorf("%s: want 422", name)
		}
	}
	if _, ok := respond("hitl_nope", tools("v9", "t1")).(api.RespondHITL404JSONResponse); !ok {
		t.Error("unknown request: want 404")
	}
	if n := e.count(`SELECT count(*) FROM hitl_responses`); n != 0 {
		t.Fatalf("invalid answers recorded %d responses", n)
	}

	// A rejection with a reason is accepted, and its action id cannot be reused for another answer.
	ok := decision("ok-1", api.ToolApprovalResponse{
		Type: api.ToolApprovalResponseTypeToolApprovalResponse,
		Approvals: []api.ToolApproval{
			{Id: "t1", Approved: false, RejectionReason: new("too risky")}, {Id: "t2", Approved: true},
		},
	})
	got, ok2 := respond(a.ID, ok).(api.RespondHITL200JSONResponse)
	if !ok2 {
		t.Fatal("valid answer: want 200")
	}
	calls := got.Response.Calls
	if len(calls) != 2 || *calls[0].Approved || !*calls[1].Approved || calls[0].ArgumentsHash == "" {
		t.Fatalf("calls = %+v", calls)
	}
	if _, isConflict := respond(a.ID, tools("ok-1", "t1", "t2")).(api.RespondHITL409JSONResponse); !isConflict {
		t.Error("action id reused for a different answer: want 409")
	}
	if _, isConflict := respond(b.ID, decision("ok-1", answer("q1", "main"))).(api.RespondHITL409JSONResponse); !isConflict {
		t.Error("action id reused on another request: want 409")
	}
	if _, isConflict := respond(a.ID, tools("ok-2", "t1", "t2")).(api.RespondHITL409JSONResponse); !isConflict {
		t.Error("second answer to a request that has one: want 409")
	}
	// A request that is no longer pending cannot be answered.
	e.k.set(func() { e.k.tasks["task-b"].Status = a2a.TaskStatus{State: a2a.TaskStateWorking} })
	e.observe()
	if _, isConflict := respond(b.ID, decision("ok-3", answer("q1", "main"))).(api.RespondHITL409JSONResponse); !isConflict {
		t.Error("unavailable request: want 409")
	}
}

func TestObserverOutageKeepsCheckpoints(t *testing.T) {
	e := newEnv(t, func(s *hitl.Service) { s.Opts.PageSize, s.Opts.PagesPerScan = 1, 1 })
	for i := range 3 {
		e.k.put(waitingTask(fmt.Sprintf("task-%d", i), fmt.Sprintf("msg-%d", i), askRequest(fmt.Sprintf("q%d", i))))
	}
	e.observe() // page 1 of 3
	if n := len(e.requests()); n != 1 {
		t.Fatalf("requests after first page = %d, want 1", n)
	}
	cursor := func() string {
		var c *string
		if err := e.pool.QueryRow(context.Background(), `SELECT task_cursor FROM hitl_sessions WHERE kagent_session_id = $1`, ksid).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return deref(c)
	}
	if got := cursor(); got != "1" {
		t.Fatalf("cursor = %q, want 1", got)
	}

	e.k.set(func() { e.k.listErr = &kagent.Unreachable{Err: errors.New("down")}; e.k.getErr = e.k.listErr })
	e.observe()
	e.observe()
	if got := cursor(); got != "1" {
		t.Fatalf("cursor after outage = %q, want 1", got)
	}
	if n := len(e.requests()); n != 1 {
		t.Fatalf("requests after outage = %d, want 1", n)
	}
	st, err := e.svc.Status(context.Background())
	if err != nil || len(st.Diagnostics) == 0 {
		t.Fatalf("status = %+v, %v", st, err)
	}
	if r := e.requestFor("task-0"); r.Availability != dbstate.HITLPending {
		t.Fatalf("availability during a short outage = %s", r.Availability)
	}

	e.k.set(func() { e.k.listErr, e.k.getErr = nil, nil })
	e.observe()
	e.observe()
	if n := len(e.requests()); n != 3 {
		t.Fatalf("requests after recovery = %d, want 3", n)
	}
}

func TestStaleReopenAndUnavailable(t *testing.T) {
	e := newEnv(t, func(s *hitl.Service) { s.Opts.StaleAfter = 30 * time.Millisecond })
	e.k.put(waitingTask("task-a", "msg-a", toolRequest("t1")))
	e.k.put(waitingTask("task-b", "msg-b", askRequest("q1")))
	e.observe()
	a, b := e.requestFor("task-a"), e.requestFor("task-b")

	e.k.set(func() { e.k.listErr = &kagent.Unreachable{Err: errors.New("down")}; e.k.getErr = e.k.listErr })
	time.Sleep(60 * time.Millisecond)
	e.observe()
	if got := e.requestFor("task-a").Availability; got != dbstate.HITLStale {
		t.Fatalf("availability after a long outage = %s, want stale", got)
	}
	v, err := e.svc.View(context.Background(), a.ID)
	if err != nil || v.Answerable || v.Request.Availability != api.HITLProjectionAvailabilityStale || v.UnavailableReason == nil {
		t.Fatalf("stale view = %+v, %v", v, err)
	}
	if _, err := e.svc.Respond(context.Background(), a.ID, approveAll("t1")); !errors.Is(err, hitl.ErrConflict) {
		t.Fatalf("answering a stale request: %v", err)
	}

	// kagent is back: the stale request reopens, and the other task has moved on.
	e.k.set(func() {
		e.k.listErr, e.k.getErr = nil, nil
		e.k.tasks["task-b"].Status = a2a.TaskStatus{State: a2a.TaskStateCompleted}
	})
	e.observe()
	if got := e.requestFor("task-a").Availability; got != dbstate.HITLPending {
		t.Fatalf("availability after recovery = %s, want pending", got)
	}
	rb := e.requestFor("task-b")
	if rb.ID != b.ID || rb.Availability != dbstate.HITLUnavailable || rb.UnavailableReason == nil {
		t.Fatalf("moved-on request = %+v", rb)
	}
	if n := e.count(`SELECT count(*) FROM attention_items WHERE state = 'open'`); n != 1 {
		t.Fatalf("open attention rows = %d, want 1", n)
	}

	// The task disappears from kagent.
	e.k.set(func() { delete(e.k.tasks, "task-a") })
	e.observe()
	if got := e.requestFor("task-a").Availability; got != dbstate.HITLUnavailable {
		t.Fatalf("availability of a deleted task = %s", got)
	}
}

func TestNewerAskSupersedesOlder(t *testing.T) {
	e := newEnv(t)
	e.k.put(waitingTask("task-a", "msg-1", toolRequest("t1")))
	e.observe()
	old := e.requestFor("task-a")

	e.k.set(func() { e.k.tasks["task-a"] = waitingTask("task-a", "msg-2", toolRequest("t1", "t2")) })
	e.observe()

	reqs := e.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	var pending, gone int
	for _, r := range reqs {
		switch r.Availability {
		case dbstate.HITLPending:
			pending++
			if r.ID == old.ID {
				t.Fatal("the old request is still pending")
			}
		case dbstate.HITLUnavailable:
			gone++
		}
	}
	if pending != 1 || gone != 1 {
		t.Fatalf("pending %d, unavailable %d", pending, gone)
	}
	if n := e.count(`SELECT count(*) FROM attention_items WHERE state = 'open'`); n != 1 {
		t.Fatalf("open attention rows = %d, want 1", n)
	}
}

func TestMalformedAskIsADiagnosticOnly(t *testing.T) {
	e := newEnv(t)
	bad := toolRequest("t1")
	bad["tools"] = []any{}
	e.k.put(waitingTask("task-a", "msg-a", bad))
	e.observe()
	if n := len(e.requests()); n != 0 {
		t.Fatalf("requests = %d, want 0", n)
	}
	st, err := e.svc.Status(context.Background())
	if err != nil || len(st.Diagnostics) != 1 {
		t.Fatalf("status = %+v, %v", st, err)
	}
}

func TestSettleFindsTheMessageInAnotherTaskOnlyWhenTheAskMovedOn(t *testing.T) {
	e := newEnv(t)
	e.k.onSend = func(*fakeKagent, hitl.Send) error { return context.DeadlineExceeded }
	e.k.put(waitingTask("task-a", "msg-a", toolRequest("t1")))
	e.observe()
	req := e.requestFor("task-a")
	e.run()
	if _, err := e.svc.Respond(context.Background(), req.ID, approveAll("t1")); err != nil {
		t.Fatal(err)
	}
	e.waitResponse("act-1", dbstate.HITLUncertain)
	// While the ask still waits, a recheck reads only that task; it never lists the conversation.
	e.k.set(func() { e.k.finds = 0 })
	time.Sleep(100 * time.Millisecond)
	if n := e.k.findCount(); n != 0 {
		t.Fatalf("lookup listed the conversation %d times while the ask waited", n)
	}
	// kagent took the message into a new task and the old one moved on.
	e.k.set(func() {
		e.k.tasks["task-a"].Status = a2a.TaskStatus{State: a2a.TaskStateCanceled}
		e.k.tasks["task-z"] = &a2a.Task{
			ID: "task-z", ContextID: ksid, Status: a2a.TaskStatus{State: a2a.TaskStateWorking},
			History: []*a2a.Message{{ID: e.k.sends[0].MessageID}},
		}
	})
	e.waitResponse("act-1", dbstate.HITLAccepted)
	if n := e.k.sendCount(); n != 1 {
		t.Fatalf("sends = %d, want 1", n)
	}
}

func (f *fakeKagent) findCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.finds
}

// clone copies a task so a reader never shares memory with the test's later edits.
func clone(t *a2a.Task) *a2a.Task {
	c := *t
	c.History = slices.Clone(t.History)
	return &c
}
