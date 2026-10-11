package health_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/health"
	"github.com/oldsj/mainloop/server/internal/kagent"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

const (
	owner   = "owner-1"
	gateway = "http://kagent.test"
	// The fixture's kagent Session (A2A contextId) and its two tasks.
	runtimeSession = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	task1          = "health-turn-1"
	task2          = "health-turn-2"
)

// fakeKagent serves the fixture tasks and counts reads.
type fakeKagent struct {
	mu    sync.Mutex
	tasks map[string]*a2a.Task
	fail  map[string]error
	calls []string
}

func (f *fakeKagent) GetTask(_ context.Context, _ kagent.AgentRef, id string) (*a2a.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, id)
	if err := f.fail[id]; err != nil {
		return nil, err
	}
	if t, ok := f.tasks[id]; ok {
		return t, nil
	}
	return nil, errors.New("no such task")
}

func (f *fakeKagent) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type fakeAgents struct{}

func (fakeAgents) ResolveAgent(_, _, _ *string, _ dbstate.BindingRole) (kagent.AgentRef, error) {
	return kagent.AgentRef{Namespace: "kagent", Name: "claude-main"}, nil
}

// fixtureTasks reads the sanitized kagent fixture and dates every observation at `at`.
func fixtureTasks(t testing.TB, at time.Time) map[string]*a2a.Task {
	t.Helper()
	raw, err := os.ReadFile("testdata/health-tasks.json")
	if err != nil {
		t.Fatal(err)
	}
	var list []*a2a.Task
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	out := map[string]*a2a.Task{}
	for _, task := range list {
		for _, art := range task.Artifacts {
			for _, p := range art.Parts {
				p.Data().(map[string]any)["occurred_at"] = at.UTC().Format(time.RFC3339)
			}
		}
		out[string(task.ID)] = task
	}
	return out
}

// env is a scratch database with one owner, one project, one Claude session and two completed
// deliveries that carry the fixture's two tasks.
type env struct {
	pool  *pgxpool.Pool
	svc   *health.Service
	kc    *fakeKagent
	tasks map[string]*a2a.Task
	at    time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := testdb.New(t).Pool
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	kc := &fakeKagent{tasks: fixtureTasks(t, at), fail: map[string]error{}}
	e := &env{pool: pool, kc: kc, tasks: kc.tasks, at: at}
	e.svc = &health.Service{
		Pool: pool, Log: slog.New(slog.NewJSONHandler(io.Discard, nil)), OwnerID: owner, Gateway: gateway,
		EventRetention: 7 * 24 * time.Hour, SummaryRetention: 90 * 24 * time.Hour,
		MaxChildrenPerParent: 3, MaxChildrenGlobal: 6, Kagent: kc, Agents: fakeAgents{},
	}
	e.exec(t, `INSERT INTO projects (id, user_id, owner, name, full_name, html_url) VALUES ('project-1', $1, 'o', 'r', 'o/r', 'https://x.example/o/r')`, owner)
	e.session(t, "session-1", "project-1", runtimeSession)
	e.delivery(t, "msg-1", "session-1", task1)
	e.delivery(t, "msg-2", "session-1", task2)
	return e
}

func (e *env) exec(t testing.TB, sql string, args ...any) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func (e *env) session(t testing.TB, id, project, kagentSession string) {
	t.Helper()
	e.exec(t, `INSERT INTO sessions (id, user_id, project_id, title, description, prompt, status) VALUES ($1, $2, $3, 'Fix the build', 'd', 'p', 'idle')`,
		id, owner, project)
	e.exec(t, `INSERT INTO agent_bindings (session_id, kind, role, kagent_session_id) VALUES ($1, 'claude', 'main', $2)`, id, kagentSession)
}

func (e *env) delivery(t testing.TB, id, session, task string) {
	t.Helper()
	e.exec(t, `INSERT INTO deliveries (id, idempotency_key, session_id, state, task_id, kagent_session_id)
		VALUES ($1, 'key-'||$1, $2, 'completed', $3, $4)`, id, session, task, runtimeSession)
}

// ingest stores one fixture task the way the live path would: in one transaction.
func (e *env) ingest(t testing.TB, task *a2a.Task, mutate func(*health.Input)) health.Result {
	t.Helper()
	delivery := "msg-1"
	if string(task.ID) == task2 {
		delivery = "msg-2"
	}
	in := health.Input{
		OwnerID: owner, Gateway: gateway, SessionID: "session-1", DeliveryID: delivery, ReceiptAt: time.Now(),
		TaskID: string(task.ID), ContextID: task.ContextID, RuntimeSessionID: runtimeSession, Artifacts: task.Artifacts,
		Now: time.Now().UTC(), EventRetention: e.svc.EventRetention,
	}
	if mutate != nil {
		mutate(&in)
	}
	var res health.Result
	err := db.InTx(context.Background(), e.pool, func(tx pgx.Tx) error {
		var err error
		res, err = health.Ingest(context.Background(), tx, in)
		return err
	})
	if err != nil {
		t.Fatalf("ingest %s: %v", task.ID, err)
	}
	return res
}

// dump is every health row as JSON, in a fixed order, for comparing states. Received and collected
// stamps are left out: they are when, not what.
func (e *env) dump(t testing.TB) string {
	t.Helper()
	var out string
	err := e.pool.QueryRow(context.Background(), `SELECT jsonb_build_object(
	  'events', (SELECT coalesce(jsonb_agg(to_jsonb(x) - 'received_at' ORDER BY x.event_id), '[]') FROM health_events x),
	  'turns', (SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY x.session_id, x.a2a_task_id, x.turn_key), '[]') FROM health_turns x),
	  'collection', (SELECT coalesce(jsonb_agg(to_jsonb(x) - 'collected_at' - 'created_at' - 'updated_at' - 'next_attempt_at' - 'receipt_at' ORDER BY x.session_id, x.a2a_task_id), '[]') FROM health_collection x)
	)::text`).Scan(&out)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// dumpAll is dump with every stamp kept: any write at all changes it.
func (e *env) dumpAll(t testing.TB) string {
	t.Helper()
	var out string
	err := e.pool.QueryRow(context.Background(), `SELECT jsonb_build_object(
	  'events', (SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY x.event_id), '[]') FROM health_events x),
	  'turns', (SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY x.session_id, x.a2a_task_id, x.turn_key), '[]') FROM health_turns x),
	  'collection', (SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY x.session_id, x.a2a_task_id), '[]') FROM health_collection x)
	)::text`).Scan(&out)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *env) count(t testing.TB, table string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), fmt.Sprintf("SELECT count(*) FROM %s", table)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func ptr[T any](v T) *T { return &v }
