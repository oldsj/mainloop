package mcpserver_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/mcpserver"
	"github.com/oldsj/mainloop/server/internal/privileged"
	"github.com/oldsj/mainloop/server/internal/privileged/privilegedtest"
)

const privilegedHead = "0123456789abcdef0123456789abcdef01234567"

// privilegedGitHub serves octo-org/widgets#7 open at privilegedHead.
func privilegedGitHub() *privilegedtest.GitHub {
	g := privilegedtest.New()
	g.Add("octo-org/widgets", 7, privilegedHead)
	return g
}

func privilegedMergeArgs(requestID string) map[string]any {
	return map[string]any{
		"request_id": requestID, "type": privileged.TypePRMerge, "reason": "CI is green.",
		"args": map[string]any{"repo": "octo-org/widgets", "pr": 7, "expected_head_sha": privilegedHead},
	}
}

// TestRequestPrivilegedAction: main's request waits for the owner with a card and a digest; a
// supervisor's request records its task; another session cannot read or cancel it; a bad argument
// is a validation error; with privileged actions off the tools say so.
func TestRequestPrivilegedAction(t *testing.T) {
	h := newTaskHarness(t, false)
	ctx := context.Background()
	main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	sup := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleSupervisor, Grant: dbstate.GrantCoordination, Parent: main.SessionID})
	task, _ := h.seedTask(t, nil, dbstate.AttemptSupervisor, dbstate.AttemptActive, sup)

	_, r := h.call(mcpserver.PathOrdinary, main.Token, "request_privileged_action", privilegedMergeArgs("r1"))
	sc := r.Result.StructuredContent
	if r.Result.IsError || sc["state"] != privileged.StatePendingApproval || len(sc["digest"].(string)) != 64 ||
		!strings.Contains(r.text(), "end your turn now, do not poll") {
		t.Fatalf("request = %v %q", sc, r.text())
	}
	id := sc["action_id"].(string)
	if sc["inbox_url"] != "/inbox?item=privileged_action_"+id {
		t.Errorf("inbox_url = %v", sc["inbox_url"])
	}
	var n int
	if err := h.env.Pool.QueryRow(ctx, `SELECT count(*) FROM attention_items WHERE source_id = $1 AND state = 'open'`, "privileged_action_"+id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("cards = %d, %v", n, err)
	}

	_, r = h.call(mcpserver.PathOrdinary, sup.Token, "get_privileged_action", map[string]any{"action_id": id})
	if !r.Result.IsError || !strings.Contains(r.text(), "[not_found]") {
		t.Fatalf("another session's get = %q", r.text())
	}
	_, r = h.call(mcpserver.PathOrdinary, sup.Token, "cancel_privileged_action", map[string]any{"action_id": id})
	if !r.Result.IsError || !strings.Contains(r.text(), "[not_found]") {
		t.Fatalf("another session's cancel = %q", r.text())
	}

	_, r = h.call(mcpserver.PathOrdinary, sup.Token, "request_privileged_action", privilegedMergeArgs("s1"))
	if r.Result.IsError {
		t.Fatalf("supervisor request = %q", r.text())
	}
	var who string
	if err := h.env.Pool.QueryRow(ctx, `SELECT concat_ws('|', requester_role, requester_task_id) FROM privileged_actions WHERE id = $1`,
		r.Result.StructuredContent["action_id"]).Scan(&who); err != nil || who != "supervisor|"+task {
		t.Fatalf("requester = %q, %v", who, err)
	}

	bad := privilegedMergeArgs("r2")
	bad["args"] = map[string]any{"repo": "octo-org/widgets", "pr": 7, "expected_head_sha": "abc"}
	_, r = h.call(mcpserver.PathOrdinary, main.Token, "request_privileged_action", bad)
	if !r.Result.IsError || !strings.Contains(r.text(), "[validation] args: expected_head_sha") && !strings.Contains(r.text(), "[validation] expected_head_sha") {
		t.Fatalf("bad args = %q", r.text())
	}

	_, r = h.call(mcpserver.PathOrdinary, main.Token, "get_privileged_action", map[string]any{"action_id": id})
	if r.Result.IsError || r.Result.StructuredContent["events"] == nil {
		t.Fatalf("get = %v %q", r.Result.StructuredContent, r.text())
	}
	_, r = h.call(mcpserver.PathOrdinary, main.Token, "cancel_privileged_action", map[string]any{"action_id": id})
	if r.Result.IsError || r.Result.StructuredContent["state"] != privileged.StateCancelled {
		t.Fatalf("cancel = %q", r.text())
	}
}

func TestPrivilegedActionsOff(t *testing.T) {
	h := buildHarness(t, false, true, func(s *mcpserver.Server, _ *pgxpool.Pool) { s.Privileged = nil })
	main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	_, r := h.call(mcpserver.PathOrdinary, main.Token, "request_privileged_action", privilegedMergeArgs("r1"))
	if !r.Result.IsError || !strings.Contains(r.text(), "[unavailable] privileged actions are not enabled") {
		t.Fatalf("off = %q", r.text())
	}
}

// command.exec is granted only to a session whose Agent is declared to run auto permission mode,
// locked (MAINLOOP_LOCKED_AUTO_AGENTS): main on a listed Agent may request it, main on any other
// Agent, or on none, is refused with unsupported_action and nothing is recorded.
func TestRequestExecNeedsALockedAutoAgent(t *testing.T) {
	h := buildHarness(t, false, true, func(s *mcpserver.Server, _ *pgxpool.Pool) {
		s.Policy.LockedAutoAgents = []string{"kagent/claude-main-dev"}
		s.Privileged.ExecutorOn, s.Privileged.ToolboxOn = true, true
	})
	args := func(id string) map[string]any {
		return map[string]any{
			"request_id": id, "type": privileged.TypeExec, "reason": "Check DNS from outside.",
			"args": map[string]any{"target": "toolbox", "reach": "internet", "script": "dig +short example.org"},
		}
	}
	locked := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination, AgentNamespace: "kagent", AgentName: "claude-main-dev"})
	_, r := h.call(mcpserver.PathOrdinary, locked.Token, "request_privileged_action", args("e1"))
	if r.Result.IsError || r.Result.StructuredContent["state"] != privileged.StateRequested {
		t.Fatalf("a locked auto session's request = %v %q", r.Result.StructuredContent, r.text())
	}
	for name, a := range map[string]authtest.Agent{
		"bypass agent":    {Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination, AgentNamespace: "kagent", AgentName: "claude-workspace-dev"},
		"other namespace": {Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination, AgentNamespace: "other", AgentName: "claude-main-dev"},
		"no agent":        {Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination},
	} {
		a.User = "owner-" + name
		s := h.env.Seed(t, a)
		_, r := h.call(mcpserver.PathOrdinary, s.Token, "request_privileged_action", args("e-"+strings.ReplaceAll(name, " ", "-")))
		if !r.Result.IsError || !strings.Contains(r.text(), "[unsupported_action]") || !strings.Contains(r.text(), "auto permission mode, locked") {
			t.Errorf("%s: %q", name, r.text())
		}
	}
	var n int
	if err := h.env.Pool.QueryRow(context.Background(), `SELECT count(*) FROM privileged_actions`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows = %d, %v", n, err)
	}
}
