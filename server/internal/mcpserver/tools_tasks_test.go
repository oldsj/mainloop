package mcpserver_test

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/mcpserver"
)

const ordinary = mcpserver.PathOrdinary

// seedTask inserts a coordination task for the owner with an attempt in the given state bound to
// the seeded agent, and returns the task and attempt ids.
func (h *harness) seedTask(t *testing.T, parent *string, role dbstate.AttemptRole, state dbstate.AttemptState, agent authtest.Seeded) (taskID, attemptID string) {
	t.Helper()
	ctx := context.Background()
	taskID, attemptID = uuid.NewString(), uuid.NewString()
	root, depth := taskID, 1
	if parent != nil {
		if err := h.env.Pool.QueryRow(ctx, `SELECT root_task_id FROM tasks WHERE id = $1`, *parent).Scan(&root); err != nil {
			t.Fatal(err)
		}
		depth = 2
	}
	// The task's mode follows the grant its agent was issued: a workspace grant is a code task.
	var grant dbstate.MCPGrantKind
	if err := h.env.Pool.QueryRow(ctx, `SELECT mcp_grant_kind FROM agent_bindings WHERE session_id = $1`, agent.SessionID).Scan(&grant); err != nil {
		t.Fatal(err)
	}
	code := grant == dbstate.GrantWorkspace
	// A code task needs a project and a checkout; its session and its ancestors share the project.
	p := "seeded-project"
	project := &p
	if _, err := h.env.Pool.Exec(ctx, `INSERT INTO projects (id, user_id, owner, name, full_name, html_url)
		        VALUES ($1, 'owner', 'o', 'r', 'o/r', 'https://x.example/o/r') ON CONFLICT DO NOTHING`, p); err != nil {
		t.Fatal(err)
	}
	if _, err := h.env.Pool.Exec(ctx, `UPDATE sessions SET project_id = $2 WHERE id = $1`, agent.SessionID, p); err != nil {
		t.Fatal(err)
	}
	if _, err := h.env.Pool.Exec(ctx, `INSERT INTO tasks (id, owner_id, root_task_id, parent_task_id, mode, project_id, title, brief, status, reason, assigned_profile_id, selection_source,
	        checkout_branch, checkout_ref, checkout_depth)
	        VALUES ($1, 'owner', $2, $3, CASE WHEN $4 THEN 'code' ELSE 'coordination' END, $5, 'Seeded', 'Brief', 'running', NULL, 'claude', 'explicit',
	                CASE WHEN $4 THEN 'seeded' END, CASE WHEN $4 THEN '' END, CASE WHEN $4 THEN 0 END)`, taskID, root, parent, code, project); err != nil {
		t.Fatal(err)
	}
	if _, err := h.env.Pool.Exec(ctx, `INSERT INTO task_attempts (id, task_id, number, profile_id, native_provider, configuration_revision,
	        agent_namespace, agent_name, role, depth, state, binding_id, session_id, workspace_id, capacity_held, stop_state)
	        VALUES ($1, $2, 1, 'claude', 'claude', 'native-v1', 'kagent', 'agent', $3, $4, $5, $6, $6, CASE WHEN $8 THEN $6 END, $7,
	                CASE $5::text WHEN 'draining' THEN 'requested' WHEN 'fenced' THEN 'confirmed' ELSE 'none' END)`,
		attemptID, taskID, role, depth, state, agent.SessionID, state == dbstate.AttemptActive || state == dbstate.AttemptCreating || state == dbstate.AttemptDraining, code); err != nil {
		t.Fatal(err)
	}
	if _, err := h.env.Pool.Exec(ctx, `UPDATE tasks SET current_attempt_id = $1 WHERE id = $2`, attemptID, taskID); err != nil {
		t.Fatal(err)
	}
	return taskID, attemptID
}

