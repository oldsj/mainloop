package transcripts

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

// fakeKagent is one conversation. Its page token is the index of the page's first task.
type fakeKagent struct {
	mu          sync.Mutex
	tasks       []*a2a.Task
	gets        map[string]int
	tokens      []string
	listErr     error
	getErr      error
	agentExists bool
}

func (f *fakeKagent) ListTasksPage(_ context.Context, _ kagent.AgentRef, _, token string, limit int) ([]*a2a.Task, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = append(f.tokens, token)
	if f.listErr != nil {
		return nil, "", f.listErr
	}
	start := 0
	if token != "" {
		start, _ = strconv.Atoi(token)
	}
	end := min(start+limit, len(f.tasks))
	var page []*a2a.Task
	for _, t := range f.tasks[start:end] {
		page = append(page, &a2a.Task{ID: t.ID, ContextID: t.ContextID, Status: t.Status}) // no artifacts
	}
	next := ""
	if end < len(f.tasks) {
		next = strconv.Itoa(end)
	}
	return page, next, nil
}

func (f *fakeKagent) GetTask(_ context.Context, _ kagent.AgentRef, id string) (*a2a.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets[id]++
	if f.getErr != nil {
		return nil, f.getErr
	}
	for _, t := range f.tasks {
		if string(t.ID) == id {
			return t, nil
		}
	}
	return nil, &kagent.TaskNotFound{}
}

func (f *fakeKagent) AgentExists(context.Context, kagent.AgentRef) (bool, error) {
	return f.agentExists, nil
}

func (f *fakeKagent) set(i int, st a2a.TaskState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tasks[i].Status.State = st
}

type fakeAgents struct{}

func (fakeAgents) ResolveAgent(_, _, _ *string, _ dbstate.BindingRole) (kagent.AgentRef, error) {
	return kagent.AgentRef{Namespace: "agents", Name: "claude"}, nil
}

// task is a fictional turn: a user message and one reply.
func task(id string, st a2a.TaskState) *a2a.Task {
	return &a2a.Task{
		ID: a2a.TaskID(id), ContextID: ksID, Status: a2a.TaskStatus{State: st},
		History:   []*a2a.Message{withPos(a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("question "+id)), 0)},
		Artifacts: []*a2a.Artifact{artifact(id+"-a", 10, a2a.NewTextPart("answer "+id))},
	}
}

type ingestEnv struct {
	d   *testdb.DB
	q   *dbq.Queries
	k   *fakeKagent
	svc *Service
}

