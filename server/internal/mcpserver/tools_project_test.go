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
