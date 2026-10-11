package preview

import (
	"context"
	"net/http"
	"testing"
)

// A workspace that belongs to a task attempt is reachable only while the attempt is active and the
// task's current one. The fence is part of the lookup, so a refused request never wakes or dials.
func TestTaskOwnedPreviewNeedsTheCurrentActiveAttempt(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := e.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO tasks (id, owner_id, root_task_id, mode, title, brief, assigned_profile_id, selection_source)
	      VALUES ('t1', $1, 't1', 'coordination', 'T', 'B', 'claude', 'explicit')`, owner)
	exec(`INSERT INTO task_attempts (id, task_id, number, profile_id, native_provider, configuration_revision,
	        agent_namespace, agent_name, role, depth, state, binding_id, session_id, workspace_id, capacity_held)
	      VALUES ('a1', 't1', 1, 'claude', 'claude', 'native-v1', 'kagent', 'agent', 'supervisor', 1, 'active', 'ws1', 'ws1', 'ws1', true)`)
	exec(`UPDATE tasks SET current_attempt_id = 'a1' WHERE id = 't1'`)

	status := func() int { return e.do("GET", wsHost, "/", nil, nil).Code }
	if got := status(); got != http.StatusOK {
		t.Fatalf("current active attempt: %d, want 200", got)
	}

	exec(`UPDATE tasks SET current_attempt_id = NULL WHERE id = 't1'`)
	if got := status(); got != http.StatusNotFound {
		t.Errorf("attempt that is not current: %d, want 404", got)
	}
	exec(`UPDATE tasks SET current_attempt_id = 'a1' WHERE id = 't1'`)
	if got := status(); got != http.StatusOK {
		t.Fatalf("current again: %d, want 200", got)
	}

	for _, state := range []string{"draining", "fenced"} {
		exec(`UPDATE task_attempts SET state = $1, capacity_held = $2, stop_state = CASE $1::text WHEN 'draining' THEN 'requested' ELSE 'confirmed' END WHERE id = 'a1'`, state, state == "draining")
		if got := status(); got != http.StatusNotFound {
			t.Errorf("%s attempt: %d, want 404", state, got)
		}
	}
	if n := len(e.router.seen()); n != 2 {
		t.Errorf("%d tunnels opened, want only the two allowed requests", n)
	}
}