func TestDelegateIsIdempotent(t *testing.T) {
	h := newTaskHarness(t, false)
	main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	args := map[string]any{"request_id": "r1", "title": "Plan", "brief": "Make a plan", "mode": "coordination"}

	w, first := h.call(ordinary, main.Token, "delegate", args)
	if w.Code != http.StatusOK || first.Result.IsError {
		t.Fatalf("delegate = %d %s", w.Code, w.Body)
	}
	sc := first.Result.StructuredContent
	if sc["state"] != "requested" || sc["kind"] != "create" || sc["task_id"] == nil || sc["id"] == nil {
		t.Errorf("operation = %v", sc)
	}
	_, again := h.call(ordinary, main.Token, "delegate", args)
	if again.Result.IsError || again.Result.StructuredContent["id"] != sc["id"] {
		t.Errorf("replay = %v, want the stored operation %v", again.Result.StructuredContent, sc["id"])
	}

	changed := map[string]any{"request_id": "r1", "title": "Plan B", "brief": "Make a plan", "mode": "coordination"}
	if _, r := h.call(ordinary, main.Token, "delegate", changed); !r.Result.IsError || r.text() != "[409] request_payload_conflict" {
		t.Errorf("changed payload = %v %q", r.Result.IsError, r.text())
	}
	// A code task needs a checkout.
	code := map[string]any{"request_id": "r2", "title": "Fix", "brief": "Fix it", "mode": "code"}
	if _, r := h.call(ordinary, main.Token, "delegate", code); !r.Result.IsError || r.text() != "[422] code_requires_checkout" {
		t.Errorf("code without checkout = %v %q", r.Result.IsError, r.text())
	}
	if _, r := h.call(ordinary, main.Token, "delegate", map[string]any{"request_id": "r3", "title": "x", "brief": "y", "mode": "other"}); !r.Result.IsError {
		t.Error("an unknown mode was accepted")
	}
}

func TestTaskReadsAndCancel(t *testing.T) {
	h := newTaskHarness(t, false)
	main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	_, op := h.call(ordinary, main.Token, "delegate", map[string]any{"request_id": "r1", "title": "Plan", "brief": "b", "mode": "coordination"})
	id, _ := op.Result.StructuredContent["task_id"].(string)

	for _, tool := range []string{"task_get", "task_history"} {
		_, r := h.call(ordinary, main.Token, tool, map[string]any{"task_id": id})
		task, _ := r.Result.StructuredContent["task"].(map[string]any)
		if r.Result.IsError || task["id"] != id || task["version"] == nil {
			t.Fatalf("%s = %v", tool, r.Result.StructuredContent)
		}
	}
	if _, r := h.call(ordinary, main.Token, "task_get", map[string]any{"task_id": "nope"}); !r.Result.IsError || r.text() != "[404] task_not_found" {
		t.Errorf("unknown task = %v %q", r.Result.IsError, r.text())
	}
	_, list := h.call(ordinary, main.Token, "task_list", map[string]any{})
	if got, _ := list.Result.StructuredContent["tasks"].([]any); len(got) != 1 {
		t.Errorf("task_list = %v", list.Result.StructuredContent)
	}

	_, get := h.call(ordinary, main.Token, "task_get", map[string]any{"task_id": id})
	version := get.Result.StructuredContent["task"].(map[string]any)["version"]
	stale := map[string]any{"request_id": "c0", "task_id": id, "expected_version": 99, "expected_attempt_id": nil}
	if _, r := h.call(ordinary, main.Token, "task_cancel", stale); !r.Result.IsError || r.text() != "[409] stale_task_version" {
		t.Errorf("stale cancel = %v %q", r.Result.IsError, r.text())
	}
	cancel := map[string]any{"request_id": "c1", "task_id": id, "expected_version": version, "expected_attempt_id": nil}
	_, done := h.call(ordinary, main.Token, "task_cancel", cancel)
	if done.Result.IsError || done.Result.StructuredContent["state"] != "completed" || done.Result.StructuredContent["kind"] != "cancel" {
		t.Errorf("cancel with no attempt = %v %q", done.Result.StructuredContent, done.text())
	}
}

