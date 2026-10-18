package app_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/app"
	"github.com/oldsj/mainloop/server/internal/app/apicheck"
	"github.com/oldsj/mainloop/server/internal/app/apptest"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/health"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

// healthServer serves the real routes over a scratch database holding one project, one session and
// the two fixture turns, ingested the way the collector stores them.
func healthServer(t *testing.T) http.Handler {
	t.Helper()
	pool := testdb.New(t).Pool
	cfg, err := config.Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": "mainloop.test", "MAINLOOP_API_HOSTS": "api.test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO projects (id, user_id, owner, name, full_name, html_url) VALUES ('project-1', 'local-dev-user', 'o', 'r', 'o/r', 'https://x.example/o/r')`,
		`INSERT INTO projects (id, user_id, owner, name, full_name, html_url) VALUES ('project-x', 'someone-else', 'o', 'x', 'o/x', 'https://x.example/o/x')`,
		`INSERT INTO sessions (id, user_id, project_id, title, description, prompt, status) VALUES ('session-1', 'local-dev-user', 'project-1', 'Fix the build', 'd', 'p', 'idle')`,
		`INSERT INTO agent_bindings (session_id, kind, role, kagent_session_id) VALUES ('session-1', 'claude', 'main', 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa')`,
		`INSERT INTO deliveries (id, idempotency_key, session_id, state, task_id, kagent_session_id) VALUES ('msg-1', 'k1', 'session-1', 'completed', 'health-turn-1', 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa')`,
		`INSERT INTO deliveries (id, idempotency_key, session_id, state, task_id, kagent_session_id) VALUES ('msg-2', 'k2', 'session-1', 'completed', 'health-turn-2', 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	raw, err := os.ReadFile("../health/testdata/health-tasks.json")
	if err != nil {
		t.Fatal(err)
	}
	var tasks []*a2a.Task
	if err := json.Unmarshal(raw, &tasks); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	for i, task := range tasks {
		for _, art := range task.Artifacts {
			for _, p := range art.Parts {
				p.Data().(map[string]any)["occurred_at"] = at
			}
		}
		in := health.Input{
			OwnerID: cfg.Identity.OwnerID, Gateway: cfg.Kagent.GatewayURL, SessionID: "session-1", DeliveryID: "msg-" + string(rune('1'+i)),
			ReceiptAt: time.Now(), TaskID: string(task.ID), ContextID: task.ContextID, RuntimeSessionID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
			Artifacts: task.Artifacts, Now: time.Now().UTC(), EventRetention: 7 * 24 * time.Hour,
		}
		err := db.InTx(ctx, pool, func(tx pgx.Tx) error {
			_, err := health.Ingest(ctx, tx, in)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	deps := apptest.Deps(t, cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)), pool)
	deps.DB = up()
	h, err := app.New(deps).Handler()
	if err != nil {
		t.Fatal(err)
	}
	return apicheck.Wrap(t, h)
}

func keys(t *testing.T, raw any) []string {
	t.Helper()
	m, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("not an object: %v", raw)
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sameKeys(t *testing.T, what string, got any, want ...string) {
	t.Helper()
	sort.Strings(want)
	if g := keys(t, got); !reflect.DeepEqual(g, want) {
		t.Errorf("%s keys = %v, want %v", what, g, want)
	}
}

// The field names the web fixtures (web/src/features/health/test/fixtures.ts) use, field for field.
func TestOrchestrationHealthRoute(t *testing.T) {
	h := healthServer(t)
	r := send(h, "GET", "/orchestration-health", "", nil)
	if r.status != 200 {
		t.Fatalf("%d %s", r.status, r.body)
	}
	var typed api.HealthView
	strict(t, r.body, &typed)
	var view map[string]any
	if err := json.Unmarshal([]byte(r.body), &view); err != nil {
		t.Fatal(err)
	}
	sameKeys(t, "view", view, "rows", "stops", "stalled_turns", "hours", "partial", "unsupported", "retention")
	sameKeys(t, "retention", view["retention"], "events_days", "summaries_days")
	if !reflect.DeepEqual(view["unsupported"], []any{"cache", "exact_approval_wait", "codex_usage", "native_child_attribution"}) ||
		view["hours"] != float64(24) || view["partial"] != false {
		t.Errorf("view = %v", view)
	}
	rows := view["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows = %v", rows)
	}
	row := rows[0]
	sameKeys(t, "row", row, "id", "task_id", "project_id", "title", "sessions", "turns", "totals", "reports", "approvals", "gaps", "expected_receipts")
	totals := row.(map[string]any)["totals"]
	sameKeys(t, "totals", totals, "turns", "usage_observed_turns", "usage_complete_turns", "input_tokens", "output_tokens",
		"failures", "repeated_failure_groups", "unresolved_calls", "reorientation", "coverage")
	if tm := totals.(map[string]any); tm["input_tokens"] != float64(7) || tm["output_tokens"] != float64(3) || tm["failures"] != float64(3) || tm["coverage"] != "complete" {
		t.Errorf("totals = %v", tm)
	}
	session := row.(map[string]any)["sessions"].([]any)[0]
	// task_title is optional in the contract and absent for a session with no task; the session title is always there.
	sameKeys(t, "session", session, "session_id", "provider", "project_id", "task_id", "attempt_id", "title", "totals")

	// A filter that matches nothing is an empty page, not an error.
	r = send(h, "GET", "/orchestration-health?provider=codex&hours=168", "", nil)
	if r.status != 200 {
		t.Fatalf("%d %s", r.status, r.body)
	}
	strict(t, r.body, &typed)
	if len(typed.Rows) != 0 || typed.Hours != 168 {
		t.Errorf("filtered view = %+v", typed)
	}
}

func TestHealthEventsRoute(t *testing.T) {
	h := healthServer(t)
	r := send(h, "GET", "/orchestration-health/events?session_id=session-1", "", nil)
	if r.status != 200 {
		t.Fatalf("%d %s", r.status, r.body)
	}
	var typed api.HealthEvents
	strict(t, r.body, &typed)
	if len(typed.Events) != 12 || typed.Partial {
		t.Fatalf("events = %d partial = %v", len(typed.Events), typed.Partial)
	}
	var raw struct{ Events []any }
	if err := json.Unmarshal([]byte(r.body), &raw); err != nil {
		t.Fatal(err)
	}
	sameKeys(t, "event", raw.Events[0], "event_id", "task_id", "attempt_id", "received_at", "payload")
	var tool map[string]any
	for _, e := range raw.Events {
		if p := e.(map[string]any)["payload"].(map[string]any); p["kind"] == "tool" {
			tool = p
			break
		}
	}
	sameKeys(t, "tool payload", tool, "schema_version", "event_id", "producer_epoch", "sequence", "kind", "occurred_at", "time_source",
		"time_precision", "provider", "runtime_version", "source", "turn_key", "runtime_session_id", "a2a_task_id", "tool")
}

func TestHealthRoutesRejectAndHideOthersRecords(t *testing.T) {
	h := healthServer(t)
	for _, c := range []struct {
		path   string
		status int
		detail string
	}{
		{"/orchestration-health?hours=0", 422, ""},
		{"/orchestration-health?hours=8761", 422, ""},
		{"/orchestration-health?hours=abc", 422, ""},
		{"/orchestration-health/events", 422, ""},
		{"/orchestration-health/events?session_id=session-1&hours=0", 422, ""},
		{"/orchestration-health?project_id=project-x", 404, "Not found"},
		{"/orchestration-health?task_id=nope", 404, "Not found"},
		{"/orchestration-health?session_id=nope", 404, "Not found"},
		{"/orchestration-health?attempt_id=nope", 404, "Not found"},
		{"/orchestration-health/events?session_id=nope", 404, "Session not found"},
		{"/projects/nope/smoke-observations", 404, "Project not found"},
		{"/projects/project-x/smoke-observations", 404, "Project not found"},
	} {
		r := send(h, "GET", c.path, "", nil)
		if r.status != c.status {
			t.Errorf("%s: %d %s, want %d", c.path, r.status, r.body, c.status)
			continue
		}
		if c.detail != "" && detail(t, r) != c.detail {
			t.Errorf("%s: detail %v, want %s", c.path, detail(t, r), c.detail)
		}
	}
	if r := send(h, "POST", "/orchestration-health", "{}", nil); r.status != http.StatusMethodNotAllowed {
		t.Errorf("POST /orchestration-health = %d", r.status)
	}
}

func TestSmokeObservationsRoute(t *testing.T) {
	h := healthServer(t)
	r := send(h, "GET", "/projects/project-1/smoke-observations?branch=mainloop/x", "", nil)
	if r.status != 200 {
		t.Fatalf("%d %s", r.status, r.body)
	}
	var typed api.SmokeObservations
	strict(t, r.body, &typed)
	var raw map[string]any
	if err := json.Unmarshal([]byte(r.body), &raw); err != nil {
		t.Fatal(err)
	}
	sameKeys(t, "smoke", raw, "deliveries", "deliveries_busy", "deliveries_truncated", "capacity_holders", "global_capacity_available",
		"parent_capacity", "pushes", "pushes_truncated", "push_confirmed")
	if typed.DeliveriesBusy || len(typed.Pushes) != 0 || typed.PushConfirmed || !typed.GlobalCapacityAvailable {
		t.Errorf("smoke = %+v", typed)
	}
}
