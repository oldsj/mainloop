package mcpserver_test

import (
	"context"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/mcpserver"
)

// TestRememberRecordsWhereEachEntryCameFrom: main's entries are active at once and a task's are
// proposals, each with the session, task, attempt and latest delivery the server resolved. Only
// the proposing session withdraws a proposal, only main ends an active entry, and project_context
// shows the active entries.
func TestRememberRecordsWhereEachEntryCameFrom(t *testing.T) {
	h := newTaskHarness(t, false)
	ctx := context.Background()
	main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	sup := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleSupervisor, Grant: dbstate.GrantCoordination, Parent: main.SessionID})
	task, attempt := h.seedTask(t, nil, dbstate.AttemptSupervisor, dbstate.AttemptActive, sup) // project seeded-project
	if _, err := h.env.Pool.Exec(ctx, `INSERT INTO deliveries (id, idempotency_key, session_id, state) VALUES ('d-main', 'k-main', $1, 'delivered')`, main.SessionID); err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct{ id, user string }{{"other", "owner"}, {"theirs", "someone-else"}} {
		if _, err := h.env.Pool.Exec(ctx, `INSERT INTO projects (id, user_id, owner, name, full_name, html_url) VALUES ($1, $2, 'o', $1, 'o/' || $1, 'https://x')`, p.id, p.user); err != nil {
			t.Fatal(err)
		}
	}
	call := func(a authtest.Seeded, tool string, args map[string]any) rpc {
		t.Helper()
		_, r := h.call(mcpserver.PathOrdinary, a.Token, tool, args)
		return r
	}
	source := func(id string) string {
		t.Helper()
		var s string
		if err := h.env.Pool.QueryRow(ctx, `SELECT concat_ws('|', author, state, source_session_id, source_task_id, source_attempt_id, source_delivery_id)
			FROM project_memory_entries WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	r := call(main, "remember", map[string]any{"project_id": "seeded-project", "text": "The owner reviews UI on a phone."})
	sc := r.Result.StructuredContent
	if r.Result.IsError || sc["state"] != "active" || sc["author"] != "main" || sc["entry"] != "The owner reviews UI on a phone." ||
		!strings.Contains(r.text(), "Every new task brief") {
		t.Fatalf("main remember = %v %q", sc, r.text())
	}
	active := sc["entry_id"].(string)
	if got, want := source(active), "main|active|"+main.SessionID+"|d-main"; got != want {
		t.Fatalf("main source = %q, want %q", got, want)
	}

	r = call(sup, "remember", map[string]any{"project_id": "seeded-project", "text": "Deploys need a canary."})
	sc = r.Result.StructuredContent
	if r.Result.IsError || sc["state"] != "proposed" || sc["author"] != "task" || !strings.Contains(r.text(), "The owner decides") {
		t.Fatalf("task remember = %v %q", sc, r.text())
	}
	proposal := sc["entry_id"].(string)
	if got, want := source(proposal), "task|proposed|"+sup.SessionID+"|"+task+"|"+attempt; got != want {
		t.Fatalf("task source = %q, want %q", got, want)
	}

	for _, c := range []struct {
		name string
		a    authtest.Seeded
		tool string
		args map[string]any
		code string
	}{
		{"task on another project", sup, "remember", map[string]any{"project_id": "other", "text": "x"}, "forbidden"},
		{"main on another owner's project", main, "remember", map[string]any{"project_id": "theirs", "text": "x"}, "not_found"},
		{"replace a missing entry", main, "remember", map[string]any{"project_id": "seeded-project", "text": "y", "replaces": "nope"}, "not_found"},
		{"task ends main's entry", sup, "forget", map[string]any{"project_id": "seeded-project", "entry_id": active}, "forbidden"},
		{"main decides a proposal", main, "forget", map[string]any{"project_id": "seeded-project", "entry_id": proposal}, "conflict"},
		{"empty text", main, "remember", map[string]any{"project_id": "seeded-project", "text": "   "}, "validation"},
	} {
		if r := call(c.a, c.tool, c.args); !r.Result.IsError || !strings.HasPrefix(r.text(), "["+c.code+"]") {
			t.Errorf("%s: %q, want [%s]", c.name, r.text(), c.code)
		}
	}

	r = call(sup, "project_context", map[string]any{"project_id": "seeded-project"})
	sc = r.Result.StructuredContent
	if r.Result.IsError || sc["proposed"] != float64(1) || len(sc["memory"].([]any)) != 1 ||
		!strings.Contains(r.text(), "- The owner reviews UI on a phone. (memory "+active+")") ||
		!strings.Contains(r.text(), "1 memory proposals wait for the owner.") {
		t.Fatalf("project_context = %v %q", sc, r.text())
	}

	if r := call(sup, "forget", map[string]any{"project_id": "seeded-project", "entry_id": proposal}); r.Result.IsError || r.Result.StructuredContent["state"] != "withdrawn" {
		t.Fatalf("withdraw = %q", r.text())
	}
	if r := call(main, "forget", map[string]any{"project_id": "seeded-project", "entry_id": active}); r.Result.IsError || r.Result.StructuredContent["state"] != "forgotten" {
		t.Fatalf("main forget = %q", r.text())
	}
}