func TestSupervisorAndChildVisibility(t *testing.T) {
	h := newTaskHarness(t, false)
	main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	sup := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleSupervisor, Grant: dbstate.GrantCoordination, Parent: main.SessionID})
	supTask, _ := h.seedTask(t, nil, dbstate.AttemptSupervisor, dbstate.AttemptActive, sup)
	child := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleChild, Grant: dbstate.GrantWorkspace, Parent: sup.SessionID})
	childTask, _ := h.seedTask(t, &supTask, dbstate.AttemptChild, dbstate.AttemptActive, child)
	other := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleChild, Grant: dbstate.GrantWorkspace, Parent: sup.SessionID})
	otherTask, _ := h.seedTask(t, &supTask, dbstate.AttemptChild, dbstate.AttemptActive, other)

	// Discovery follows the per-actor table.
	if got := h.list(t, ordinary, child.Token); !slices.Equal(got, []string{"open_pull_request", "report", "task_get", "task_history", "task_list", "whoami"}) {
		t.Errorf("child lists %v", got)
	}

	// whoami carries the task fields.
	_, who := h.call(ordinary, sup.Token, "whoami", nil)
	sc := who.Result.StructuredContent
	if who.Result.IsError || sc["task_id"] != supTask || sc["root_task_id"] != supTask || sc["attempt_id"] == nil || sc["depth"] != float64(1) {
		t.Errorf("supervisor whoami = %v %q", sc, who.text())
	}
	_, who = h.call(ordinary, child.Token, "whoami", nil)
	if sc = who.Result.StructuredContent; sc["task_id"] != childTask || sc["parent_task_id"] != supTask || sc["depth"] != float64(2) {
		t.Errorf("child whoami = %v", sc)
	}

	// A child reads itself and nothing else.
	if _, r := h.call(ordinary, child.Token, "task_get", map[string]any{"task_id": childTask}); r.Result.IsError {
		t.Errorf("child reads itself: %q", r.text())
	}
	for _, id := range []string{supTask, otherTask} {
		if _, r := h.call(ordinary, child.Token, "task_get", map[string]any{"task_id": id}); !r.Result.IsError || r.text() != "[404] task_not_found" {
			t.Errorf("child reads %s: %v %q", id, r.Result.IsError, r.text())
		}
	}
	// A supervisor sees itself and its children, and lists exactly those.
	_, list := h.call(ordinary, sup.Token, "task_list", map[string]any{})
	if got, _ := list.Result.StructuredContent["tasks"].([]any); len(got) != 3 {
		t.Errorf("supervisor lists %v", list.Result.StructuredContent)
	}
	if _, r := h.call(ordinary, sup.Token, "task_get", map[string]any{"task_id": childTask}); r.Result.IsError {
		t.Errorf("supervisor reads a child: %q", r.text())
	}
	// A supervisor delegates children under itself.
	_, d := h.call(ordinary, sup.Token, "delegate", map[string]any{"request_id": "s1", "title": "Sub", "brief": "b", "mode": "coordination"})
	if d.Result.IsError {
		t.Fatalf("supervisor delegate: %q", d.text())
	}
	var parent *string
	if err := h.env.Pool.QueryRow(context.Background(), `SELECT parent_task_id FROM tasks WHERE id = $1`, d.Result.StructuredContent["task_id"]).Scan(&parent); err != nil || parent == nil || *parent != supTask {
		t.Errorf("delegated parent = %v, %v", parent, err)
	}
}