// newIngest is a bound session with no transcript row yet, and a conversation of the given task
// states (t0, t1, …).
func newIngest(t *testing.T, opts Options, states ...a2a.TaskState) *ingestEnv {
	t.Helper()
	d := testdb.New(t)
	ctx := context.Background()
	for _, stmt := range []string{
		`INSERT INTO sessions (id, user_id, title, description, prompt) VALUES ('` + session + `', '` + owner + `', 't', '', '')`,
		`INSERT INTO agent_bindings (session_id, kind, role, kagent_session_id) VALUES ('` + session + `', 'claude', 'agent', '` + ksID + `')`,
	} {
		if _, err := d.Pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	k := &fakeKagent{gets: map[string]int{}, agentExists: true}
	for i, st := range states {
		k.tasks = append(k.tasks, task("t"+strconv.Itoa(i), st))
	}
	return &ingestEnv{d: d, q: dbq.New(d.Pool), k: k, svc: &Service{Pool: d.Pool, Kagent: k, Agents: fakeAgents{}, OwnerID: owner, Options: opts}}
}

func (e *ingestEnv) ingest(t *testing.T) {
	t.Helper()
	if err := e.svc.Ingest(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// due makes the session due now, as a finished turn's nudge does.
func (e *ingestEnv) due(t *testing.T) {
	t.Helper()
	if _, err := e.d.Pool.Exec(context.Background(), `UPDATE transcript_sessions SET next_scan_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
}

func (e *ingestEnv) copied(t *testing.T) map[string]bool {
	t.Helper()
	rows, err := e.d.Pool.Query(context.Background(), `SELECT task_id FROM transcript_tasks`)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids[id] = true
	}
	return ids
}

func (e *ingestEnv) session(t *testing.T) dbq.TranscriptSession {
	t.Helper()
	ts, err := e.q.GetTranscriptSession(context.Background(), ksID)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestIngestCopiesFinishedTurnsOnce(t *testing.T) {
	e := newIngest(t, Options{}, a2a.TaskStateCompleted, a2a.TaskStateFailed, a2a.TaskStateWorking, a2a.TaskStateInputRequired)
	e.ingest(t)
	if got := e.copied(t); len(got) != 2 || !got["t0"] || !got["t1"] {
		t.Fatalf("copied %v", got)
	}
	var entries int
	if err := e.d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM transcript_entries WHERE session_id = $1`, session).Scan(&entries); err != nil || entries != 4 {
		t.Fatalf("entries %d err %v", entries, err)
	}
	ts := e.session(t)
	if ts.State != string(StateActive) || ts.ScannedAt == nil || !ts.NextScanAt.After(time.Now().Add(10*time.Minute)) {
		t.Fatalf("session after a pass: %+v", ts)
	}

	// Not due: the next pass leaves it alone.
	e.ingest(t)
	if len(e.k.tokens) != 1 {
		t.Fatalf("listed %d times", len(e.k.tokens))
	}

	// The running turn finishes and nudges the session; the waiting one is still waiting.
	e.k.set(2, a2a.TaskStateCompleted)
	if err := e.q.NudgeTranscriptSession(context.Background(), ksID); err != nil {
		t.Fatal(err)
	}
	e.ingest(t)
	if got := e.copied(t); len(got) != 3 || !got["t2"] || got["t3"] {
		t.Fatalf("copied %v", got)
	}
	for id, n := range e.k.gets {
		if n != 1 {
			t.Errorf("task %s read %d times", id, n)
		}
	}
}

func TestIngestCursorHeldByUnfinishedTask(t *testing.T) {
	// Two tasks a page: [done, waiting] [done, done] [done].
	e := newIngest(t, Options{PageSize: 2},
		a2a.TaskStateCompleted, a2a.TaskStateInputRequired, a2a.TaskStateCompleted, a2a.TaskStateCompleted, a2a.TaskStateCompleted)
	e.ingest(t)
	if got := e.copied(t); len(got) != 4 || got["t1"] {
		t.Fatalf("tasks after the waiting one are copied too: %v", got)
	}
	if ts := e.session(t); ts.TaskCursor != nil {
		t.Fatalf("cursor passed the waiting task's page: %q", *ts.TaskCursor)
	}

	// Answered, it finishes: the next pass copies it and the cursor moves to the last page.
	e.k.set(1, a2a.TaskStateCompleted)
	e.due(t)
	e.ingest(t)
	if got := e.copied(t); len(got) != 5 {
		t.Fatalf("copied %v", got)
	}
	if ts := e.session(t); ts.TaskCursor == nil || *ts.TaskCursor != "4" {
		t.Fatalf("cursor %v", ts.TaskCursor)
	}

	// A new turn on the last page is found from the cursor.
	e.k.tasks = append(e.k.tasks, task("t5", a2a.TaskStateCompleted))
	e.due(t)
	e.ingest(t)
	if !e.copied(t)["t5"] || e.k.tokens[len(e.k.tokens)-1] != "4" {
		t.Fatalf("new turn not copied from the cursor; tokens %v", e.k.tokens)
	}
}

func TestIngestCopiesPerScanBound(t *testing.T) {
	e := newIngest(t, Options{CopiesPerScan: 2}, a2a.TaskStateCompleted, a2a.TaskStateCompleted, a2a.TaskStateCompleted)
	e.ingest(t)
	if len(e.copied(t)) != 2 {
		t.Fatalf("copied %v", e.copied(t))
	}
	if ts := e.session(t); ts.NextScanAt.After(time.Now().Add(time.Second)) {
		t.Fatalf("a session with copies left must be due again at once: %v", ts.NextScanAt)
	}
	e.ingest(t)
	if len(e.copied(t)) != 3 {
		t.Fatalf("copied %v", e.copied(t))
	}
}

func TestIngestGoneSession(t *testing.T) {
	for _, c := range []struct {
		name      string
		delivered bool
		state     State
	}{
		{"every sent turn copied", false, StateComplete},
		{"a sent turn not copied", true, StateUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newIngest(t, Options{}, a2a.TaskStateCompleted)
			e.ingest(t)
			if c.delivered {
				if _, err := e.d.Pool.Exec(context.Background(), `INSERT INTO deliveries (id, idempotency_key, session_id, state, task_id, kagent_session_id)
					VALUES ('m1', 'k1', $1, 'completed', 'never-copied', $2)`, session, ksID); err != nil {
					t.Fatal(err)
				}
			}
			e.k.listErr = &kagent.TaskNotFound{}
			e.due(t)
			e.ingest(t)
			ts := e.session(t)
			if ts.State != string(c.state) || (c.state == StateUnavailable) != (ts.Reason != "") {
				t.Fatalf("state %q reason %q", ts.State, ts.Reason)
			}
		})
	}
}

func TestIngestNotFoundWithoutTheAgentBacksOff(t *testing.T) {
	e := newIngest(t, Options{}, a2a.TaskStateCompleted)
	e.k.agentExists = false
	e.k.listErr = &kagent.TaskNotFound{}
	e.ingest(t)
	if ts := e.session(t); ts.State != string(StateActive) || ts.Failures != 1 {
		t.Fatalf("%+v", ts)
	}
}

func TestIngestUnreachableStopsThePass(t *testing.T) {
	e := newIngest(t, Options{}, a2a.TaskStateCompleted)
	e.k.listErr = &kagent.Unreachable{Err: errors.New("connection refused")}
	if err := e.svc.Ingest(context.Background()); err == nil {
		t.Fatal("no error")
	}
	if ts := e.session(t); ts.Failures != 0 || ts.State != string(StateActive) {
		t.Fatalf("an outage blames no session: %+v", ts)
	}
}

func TestIngestGivesUpAfterMaxFailures(t *testing.T) {
	e := newIngest(t, Options{MaxFailures: 2}, a2a.TaskStateCompleted)
	e.k.getErr = errors.New("boom")
	e.ingest(t)
	if ts := e.session(t); ts.Failures != 1 || ts.State != string(StateActive) {
		t.Fatalf("first failure: %+v", ts)
	}
	e.due(t)
	e.ingest(t)
	if ts := e.session(t); ts.State != string(StateUnavailable) || ts.Reason == "" {
		t.Fatalf("second failure: %+v", ts)
	}
}

func TestFlushAdoptsAndCopies(t *testing.T) {
	e := newIngest(t, Options{}, a2a.TaskStateCompleted)
	if err := e.svc.Flush(context.Background(), ksID); err != nil {
		t.Fatal(err)
	}
	if !e.copied(t)["t0"] {
		t.Fatal("not copied")
	}
}

func TestRetainPrunesEntriesAndKeepsReceipts(t *testing.T) {
	e := newIngest(t, Options{Retention: 24 * time.Hour}, a2a.TaskStateCompleted)
	e.ingest(t)
	e.svc.now = func() time.Time { return time.Now().Add(48 * time.Hour) }
	n, err := e.svc.Retain(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("pruned %d err %v", n, err)
	}
	var entries int
	var pruned *time.Time
	if err := e.d.Pool.QueryRow(context.Background(),
		`SELECT (SELECT count(*) FROM transcript_entries), (SELECT pruned_at FROM transcript_tasks WHERE task_id = 't0')`).Scan(&entries, &pruned); err != nil {
		t.Fatal(err)
	}
	if entries != 0 || pruned == nil {
		t.Fatalf("entries %d pruned %v", entries, pruned)
	}
	// kagent still lists the task: it is not copied again.
	e.due(t)
	e.ingest(t)
	if e.k.gets["t0"] != 1 {
		t.Fatalf("read %d times", e.k.gets["t0"])
	}
	if n, _ := e.svc.Retain(context.Background()); n != 0 {
		t.Fatalf("pruned again: %d", n)
	}
}

func TestBudgetStubsPastTheLimit(t *testing.T) {
	entries := []Entry{{Text: "aaaa"}, {Text: "bbbb"}, {Text: "cc"}}
	got := Budget(entries, 2, 8)
	if got[0].Text != "aaaa" || got[1].Text != "" || !got[1].Truncated || got[2].Text != "cc" {
		t.Fatalf("%+v", got)
	}
}
