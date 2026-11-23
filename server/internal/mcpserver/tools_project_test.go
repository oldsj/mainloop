package mcpserver_test

import (
	"context"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/mcpserver"
	"github.com/oldsj/mainloop/server/internal/projectcontext"
)

// TestProjectContextReadsTheOwnersInstructions: project_context answers a project's current
// instructions. A workspace agent reads only its own project; main reads any of its owner's
// projects; nobody reads another owner's.
func TestProjectContextReadsTheOwnersInstructions(t *testing.T) {
	h := newPRHarness(t)
	ctx := context.Background()
	ws := h.seedAgent(t, nil) // bound to p1
	main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	for _, p := range []struct{ id, user string }{{"p2", "owner"}, {"theirs", "someone-else"}} {
		if _, err := h.pool.Exec(ctx, `INSERT INTO projects (id, user_id, owner, name, full_name, html_url) VALUES ($1, $2, 'o', $1, 'o/' || $1, 'https://x')`, p.id, p.user); err != nil {
			t.Fatal(err)
		}
	}
	call := func(token, project string) rpc {
		t.Helper()
		_, r := h.call(mcpserver.PathOrdinary, token, "project_context", map[string]any{"project_id": project})
		return r
	}

	if r := call(ws.Token, "p1"); r.Result.IsError || r.Result.StructuredContent["version"] != float64(0) || r.text() != "This project has no owner instructions.\nNo active project memory." {
		t.Fatalf("unset = %v %q", r.Result.StructuredContent, r.text())
	}
	pc := &projectcontext.Service{Pool: h.pool, OwnerID: "owner"}
	if _, err := pc.SetInstructions(ctx, "p1", "Merge with --squash.", 0); err != nil {
		t.Fatal(err)
	}
	r := call(ws.Token, "p1")
	sc := r.Result.StructuredContent
	if r.Result.IsError || sc["version"] != float64(1) || sc["instructions"] != "Merge with --squash." || sc["updated_at"] == nil ||
		r.text() != "Project instructions (owner, v1):\nMerge with --squash.\nNo active project memory." {
		t.Fatalf("set = %v %q", sc, r.text())
	}
	if r := call(main.Token, "p1"); r.Result.IsError || r.Result.StructuredContent["instructions"] != "Merge with --squash." {
		t.Fatalf("main = %v %q", r.Result.StructuredContent, r.text())
	}
	if r := call(main.Token, "p2"); r.Result.IsError || r.Result.StructuredContent["version"] != float64(0) {
		t.Fatalf("main other project = %v %q", r.Result.StructuredContent, r.text())
	}
	for _, c := range []struct {
		token, project, code string
	}{
		{ws.Token, "p2", "forbidden"},
		{main.Token, "theirs", "not_found"},
		{main.Token, "missing", "not_found"},
	} {
		if r := call(c.token, c.project); !r.Result.IsError || !strings.HasPrefix(r.text(), "["+c.code+"]") {
			t.Errorf("%s: %v %q, want %s", c.project, r.Result.StructuredContent, r.text(), c.code)
		}
	}
}

// TestProjectSettingsIsMainsAlone: main reads and changes a project's worker limit for the owner,
// and the change records main's session; a supervisor may not call it.
func TestProjectSettingsIsMainsAlone(t *testing.T) {
	h := newPRHarness(t)
	h.seedAgent(t, nil) // makes p1
	main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	sup := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleSupervisor, Grant: dbstate.GrantCoordination, Parent: main.SessionID})
	call := func(token string, args map[string]any) rpc {
		t.Helper()
		_, r := h.call(mcpserver.PathOrdinary, token, "project_settings", args)
		return r
	}

	// Main reads the default, sets 4, and resets it
	if r := call(main.Token, map[string]any{"project_id": "p1"}); r.Result.IsError || r.Result.StructuredContent["max_workers"] != nil {
		t.Fatalf("read = %v %q", r.Result.StructuredContent, r.text())
	}
	if r := call(main.Token, map[string]any{"project_id": "p1", "max_workers": 4}); r.Result.IsError ||
		r.Result.StructuredContent["max_workers"] != float64(4) || r.Result.StructuredContent["effective_max_workers"] != float64(4) {
		t.Fatalf("set = %v %q", r.Result.StructuredContent, r.text())
	}
	if r := call(main.Token, map[string]any{"project_id": "p1", "reset_max_workers": true}); r.Result.IsError || r.Result.StructuredContent["max_workers"] != nil {
		t.Fatalf("reset = %v %q", r.Result.StructuredContent, r.text())
	}
	var by string
	if err := h.pool.QueryRow(context.Background(), `SELECT string_agg(changed_by || ':' || session_id, ',' ORDER BY id) FROM project_worker_limit_audit`).Scan(&by); err != nil {
		t.Fatal(err)
	}
	if want := "main:" + main.SessionID + ",main:" + main.SessionID; by != want {
		t.Fatalf("audit = %q, want %q", by, want)
	}

	// Both at once, out of range, or someone else's project is refused
	for _, args := range []map[string]any{
		{"project_id": "p1", "max_workers": 3, "reset_max_workers": true},
		{"project_id": "p1", "max_workers": 0},
		{"project_id": "missing", "max_workers": 3},
	} {
		if r := call(main.Token, args); !r.Result.IsError && r.Error == nil {
			t.Errorf("%v was accepted: %v", args, r.Result.StructuredContent)
		}
	}

	// A supervisor is refused: it must not raise its own project's limit
	if r := call(sup.Token, map[string]any{"project_id": "p1", "max_workers": 50}); !r.Result.IsError && r.Error == nil {
		t.Fatalf("a supervisor changed the limit: %v", r.Result.StructuredContent)
	}
}