func TestReportTool(t *testing.T) {
	h := newTaskHarness(t, false)
	main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	sup := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleSupervisor, Grant: dbstate.GrantCoordination, Parent: main.SessionID})
	task, attempt := h.seedTask(t, nil, dbstate.AttemptSupervisor, dbstate.AttemptActive, sup)
	args := map[string]any{
		"task_id": task, "attempt_id": attempt, "request_id": "rep1", "summary": "half way", "outcome": "progress",
		"evidence_refs": []string{"https://example.test/log"},
	}
	_, first := h.call(ordinary, sup.Token, "report", args)
	if first.Result.IsError || first.Result.StructuredContent["replayed"] != false {
		t.Fatalf("report = %v %q", first.Result.StructuredContent, first.text())
	}
	_, again := h.call(ordinary, sup.Token, "report", args)
	if again.Result.IsError || again.Result.StructuredContent["replayed"] != true {
		t.Errorf("replay = %v", again.Result.StructuredContent)
	}
	args["summary"] = "different"
	if _, r := h.call(ordinary, sup.Token, "report", args); !r.Result.IsError || r.text() != "[409] report_request_conflict" {
		t.Errorf("changed report = %v %q", r.Result.IsError, r.text())
	}
	// A report naming an attempt other than the caller's is out of scope.
	stale := map[string]any{"task_id": task, "attempt_id": uuid.NewString(), "request_id": "rep2", "summary": "s", "outcome": "progress"}
	if _, r := h.call(ordinary, sup.Token, "report", stale); !r.Result.IsError || r.text() != "[403] report_attempt_scope" {
		t.Errorf("foreign attempt = %v %q", r.Result.IsError, r.text())
	}
	// Completing finishes the task.
	done := map[string]any{"task_id": task, "attempt_id": attempt, "request_id": "rep3", "summary": "done", "outcome": "completed"}
	_, r := h.call(ordinary, sup.Token, "report", done)
	if r.Result.IsError || r.Result.StructuredContent["task_status"] == "running" {
		t.Errorf("completed report = %v %q", r.Result.StructuredContent, r.text())
	}
	// The main thread has no attempt to report for.
	if got := h.list(t, ordinary, main.Token); slices.Contains(got, "report") {
		t.Errorf("main lists report: %v", got)
	}
}

func TestStaleAttemptsAreRefusedAtAuthentication(t *testing.T) {
	for _, state := range []dbstate.AttemptState{dbstate.AttemptFenced, dbstate.AttemptSuperseded} {
		t.Run(string(state), func(t *testing.T) {
			h := newTaskHarness(t, false)
			main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
			sup := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleSupervisor, Grant: dbstate.GrantCoordination, Parent: main.SessionID})
			h.seedTask(t, nil, dbstate.AttemptSupervisor, state, sup)
			w, _ := h.post(ordinary, host, sup.Token, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
			if w.Code != http.StatusUnauthorized {
				t.Errorf("%s attempt: status = %d, want 401", state, w.Code)
			}
		})
	}
	// A supervisor with no attempt at all is refused too.
	h := newTaskHarness(t, false)
	main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	sup := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleSupervisor, Grant: dbstate.GrantCoordination, Parent: main.SessionID})
	if w, _ := h.post(ordinary, host, sup.Token, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}); w.Code != http.StatusUnauthorized {
		t.Errorf("no attempt: status = %d, want 401", w.Code)
	}
}

func TestMergeStatusOnlyAfterTheTaskEnds(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mergeTools bool
		grant      dbstate.MCPGrantKind
		wantOK     bool
	}{
		{"merge tools on, workspace grant", true, dbstate.GrantWorkspace, true},
		{"merge tools off", false, dbstate.GrantWorkspace, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTaskHarness(t, tc.mergeTools)
			main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
			sup := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleSupervisor, Grant: dbstate.GrantCoordination, Parent: main.SessionID})
			supTask, _ := h.seedTask(t, nil, dbstate.AttemptSupervisor, dbstate.AttemptActive, sup)
			child := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleChild, Grant: tc.grant, Parent: sup.SessionID})
			childTask, _ := h.seedTask(t, &supTask, dbstate.AttemptChild, dbstate.AttemptActive, child)
			if _, err := h.env.Pool.Exec(context.Background(), `UPDATE tasks SET status = 'completed', reason = NULL WHERE id = $1`, childTask); err != nil {
				t.Fatal(err)
			}
			// The attempt is still active and current: settlement has not run yet.
			w, r := h.post(ordinary, host, child.Token, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
				"params": map[string]any{"name": "whoami", "arguments": map[string]any{}}})
			if tc.wantOK {
				if w.Code != http.StatusOK || r.Result.StructuredContent["task_id"] != childTask {
					t.Errorf("whoami = %d %v", w.Code, r.Result.StructuredContent)
				}
				// A task that has ended reads only itself and cannot act.
				if _, g := h.call(ordinary, child.Token, "task_get", map[string]any{"task_id": childTask}); g.Result.IsError {
					t.Errorf("a finished child cannot read itself: %q", g.text())
				}
				if _, g := h.call(ordinary, child.Token, "task_get", map[string]any{"task_id": supTask}); !g.Result.IsError {
					t.Error("a finished child read its parent")
				}
				return
			}
			if w.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", w.Code)
			}
		})
	}
}

