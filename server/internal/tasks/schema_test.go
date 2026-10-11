package tasks_test

import (
	"context"
	"strings"
	"testing"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

// refused runs sql and requires the database to refuse it with a message containing want.
func (e *env) refused(want, sql string, args ...any) {
	e.t.Helper()
	_, err := e.pool.Exec(context.Background(), sql, args...)
	if err == nil || !strings.Contains(err.Error(), want) {
		e.t.Fatalf("%s: err = %v, want it to contain %q", sql, err, want)
	}
}

// The status × reason table in internal/tasks is the table in the tasks_status_reason CHECK:
// every pair the machine allows is stored, and every pair it refuses is refused by the database.
func TestStatusReasonTableMatchesTheDatabase(t *testing.T) {
	e := newEnv(t)
	statuses := []dbstate.TaskStatus{dbstate.TaskQueued, dbstate.TaskRunning, dbstate.TaskWaiting, dbstate.TaskBlocked,
		dbstate.TaskCompleted, dbstate.TaskFailed, dbstate.TaskCancelled}
	reasons := []dbstate.TaskReason{"", dbstate.ReasonOwnerDecision, dbstate.ReasonAwaitingChild, dbstate.ReasonApproval, dbstate.ReasonCI,
		dbstate.ReasonPublication, dbstate.ReasonHandoff, dbstate.ReasonReconciliation, dbstate.ReasonProvisioningUnavailable,
		dbstate.ReasonHandoffUnavailable, dbstate.ReasonCancelUnavailable}
	id := e.mustCreate(e.own, tasks.CreateInput{})
	for _, s := range statuses {
		for _, r := range reasons {
			allowed := tasks.ValidTaskState(tasks.TaskState{Status: s, Reason: r})
			_, err := e.pool.Exec(context.Background(), `UPDATE tasks SET status = $2, reason = NULLIF($3, '') WHERE id = $1`, id, s, string(r))
			if allowed && err != nil {
				t.Errorf("%s/%q: allowed by the machine, refused by the database: %v", s, r, err)
			}
			if !allowed && (err == nil || !strings.Contains(err.Error(), "tasks_status_reason")) {
				t.Errorf("%s/%q: refused by the machine, database says %v", s, r, err)
			}
		}
	}
}

func TestTaskIdentityAndVersionAreProtected(t *testing.T) {
	e := newEnv(t)
	root := e.mustCreate(e.own, tasks.CreateInput{ProjectID: "p1"})
	e.attempt(root, dbstate.AttemptSupervisor, dbstate.AttemptActive, "sup-1")
	child := e.mustCreate(e.agent(dbstate.RoleSupervisor, "sup-1"), tasks.CreateInput{})

	for _, col := range []struct{ set, value string }{
		{"owner_id", "'x'"}, {"project_id", "'p2'"}, {"topic_id", "'tp1'"}, {"parent_task_id", "NULL"},
		{"root_task_id", "id"}, {"creator_binding_id", "'other'"}, {"mode", "'code'"},
	} {
		e.refused("immutable", `UPDATE tasks SET `+col.set+` = `+col.value+` WHERE id = $1`, child)
	}
	e.exec(`UPDATE tasks SET version = version + 1 WHERE id = $1`, child)
	e.refused("version cannot decrease", `UPDATE tasks SET version = 1 WHERE id = $1`, child)
	e.refused("immutable", `UPDATE tasks SET parent_task_id = id WHERE id = $1`, root)
	e.refused("tasks_no_self_parent", `INSERT INTO tasks (id, owner_id, parent_task_id, root_task_id, title, brief, mode, assigned_profile_id, selection_source)
		VALUES ('loop', $1, 'loop', 'loop', 'T', 'B', 'coordination', 'claude', 'explicit')`, owner)
}

func TestTaskHierarchyAndOwnership(t *testing.T) {
	e := newEnv(t)
	other := func(sql string, args ...any) { e.refused("", sql, args...) }
	// A project or topic of another owner cannot be attached.
	e.refused("project owner mismatch", `INSERT INTO tasks (id, owner_id, project_id, root_task_id, title, brief, mode, assigned_profile_id, selection_source)
		VALUES ('t1', $1, 'foreign', 't1', 'T', 'B', 'coordination', 'claude', 'explicit')`, owner)
	e.exec(`INSERT INTO topics (id, user_id, name) VALUES ('tp-foreign', 'someone-else', 'x')`)
	e.refused("topic owner mismatch", `INSERT INTO tasks (id, owner_id, topic_id, root_task_id, title, brief, mode, assigned_profile_id, selection_source)
		VALUES ('t2', $1, 'tp-foreign', 't2', 'T', 'B', 'coordination', 'claude', 'explicit')`, owner)
	// A root names itself as root; a child cannot sit under another owner's task or form a loop.
	e.refused("tasks_root", `INSERT INTO tasks (id, owner_id, root_task_id, title, brief, mode, assigned_profile_id, selection_source)
		VALUES ('t3', $1, 'elsewhere', 'T', 'B', 'coordination', 'claude', 'explicit')`, owner)
	other(`INSERT INTO tasks (id, owner_id, parent_task_id, root_task_id, title, brief, mode, assigned_profile_id, selection_source)
		VALUES ('t4', $1, 'missing', 'missing', 'T', 'B', 'coordination', 'claude', 'explicit')`, owner)
	// Code tasks need a project and a checkout; coordination tasks have no checkout.
	e.refused("tasks_code_has_project", `INSERT INTO tasks (id, owner_id, root_task_id, title, brief, mode, assigned_profile_id, selection_source, checkout_branch, checkout_ref, checkout_depth)
		VALUES ('t5', $1, 't5', 'T', 'B', 'code', 'claude', 'explicit', 'b', '', 0)`, owner)
	e.refused("tasks_checkout_by_mode", `INSERT INTO tasks (id, owner_id, root_task_id, title, brief, mode, assigned_profile_id, selection_source, checkout_branch, checkout_ref, checkout_depth)
		VALUES ('t6', $1, 't6', 'T', 'B', 'coordination', 'claude', 'explicit', 'b', '', 0)`, owner)
	// A decision is all three columns or none.
	id := e.mustCreate(e.own, tasks.CreateInput{})
	e.refused("tasks_decision_whole", `UPDATE tasks SET decision_action = 'do it' WHERE id = $1`, id)
}

func TestAttemptInvariants(t *testing.T) {
	e := newEnv(t)
	root := e.mustCreate(e.own, tasks.CreateInput{})
	att := e.attempt(root, dbstate.AttemptSupervisor, dbstate.AttemptActive, "sup-1")
	insert := func(number int, state string, role string, held bool) error {
		_, err := e.pool.Exec(context.Background(), `INSERT INTO task_attempts (id, task_id, number, profile_id, native_provider, configuration_revision,
			agent_namespace, agent_name, role, depth, state, capacity_held, stop_state) VALUES (gen_random_uuid()::text, $1, $2, 'claude', 'claude', 'v', 'kagent', 'a', $3, 1, $4::text, $5,
			CASE $4::text WHEN 'draining' THEN 'requested' WHEN 'fenced' THEN 'confirmed' ELSE 'none' END)`,
			root, number, role, state, held)
		return err
	}
	// One attempt per task is unfenced.
	for _, s := range []string{"creating", "active", "draining"} {
		if err := insert(2, s, "supervisor", true); err == nil || !strings.Contains(err.Error(), "task_attempts_one_unfenced") {
			t.Errorf("second %s attempt: %v", s, err)
		}
	}
	// A fenced one is fine, and then a new one may start.
	e.exec(`UPDATE task_attempts SET state = 'fenced', stop_state = 'confirmed', capacity_held = false WHERE id = $1`, att)
	if err := insert(2, "active", "supervisor", true); err != nil {
		t.Fatal(err)
	}
	if err := insert(2, "fenced", "supervisor", false); err == nil || !strings.Contains(err.Error(), "task_attempts_task_id_number_key") {
		t.Errorf("duplicate number: %v", err)
	}
	// Unfenced attempts hold capacity.
	if err := insert(3, "fenced", "supervisor", true); err != nil {
		t.Fatal(err)
	}
	e.refused("task_attempts_unfenced_hold_capacity", `UPDATE task_attempts SET capacity_held = false WHERE task_id = $1 AND state = 'active'`, root)
	// The role follows the task's place in the tree.
	if err := insert(4, "fenced", "child", false); err == nil || !strings.Contains(err.Error(), "role does not match") {
		t.Errorf("child attempt on a root task: %v", err)
	}
	// Routing is fixed once an attempt exists.
	e.refused("routing is immutable", `UPDATE task_attempts SET profile_id = 'codex' WHERE id = $1`, att)
	e.refused("routing is immutable", `UPDATE task_attempts SET number = 9 WHERE id = $1`, att)
	e.refused("routing is immutable", `UPDATE task_attempts SET workspace_request_id = gen_random_uuid()::text WHERE id = $1`, att)
	e.refused("workspace_creates_inflight_check", `UPDATE task_attempts SET workspace_creates_inflight = -1 WHERE id = $1`, att)
	// A task's current attempt must be its own.
	other := e.mustCreate(e.own, tasks.CreateInput{})
	e.refused("tasks_current_attempt", `UPDATE tasks SET current_attempt_id = $1 WHERE id = $2`, att, other)
}

func TestImmutableRecords(t *testing.T) {
	e := newEnv(t)
	id := e.mustCreate(e.own, tasks.CreateInput{})
	att := e.attempt(id, dbstate.AttemptSupervisor, dbstate.AttemptActive, "sup-1")
	var opID string
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM task_operations WHERE task_id = $1`, id).Scan(&opID); err != nil {
		t.Fatal(err)
	}
	e.refused("request is immutable", `UPDATE task_operations SET request_digest = repeat('a', 64) WHERE id = $1`, opID)
	e.refused("request is immutable", `UPDATE task_operations SET request_id = 'other' WHERE id = $1`, opID)
	e.exec(`UPDATE task_operations SET state = 'blocked', last_confirmed_step = 'blocked' WHERE id = $1`, opID)

	e.exec(`INSERT INTO task_artifacts (id, operation_id, kind, content, sha256) VALUES ('art1', $1, 'checkpoint', '{}', repeat('b', 64))`, opID)
	e.refused("immutable", `UPDATE task_artifacts SET content = '{"x":1}' WHERE id = 'art1'`)
	e.refused("immutable", `DELETE FROM task_artifacts WHERE id = 'art1'`)
	e.refused("task_artifacts_summary_size", `INSERT INTO task_artifacts (id, operation_id, kind, content, sha256) VALUES ('art2', $1, 'unverified_provider_summary', repeat('x', 8193), repeat('b', 64))`, opID)

	e.exec(`INSERT INTO task_reports (id, task_id, attempt_id, request_id, request_digest, summary, outcome, task_version) VALUES ('rep1', $1, $2, 'r', repeat('c', 64), 's', 'progress', 1)`, id, att)
	e.refused("immutable", `UPDATE task_reports SET summary = 'changed' WHERE id = 'rep1'`)
	e.refused("immutable", `DELETE FROM task_reports WHERE id = 'rep1'`)
	e.refused("task_reports_owner_action", `INSERT INTO task_reports (id, task_id, attempt_id, request_id, request_digest, summary, outcome, owner_action, task_version) VALUES ('rep2', $1, $2, 'r2', repeat('c', 64), 's', 'progress', 'ask', 1)`, id, att)
	e.refused("task_reports_attempt_id_request_id_key", `INSERT INTO task_reports (id, task_id, attempt_id, request_id, request_digest, summary, outcome, task_version) VALUES ('rep3', $1, $2, 'r', repeat('c', 64), 's', 'progress', 1)`, id, att)
	// A report names an attempt of its own task.
	other := e.mustCreate(e.own, tasks.CreateInput{})
	e.refused("task_reports_task_id_attempt_id_fkey", `INSERT INTO task_reports (id, task_id, attempt_id, request_id, request_digest, summary, outcome, task_version) VALUES ('rep4', $1, $2, 'r4', repeat('c', 64), 's', 'progress', 1)`, other, att)
}

func TestWriterGenerationOnlyMovesUpByOne(t *testing.T) {
	e := newEnv(t)
	id := e.mustCreate(e.own, tasks.CreateInput{})
	a1 := e.attempt(id, dbstate.AttemptSupervisor, dbstate.AttemptActive, "sup-1")
	e.exec(`INSERT INTO workspace_writer_claims (owner_id, repository, branch, generation, attempt_id) VALUES ($1, 'o/r', 'main', 1, $2)`, owner, a1)
	e.exec(`UPDATE task_attempts SET state = 'fenced', stop_state = 'confirmed', capacity_held = false WHERE id = $1`, a1)
	a2 := e.uuidAttempt(id, 2)

	e.refused("must increment", `UPDATE workspace_writer_claims SET attempt_id = $1, generation = 1 WHERE repository = 'o/r'`, a2)
	e.refused("must increment", `UPDATE workspace_writer_claims SET attempt_id = $1, generation = 3 WHERE repository = 'o/r'`, a2)
	e.refused("cannot decrease", `UPDATE workspace_writer_claims SET generation = 0 WHERE repository = 'o/r'`)
	e.refused("unexpected writer generation change", `UPDATE workspace_writer_claims SET generation = 2 WHERE repository = 'o/r'`)
	e.refused("tombstones must be retained", `DELETE FROM workspace_writer_claims WHERE repository = 'o/r'`)
	e.exec(`UPDATE workspace_writer_claims SET attempt_id = $1, generation = 2 WHERE repository = 'o/r'`, a2)
	// Releasing keeps the generation; the next holder takes it up by one.
	e.exec(`UPDATE workspace_writer_claims SET held = false, fenced_at = now() WHERE repository = 'o/r'`)
	e.refused("must increment", `UPDATE workspace_writer_claims SET held = true WHERE repository = 'o/r'`)
	e.exec(`UPDATE workspace_writer_claims SET held = true, generation = 3 WHERE repository = 'o/r'`)
	e.refused("claim", `INSERT INTO workspace_writer_claims (owner_id, repository, branch, generation) VALUES ($1, 'O/R', 'x', 1)`, owner)
}

// uuidAttempt adds a creating attempt with the given number, beside an existing fenced one.
func (e *env) uuidAttempt(taskID string, number int) string {
	e.t.Helper()
	var id string
	err := e.pool.QueryRow(context.Background(), `INSERT INTO task_attempts (id, task_id, number, profile_id, native_provider, configuration_revision,
		agent_namespace, agent_name, role, depth, state) VALUES (gen_random_uuid()::text, $1, $2, 'claude', 'claude', 'v', 'kagent', 'a', 'supervisor', 1, 'creating') RETURNING id`,
		taskID, number).Scan(&id)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}
