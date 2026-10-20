package tasks_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/app"
	"github.com/oldsj/mainloop/server/internal/app/apicheck"
	"github.com/oldsj/mainloop/server/internal/app/apptest"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

type rest struct {
	t       *testing.T
	handler http.Handler
}

func newRest(t *testing.T) *rest {
	t.Helper()
	d := testdb.New(t)
	cfg, err := config.Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": "mainloop.test", "MAINLOOP_API_HOSTS": "api.test"})
	if err != nil {
		t.Fatal(err)
	}
	h, err := app.New(apptest.Deps(t, cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)), d.Pool)).Handler()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(context.Background(),
		`INSERT INTO projects (id, user_id, owner, name, full_name, html_url) VALUES ('p1', 'local-dev-user', 'o', 'p1', 'o/p1', 'https://example.test/p1')`); err != nil {
		t.Fatal(err)
	}
	d.SelectEnvironment(t, "local-dev-user", "p1")
	return &rest{t: t, handler: apicheck.Wrap(t, h)}
}

func (r *rest) do(method, path string, body any, want int) map[string]any {
	r.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req := httptest.NewRequestWithContext(context.Background(), method, path, rd)
	req.Host = "api.test"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, req)
	if rec.Code != want {
		r.t.Fatalf("%s %s = %d, want %d; body %s", method, path, rec.Code, want, rec.Body.String())
	}
	var m map[string]any
	if rec.Body.Len() > 0 && rec.Body.Bytes()[0] == '{' {
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			r.t.Fatal(err)
		}
	}
	if m == nil && rec.Body.Len() > 0 {
		m = map[string]any{"list": rec.Body.String()}
	}
	return m
}

func reason(m map[string]any) any {
	d, _ := m["detail"].(map[string]any)
	return d["reason"]
}

func TestRESTTaskLifecycle(t *testing.T) {
	r := newRest(t)

	var empty []any
	if err := json.Unmarshal([]byte(r.do("GET", "/tasks", nil, 200)["list"].(string)), &empty); err != nil || len(empty) != 0 {
		t.Fatalf("empty list = %v, %v", empty, err)
	}

	create := map[string]any{"request_id": "req-1", "title": "Plan", "brief": "Brief", "mode": "code", "project_id": "p1",
		"checkout": map[string]any{"branch": "task/x"}}
	op := r.do("POST", "/tasks", create, 202)
	if op["kind"] != "create" || op["state"] != "requested" || op["reason"] != nil ||
		op["principal_key"] != "owner:local-dev-user" || op["request_id"] != "req-1" {
		t.Fatalf("create operation = %v", op)
	}
	id := op["task_id"].(string)
	if again := r.do("POST", "/tasks", create, 202); again["id"] != op["id"] {
		t.Fatalf("replay = %v", again)
	}
	create["title"] = "Other"
	if c := r.do("POST", "/tasks", create, 409); reason(c) != "request_payload_conflict" {
		t.Fatalf("conflict = %v", c)
	}
	if bad := r.do("POST", "/tasks", map[string]any{"request_id": "r2", "title": "t", "brief": "b", "mode": "code", "checkout": map[string]any{"branch": "b"}}, 422); reason(bad) != "code_requires_project" {
		t.Fatalf("invalid = %v", bad)
	}
	if nf := r.do("POST", "/tasks", map[string]any{"request_id": "r3", "title": "t", "brief": "b", "mode": "coordination", "project_id": "gone"}, 404); reason(nf) != "project_not_found" {
		t.Fatalf("not found = %v", nf)
	}

	got := r.do("GET", "/tasks/"+id, nil, 200)
	task := got["task"].(map[string]any)
	if task["status"] != "queued" || task["reason"] != nil || task["version"] != 1.0 ||
		task["checkout"].(map[string]any)["branch"] != "task/x" || task["owner_id"] != "local-dev-user" {
		t.Fatalf("task = %v", task)
	}
	proj := got["projection"].(map[string]any)
	if proj["pr_state"] != "unknown" || proj["ci_state"] != "unknown" {
		t.Fatalf("projection = %v", proj)
	}
	actions := got["actions"].(map[string]any)
	if a := actions["retry"].(map[string]any); a["available"] != false || a["reason"] != "handoff_unavailable" {
		t.Fatalf("retry action = %v", actions)
	}
	if a := actions["cancel"].(map[string]any); a["available"] != true || a["reason"] != nil {
		t.Fatalf("cancel action = %v", actions)
	}
	for _, k := range []string{"attempts", "operations", "artifacts", "reports"} {
		if _, ok := got[k].([]any); !ok {
			t.Errorf("%s = %v, want an array", k, got[k])
		}
	}

	read := r.do("GET", "/task-operations/"+op["id"].(string), nil, 200)
	if read["id"] != op["id"] {
		t.Fatalf("operation read = %v", read)
	}
	if nf := r.do("GET", "/task-operations/nope", nil, 404); reason(nf) != "operation_not_found" {
		t.Fatalf("operation 404 = %v", nf)
	}
	if nf := r.do("GET", "/tasks/nope", nil, 404); reason(nf) != "task_not_found" {
		t.Fatalf("task 404 = %v", nf)
	}

	// Retry and reassign answer 202 with the blocked operation; each is a task change.
	act := func(kind, req string, version float64) map[string]any {
		body := map[string]any{"request_id": req, "expected_version": version, "expected_attempt_id": nil}
		if kind == "reassign" {
			body["target_profile_id"] = "codex"
		}
		return r.do("POST", "/tasks/"+id+"/"+kind, body, 202)
	}
	if o := act("retry", "a1", 1); o["state"] != "blocked" || o["reason"] != "handoff_unavailable" || o["kind"] != "retry" {
		t.Fatalf("retry = %v", o)
	}
	if o := act("reassign", "a2", 2); o["state"] != "blocked" || o["reason"] != "handoff_unavailable" || o["kind"] != "reassign" {
		t.Fatalf("reassign = %v", o)
	}
	stale := map[string]any{"request_id": "a3", "expected_version": 1, "expected_attempt_id": nil}
	if c := r.do("POST", "/tasks/"+id+"/cancel", stale, 409); reason(c) != "stale_task_version" {
		t.Fatalf("stale = %v", c)
	}

	if o := act("cancel", "a4", 3); o["state"] != "completed" || o["kind"] != "cancel" {
		t.Fatalf("cancel = %v", o)
	}
	final := r.do("GET", "/tasks/"+id, nil, 200)
	if final["task"].(map[string]any)["status"] != "cancelled" || final["task"].(map[string]any)["version"] != 4.0 {
		t.Fatalf("after cancel = %v", final["task"])
	}
	if c := r.do("POST", "/tasks/"+id+"/retry", map[string]any{"request_id": "a5", "expected_version": 4, "expected_attempt_id": nil}, 409); reason(c) != "task_terminal" {
		t.Fatalf("retry after cancel = %v", c)
	}

	var list []map[string]any
	if err := json.Unmarshal([]byte(r.do("GET", "/tasks?project_id=p1", nil, 200)["list"].(string)), &list); err != nil || len(list) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}
	r.do("POST", "/tasks/"+id+"/cancel", nil, 422)
}