func TestDelegateRefusesBadCheckouts(t *testing.T) {
	h := newTaskHarness(t, false)
	main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	n := 0
	for _, bad := range []string{"-x", "--upload-pack=x", "refs/heads/main", "a..b", "@", "x.lock", "a b", "a~1"} {
		for _, field := range []string{"branch", "ref"} {
			n++
			checkout := map[string]any{"branch": "ok", "ref": ""}
			checkout[field] = bad
			args := map[string]any{
				"request_id": "bad-" + string(rune('a'+n)), "title": "Fix", "brief": "b", "mode": "code",
				"project_id": "p1", "checkout": checkout,
			}
			if _, r := h.call(ordinary, main.Token, "delegate", args); !r.Result.IsError || r.text() != "[422] invalid_checkout" {
				t.Errorf("%s %q = %v %q", field, bad, r.Result.IsError, r.text())
			}
		}
	}
}

func TestAgentReadsHideOwnerFields(t *testing.T) {
	h := newTaskHarness(t, false)
	main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	sup := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleSupervisor, Grant: dbstate.GrantCoordination, Parent: main.SessionID})
	_, attempt := h.seedTask(t, nil, dbstate.AttemptSupervisor, dbstate.AttemptActive, sup)
	ctx := context.Background()
	if _, err := h.env.Pool.Exec(ctx, `UPDATE task_attempts SET evidence_refs = '{"e1"}', result_ref = 'report:r', brief_delivery_id = 'd1', retention_hold = 'hold' WHERE id = $1`, attempt); err != nil {
		t.Fatal(err)
	}
	if _, r := h.call(ordinary, main.Token, "delegate", map[string]any{"request_id": "o1", "title": "T", "brief": "b", "mode": "coordination"}); r.Result.IsError {
		t.Fatal(r.text())
	}
	for _, tc := range []struct {
		name  string
		token string
	}{{"supervisor", sup.Token}, {"main", main.Token}} {
		_, r := h.call(ordinary, tc.token, "task_list", map[string]any{})
		list, _ := r.Result.StructuredContent["tasks"].([]any)
		if len(list) == 0 {
			t.Fatalf("%s lists nothing", tc.name)
		}
		for _, v := range list {
			view := v.(map[string]any)
			for _, o := range view["operations"].([]any) {
				op := o.(map[string]any)
				if op["principal_key"] != "" || op["request_digest"] != "" || len(op["request_payload"].(map[string]any)) != 0 {
					t.Errorf("%s sees operation fields %v", tc.name, op)
				}
			}
			for _, a := range view["attempts"].([]any) {
				at := a.(map[string]any)
				if len(at["evidence_refs"].([]any)) != 0 || at["result_ref"] != nil || at["brief_delivery_id"] != nil || at["retention_hold"] != nil {
					t.Errorf("%s sees attempt fields %v", tc.name, at)
				}
			}
		}
	}
}

func TestCreatingAttemptsAreNotAdmitted(t *testing.T) {
	h := newTaskHarness(t, false)
	main := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleMain, Grant: dbstate.GrantCoordination})
	sup := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleSupervisor, Grant: dbstate.GrantCoordination, Parent: main.SessionID})
	h.seedTask(t, nil, dbstate.AttemptSupervisor, dbstate.AttemptCreating, sup)
	// A creating attempt is not let into MCP.
	if w, _ := h.post(ordinary, host, sup.Token, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}); w.Code != http.StatusUnauthorized {
		t.Errorf("creating attempt: status = %d, want 401", w.Code)
	}
}
