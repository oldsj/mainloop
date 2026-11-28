package mcpserver_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/mcpserver"
)

// TestOwnerScopeMemory: main's owner entries are active at once and a supervisor's are proposals;
// workers and workspace agents are refused the owner scope, and their remember and forget
// descriptions leave it out. owner_memory reads the active entries.
func TestOwnerScopeMemory(t *testing.T) {
	h := newTaskHarness(t, false)
	main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	sup := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleSupervisor, Grant: dbstate.GrantCoordination, Parent: main.SessionID})
	supTask, _ := h.seedTask(t, nil, dbstate.AttemptSupervisor, dbstate.AttemptActive, sup)
	child := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleChild, Grant: dbstate.GrantWorkspace, Parent: sup.SessionID})
	h.seedTask(t, &supTask, dbstate.AttemptChild, dbstate.AttemptActive, child)
	agent := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	call := func(a authtest.Seeded, tool string, args map[string]any) rpc {
		t.Helper()
		_, r := h.call(mcpserver.PathOrdinary, a.Token, tool, args)
		return r
	}

	r := call(main, "remember", map[string]any{"scope": "owner", "text": "I review UI on my phone."})
	sc := r.Result.StructuredContent
	if r.Result.IsError || sc["state"] != "active" || sc["author"] != "main" || sc["scope"] != "owner" ||
		!strings.Contains(r.text(), "Every session the owner has") {
		t.Fatalf("main owner remember = %v %q", sc, r.text())
	}
	active := sc["entry_id"].(string)
	r = call(sup, "remember", map[string]any{"scope": "owner", "text": "Prefers one PR per change."})
	sc = r.Result.StructuredContent
	if r.Result.IsError || sc["state"] != "proposed" || sc["author"] != "task" {
		t.Fatalf("supervisor owner remember = %v %q", sc, r.text())
	}
	proposal := sc["entry_id"].(string)

	for _, c := range []struct {
		name string
		a    authtest.Seeded
		tool string
		args map[string]any
		code string
	}{
		{"worker remembers", child, "remember", map[string]any{"scope": "owner", "text": "x"}, "forbidden"},
		{"worker forgets", child, "forget", map[string]any{"scope": "owner", "entry_id": active}, "forbidden"},
		{"workspace agent remembers", agent, "remember", map[string]any{"scope": "owner", "text": "x"}, "forbidden"},
		{"owner scope with a project", main, "remember", map[string]any{"scope": "owner", "project_id": "seeded-project", "text": "x"}, "validation"},
		{"project scope without a project", main, "remember", map[string]any{"text": "x"}, "validation"},
		{"a secret", main, "remember", map[string]any{"scope": "owner", "text": "token ghp_" + strings.Repeat("a", 36)}, "validation"},
		{"supervisor forgets main's", sup, "forget", map[string]any{"scope": "owner", "entry_id": active}, "forbidden"},
		{"main forgets a proposal", main, "forget", map[string]any{"scope": "owner", "entry_id": proposal}, "conflict"},
		{"missing entry", main, "forget", map[string]any{"scope": "owner", "entry_id": "nope"}, "not_found"},
	} {
		if r := call(c.a, c.tool, c.args); !r.Result.IsError || !strings.HasPrefix(r.text(), "["+c.code+"]") {
			t.Errorf("%s: %q, want [%s]", c.name, r.text(), c.code)
		}
	}

	r = call(child, "owner_memory", map[string]any{})
	sc = r.Result.StructuredContent
	if r.Result.IsError || len(sc["entries"].([]any)) != 1 || sc["proposed"] != float64(1) || sc["max_bytes"] != float64(3072) ||
		!strings.Contains(r.text(), "- I review UI on my phone. (owner memory "+active+", confirmed ") {
		t.Fatalf("owner_memory = %v %q", sc, r.text())
	}

	// The listing: a worker's remember and forget descriptions leave the owner scope out; main's
	// name it. The schema is the same, so a worker's owner-scope call is forbidden, not malformed.
	described := func(a authtest.Seeded) map[string]bool {
		t.Helper()
		w, r := h.post(mcpserver.PathOrdinary, host, a.Token, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
		if w.Code != http.StatusOK || r.Error != nil {
			t.Fatalf("tools/list = %d %s", w.Code, w.Body)
		}
		out := map[string]bool{}
		for _, tool := range r.Result.Tools {
			if tool.Name == "remember" || tool.Name == "forget" {
				out[tool.Name] = strings.Contains(tool.Description, "scope owner")
			}
		}
		return out
	}
	if got := described(main); !got["remember"] || !got["forget"] {
		t.Errorf("main's descriptions name the owner scope: %v, want both", got)
	}
	for _, a := range []authtest.Seeded{child, agent} {
		if got := described(a); got["remember"] || got["forget"] {
			t.Errorf("worker descriptions name the owner scope: %v, want neither", got)
		}
	}

	if r := call(sup, "forget", map[string]any{"scope": "owner", "entry_id": proposal}); r.Result.IsError || r.Result.StructuredContent["state"] != "withdrawn" {
		t.Fatalf("withdraw = %q", r.text())
	}
	if r := call(main, "forget", map[string]any{"scope": "owner", "entry_id": active}); r.Result.IsError || r.Result.StructuredContent["state"] != "forgotten" {
		t.Fatalf("main forget = %q", r.text())
	}
}
