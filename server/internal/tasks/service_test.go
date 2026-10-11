package tasks_test

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/providers"
	"github.com/oldsj/mainloop/server/internal/tasks"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

const owner = "owner-1"

type env struct {
	t    *testing.T
	pool *pgxpool.Pool
	svc  *tasks.Service
	own  tasks.Principal
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d := testdb.New(t)
	cfg, err := config.Parse(map[string]string{"AGENT_TOKEN_KEY": "k", "FRONTEND_DOMAIN": "mainloop.test", "MAINLOOP_API_HOSTS": "api.test"})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := providers.NewRegistry(cfg.Kagent)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, pool: d.Pool, own: tasks.OwnerPrincipal(owner),
		svc: &tasks.Service{Pool: d.Pool, Providers: reg, DefaultProfile: "claude", MergeTools: true}}
	for _, id := range []string{"p1", "p2"} {
		e.exec(`INSERT INTO projects (id, user_id, owner, name, full_name, html_url) VALUES ($1, $2, 'o', $1, 'o/'||$1, 'https://example.test/'||$1)`, id, owner)
	}
	e.exec(`INSERT INTO projects (id, user_id, owner, name, full_name, html_url) VALUES ('foreign', 'someone-else', 'o', 'f', 'o/f', 'https://example.test/f')`)
	e.exec(`INSERT INTO topics (id, user_id, name) VALUES ('tp1', $1, 'Infra')`, owner)
	return e
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func wantRefusal(t *testing.T, err error, status int, reason string) {
	t.Helper()
	var r *tasks.Error
	if !errors.As(err, &r) || r.Status != status || r.Reason != reason {
		t.Fatalf("err = %v, want %d %s", err, status, reason)
	}
}

func (e *env) create(p tasks.Principal, in tasks.CreateInput) (api.TaskOperation, error) {
	e.t.Helper()
	if in.RequestID == "" {
		in.RequestID = uuid.NewString()
	}
	if in.Title == "" {
		in.Title = "Title"
	}
	if in.Brief == "" {
		in.Brief = "Brief"
	}
	if in.Mode == "" {
		in.Mode = dbstate.TaskModeCoordination
	}
	return e.svc.Create(context.Background(), p, in)
}

func (e *env) mustCreate(p tasks.Principal, in tasks.CreateInput) string {
	e.t.Helper()
	op, err := e.create(p, in)
	if err != nil {
		e.t.Fatal(err)
	}
	return *op.TaskId
}

func (e *env) codeTask() string {
	return e.mustCreate(e.own, tasks.CreateInput{
		Mode: dbstate.TaskModeCode, ProjectID: "p1", Checkout: &tasks.Checkout{Branch: "feature"},
	})
}

func (e *env) view(p tasks.Principal, id string) api.TaskView {
	e.t.Helper()
	v, err := e.svc.Get(context.Background(), p, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return v
}

// attempt gives a task an attempt in the state, bound to binding, and makes it current.
func (e *env) attempt(taskID string, role dbstate.AttemptRole, state dbstate.AttemptState, binding string) string {
	e.t.Helper()
	id, depth := uuid.NewString(), 1
	if role == dbstate.AttemptChild {
		depth = 2
	}
	e.exec(`INSERT INTO task_attempts (id, task_id, number, profile_id, native_provider, configuration_revision,
	        agent_namespace, agent_name, role, depth, state, binding_id, session_id, capacity_held)
	        VALUES ($1, $2, 1, 'claude', 'claude', 'native-v1', 'kagent', 'agent', $3, $4, $5, NULLIF($6, ''), NULLIF($6, ''), $7)`,
		id, taskID, role, depth, state, binding, state == dbstate.AttemptCreating || state == dbstate.AttemptActive || state == dbstate.AttemptDraining)
	e.exec(`UPDATE tasks SET current_attempt_id = $1 WHERE id = $2`, id, taskID)
	return id
}

func (e *env) setStatus(taskID, status, reason string) {
	e.t.Helper()
	e.exec(`UPDATE tasks SET status = $2, reason = NULLIF($3, '') WHERE id = $1`, taskID, status, reason)
}

func (e *env) agent(role dbstate.BindingRole, binding string) tasks.Principal {
	e.t.Helper()
	p, err := e.svc.Resolve(context.Background(), tasks.Principal{OwnerID: owner, Role: role, BindingID: binding})
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func TestCreateRecordsTaskAndOperation(t *testing.T) {
	e := newEnv(t)
	op, err := e.create(e.own, tasks.CreateInput{Title: "Plan", Brief: "Do it", ProjectID: "p1", TopicID: "tp1"})
	if err != nil {
		t.Fatal(err)
	}
	if op.Kind != "create" || op.State != "requested" || op.LastConfirmedStep != "requested" || op.TaskId == nil ||
		op.Reason == nil || *op.Reason != "provisioning_unavailable" || op.PrincipalKey != "owner:owner-1" ||
		op.AttemptId != nil || len(op.RequestDigest) != 64 {
		t.Fatalf("operation = %+v", op)
	}
	v := e.view(e.own, *op.TaskId)
	tk := v.Task
	if tk.Status != "queued" || tk.Reason == nil || *tk.Reason != "provisioning_unavailable" || tk.Version != 1 ||
		tk.RootTaskId != tk.Id || tk.ParentTaskId != nil || tk.AssignedProfileId != "claude" ||
		tk.SelectionSource != "installation_default" || tk.ProviderConstraint != nil || tk.CurrentAttemptId != nil ||
		tk.Checkout != nil || *tk.ProjectId != "p1" || *tk.TopicId != "tp1" {
		t.Fatalf("task = %+v", tk)
	}
	if len(v.Operations) != 1 || len(v.Attempts) != 0 || len(v.Reports) != 0 || v.Projection.PrState != "unknown" || v.Projection.CiState != "unknown" {
		t.Fatalf("view = %+v", v)
	}
	if a := v.Actions; !a["cancel"].Available || a["retry"].Available || *a["retry"].Reason != "handoff_unavailable" || a["reassign"].Available {
		t.Fatalf("actions = %+v", a)
	}
	got, err := e.svc.Operation(context.Background(), e.own, op.Id)
	if err != nil || got.Id != op.Id {
		t.Fatalf("operation read = %+v, %v", got, err)
	}
	_, err = e.svc.Operation(context.Background(), e.own, "nope")
	wantRefusal(t, err, 404, tasks.ReasonOperationNotFound)
}

func TestCreateIsIdempotent(t *testing.T) {
	e := newEnv(t)
	in := tasks.CreateInput{RequestID: "r1", ProjectID: "p1"}
	first, err := e.create(e.own, in)
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.create(e.own, in)
	if err != nil || again.Id != first.Id {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	if n := e.count(`SELECT count(*) FROM tasks`); n != 1 {
		t.Fatalf("tasks = %d", n)
	}
	in.Title = "Different"
	_, err = e.create(e.own, in)
	wantRefusal(t, err, 409, tasks.ReasonPayloadConflict)
	// The same request id from another principal is another request.
	if _, err := e.create(tasks.Principal{OwnerID: owner, Role: dbstate.RoleMain, BindingID: "main-1"}, tasks.CreateInput{RequestID: "r1", ProjectID: "p1"}); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM tasks`); n != 2 {
		t.Fatalf("tasks = %d", n)
	}
}

func TestCreateValidation(t *testing.T) {
	e := newEnv(t)
	for name, tc := range map[string]struct {
		in     tasks.CreateInput
		status int
		reason string
	}{
		"code needs a project":     {tasks.CreateInput{Mode: dbstate.TaskModeCode, Checkout: &tasks.Checkout{Branch: "b"}}, 422, tasks.ReasonCodeNeedsProject},
		"code needs a checkout":    {tasks.CreateInput{Mode: dbstate.TaskModeCode, ProjectID: "p1"}, 422, tasks.ReasonCodeNeedsCheckout},
		"coordination no checkout": {tasks.CreateInput{ProjectID: "p1", Checkout: &tasks.Checkout{Branch: "b"}}, 422, tasks.ReasonCoordinationCheckout},
		"foreign project":          {tasks.CreateInput{ProjectID: "foreign"}, 404, tasks.ReasonProjectNotFound},
		"missing project":          {tasks.CreateInput{ProjectID: "nope"}, 404, tasks.ReasonProjectNotFound},
		"missing topic":            {tasks.CreateInput{TopicID: "nope"}, 404, tasks.ReasonTopicNotFound},
		"unknown provider":         {tasks.CreateInput{ProviderProfileID: "nope"}, 422, tasks.ReasonProviderUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := e.create(e.own, tc.in)
			wantRefusal(t, err, tc.status, tc.reason)
		})
	}
	if n := e.count(`SELECT count(*) FROM tasks`) + e.count(`SELECT count(*) FROM task_operations`) + e.count(`SELECT count(*) FROM events`); n != 0 {
		t.Fatalf("refused requests left %d rows", n)
	}
	if _, err := e.create(tasks.Principal{OwnerID: owner, Role: dbstate.RoleChild, BindingID: "c"}, tasks.CreateInput{}); err == nil {
		t.Fatal("child created a task")
	} else {
		wantRefusal(t, err, 403, tasks.ReasonManagementDenied)
	}
}

func TestProviderSelection(t *testing.T) {
	e := newEnv(t)
	id := e.mustCreate(e.own, tasks.CreateInput{ProviderProfileID: "codex"})
	tk := e.view(e.own, id).Task
	if tk.AssignedProfileId != "codex" || tk.SelectionSource != "explicit" || tk.ProviderConstraint == nil || *tk.ProviderConstraint != "codex" {
		t.Fatalf("explicit = %+v", tk)
	}
	e.exec(`INSERT INTO project_provider_preferences (project_id, profile_id, version) VALUES ('p2', 'codex', 1)`)
	tk = e.view(e.own, e.mustCreate(e.own, tasks.CreateInput{ProjectID: "p2"})).Task
	if tk.AssignedProfileId != "codex" || tk.SelectionSource != "project_default" || tk.ProviderConstraint != nil {
		t.Fatalf("project default = %+v", tk)
	}
	tk = e.view(e.own, e.mustCreate(e.own, tasks.CreateInput{ProjectID: "p1"})).Task
	if tk.AssignedProfileId != "claude" || tk.SelectionSource != "installation_default" {
		t.Fatalf("installation default = %+v", tk)
	}
}

func TestSupervisorCreatesChildren(t *testing.T) {
	e := newEnv(t)
	root := e.mustCreate(e.own, tasks.CreateInput{ProjectID: "p1", TopicID: "tp1", ProviderProfileID: "codex"})
	e.attempt(root, dbstate.AttemptSupervisor, dbstate.AttemptActive, "sup-1")
	sup := e.agent(dbstate.RoleSupervisor, "sup-1")

	child := e.mustCreate(sup, tasks.CreateInput{Mode: dbstate.TaskModeCode, Checkout: &tasks.Checkout{Branch: "b", Depth: 5}})
	ct := e.view(e.own, child).Task
	if ct.ParentTaskId == nil || *ct.ParentTaskId != root || ct.RootTaskId != root || *ct.ProjectId != "p1" || *ct.TopicId != "tp1" ||
		ct.AssignedProfileId != "codex" || ct.SelectionSource != "inherited" || *ct.ProviderConstraint != "codex" ||
		ct.CreatorBindingId == nil || *ct.CreatorBindingId != "sup-1" || ct.Checkout == nil || ct.Checkout.Depth != 5 {
		t.Fatalf("child = %+v", ct)
	}

	_, err := e.create(sup, tasks.CreateInput{ProviderProfileID: "claude"})
	wantRefusal(t, err, 403, tasks.ReasonInheritedProvider)
	if _, err := e.create(sup, tasks.CreateInput{ProviderProfileID: "codex"}); err != nil {
		t.Fatal(err)
	}
	_, err = e.create(sup, tasks.CreateInput{ProjectID: "p2"})
	wantRefusal(t, err, 403, tasks.ReasonInheritedProject)

	e.setStatus(root, "cancelled", "")
	_, err = e.create(sup, tasks.CreateInput{})
	wantRefusal(t, err, 409, tasks.ReasonParentNotActive)
}

func TestVisibility(t *testing.T) {
	e := newEnv(t)
	root := e.mustCreate(e.own, tasks.CreateInput{})
	other := e.mustCreate(e.own, tasks.CreateInput{})
	e.attempt(root, dbstate.AttemptSupervisor, dbstate.AttemptActive, "sup-1")
	sup := e.agent(dbstate.RoleSupervisor, "sup-1")
	child := e.mustCreate(sup, tasks.CreateInput{})
	e.attempt(child, dbstate.AttemptChild, dbstate.AttemptActive, "child-1")
	kid := e.agent(dbstate.RoleChild, "child-1")

	ids := func(p tasks.Principal) map[string]bool {
		vs, err := e.svc.List(context.Background(), p, "", "")
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]bool{}
		for _, v := range vs {
			m[v.Task.Id] = true
		}
		return m
	}
	if got := ids(e.own); len(got) != 3 {
		t.Fatalf("owner sees %v", got)
	}
	if got := ids(sup); len(got) != 2 || !got[root] || !got[child] {
		t.Fatalf("supervisor sees %v", got)
	}
	if got := ids(kid); len(got) != 1 || !got[child] {
		t.Fatalf("child sees %v", got)
	}
	if _, err := e.svc.Get(context.Background(), sup, other); err == nil {
		t.Fatal("supervisor read another root")
	} else {
		wantRefusal(t, err, 404, tasks.ReasonTaskNotFound)
	}
	if _, err := e.svc.Get(context.Background(), kid, root); err == nil {
		t.Fatal("child read its parent")
	}
	// A supervisor reads its own task but manages only its children.
	v := e.view(sup, root)
	_, err := e.svc.Cancel(context.Background(), sup, tasks.ActionInput{RequestID: "x", TaskID: root, ExpectedVersion: v.Task.Version, ExpectedAttemptID: *v.Task.CurrentAttemptId})
	wantRefusal(t, err, 403, tasks.ReasonManagementDenied)
	cv := e.view(sup, child)
	if _, err := e.svc.Cancel(context.Background(), sup, tasks.ActionInput{RequestID: "y", TaskID: child, ExpectedVersion: cv.Task.Version, ExpectedAttemptID: *cv.Task.CurrentAttemptId}); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Cancel(context.Background(), kid, tasks.ActionInput{RequestID: "z", TaskID: child})
	wantRefusal(t, err, 403, tasks.ReasonManagementDenied)

	vs, err := e.svc.List(context.Background(), e.own, "", child)
	if err != nil || len(vs) != 0 {
		t.Fatalf("children of child = %v, %v", vs, err)
	}
	vs, err = e.svc.List(context.Background(), e.own, "", root)
	if err != nil || len(vs) != 1 {
		t.Fatalf("children of root = %v, %v", vs, err)
	}
}

func action(id string, v api.TaskView, req string) tasks.ActionInput {
	in := tasks.ActionInput{RequestID: req, TaskID: id, ExpectedVersion: v.Task.Version}
	if v.Task.CurrentAttemptId != nil {
		in.ExpectedAttemptID = *v.Task.CurrentAttemptId
	}
	return in
}

func TestCancelWithoutAttemptCompletes(t *testing.T) {
	e := newEnv(t)
	id := e.mustCreate(e.own, tasks.CreateInput{})
	in := action(id, e.view(e.own, id), "c1")
	op, err := e.svc.Cancel(context.Background(), e.own, in)
	if err != nil {
		t.Fatal(err)
	}
	if op.Kind != "cancel" || op.State != "completed" || op.LastConfirmedStep != "completed" || *op.TaskId != id {
		t.Fatalf("op = %+v", op)
	}
	v := e.view(e.own, id)
	if v.Task.Status != "cancelled" || v.Task.Reason != nil || v.Task.Version != 2 || v.Actions["cancel"].Available {
		t.Fatalf("task = %+v actions = %+v", v.Task, v.Actions)
	}
	for _, o := range v.Operations {
		if o.Kind == "create" && o.State != "blocked" {
			t.Fatalf("create operation is %s after cancel", o.State)
		}
	}
	// A replay returns the stored operation and changes nothing.
	again, err := e.svc.Cancel(context.Background(), e.own, in)
	if err != nil || again.Id != op.Id || e.view(e.own, id).Task.Version != 2 {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	// Cancelling a final task completes an operation and changes nothing.
	final, err := e.svc.Cancel(context.Background(), e.own, action(id, e.view(e.own, id), "c2"))
	if err != nil || final.State != "completed" || e.view(e.own, id).Task.Version != 2 {
		t.Fatalf("final = %+v, %v", final, err)
	}
}

func TestCancelWithAttemptWaits(t *testing.T) {
	e := newEnv(t)
	id := e.mustCreate(e.own, tasks.CreateInput{})
	att := e.attempt(id, dbstate.AttemptSupervisor, dbstate.AttemptActive, "sup-1")
	e.setStatus(id, "running", "")
	op, err := e.svc.Cancel(context.Background(), e.own, action(id, e.view(e.own, id), "c1"))
	if err != nil {
		t.Fatal(err)
	}
	if op.State != "draining" || op.LastConfirmedStep != "draining" || op.AttemptId == nil || *op.AttemptId != att ||
		op.SourceAttemptId == nil || *op.SourceAttemptId != att {
		t.Fatalf("op = %+v", op)
	}
	v := e.view(e.own, id)
	if v.Task.Status != "running" || v.Task.Version != 2 {
		t.Fatalf("task = %+v", v.Task)
	}
}

func TestActionsRefuseStaleViews(t *testing.T) {
	e := newEnv(t)
	id := e.mustCreate(e.own, tasks.CreateInput{})
	v := e.view(e.own, id)
	in := action(id, v, "c1")
	in.ExpectedVersion = 7
	_, err := e.svc.Cancel(context.Background(), e.own, in)
	wantRefusal(t, err, 409, tasks.ReasonStaleVersion)
	in = action(id, v, "c2")
	in.ExpectedAttemptID = "some-attempt"
	_, err = e.svc.Cancel(context.Background(), e.own, in)
	wantRefusal(t, err, 409, tasks.ReasonStaleAttempt)
	_, err = e.svc.Cancel(context.Background(), e.own, tasks.ActionInput{RequestID: "c3", TaskID: "missing"})
	wantRefusal(t, err, 404, tasks.ReasonTaskNotFound)
	if e.view(e.own, id).Task.Version != 1 {
		t.Fatal("a refused action changed the task")
	}
}

func TestRetryAndReassignAreBlocked(t *testing.T) {
	e := newEnv(t)
	id := e.codeTask()
	for _, kind := range []string{"retry", "reassign"} {
		in := action(id, e.view(e.own, id), kind+"-1")
		in.TargetProfileID = "codex"
		var op api.TaskOperation
		var err error
		if kind == "retry" {
			op, err = e.svc.Retry(context.Background(), e.own, in)
		} else {
			op, err = e.svc.Reassign(context.Background(), e.own, in)
		}
		if err != nil {
			t.Fatal(err)
		}
		if string(op.Kind) != kind || op.State != "blocked" || op.Reason == nil || *op.Reason != "handoff_unavailable" || *op.TaskId != id {
			t.Fatalf("%s op = %+v", kind, op)
		}
	}
	if v := e.view(e.own, id); v.Task.Version != 3 || v.Task.Status != "queued" || len(v.Operations) != 3 {
		t.Fatalf("task = %+v ops = %d", v.Task, len(v.Operations))
	}

	in := action(id, e.view(e.own, id), "reassign-2")
	in.TargetProfileID = "nope"
	_, err := e.svc.Reassign(context.Background(), e.own, in)
	wantRefusal(t, err, 422, tasks.ReasonProviderUnknown)
	// The same request id with another payload is a conflict.
	in = action(id, e.view(e.own, id), "retry-1")
	_, err = e.svc.Retry(context.Background(), e.own, in)
	wantRefusal(t, err, 409, tasks.ReasonPayloadConflict)

	coord := e.mustCreate(e.own, tasks.CreateInput{})
	_, err = e.svc.Retry(context.Background(), e.own, action(coord, e.view(e.own, coord), "r"))
	wantRefusal(t, err, 422, tasks.ReasonCodeHandoffRequired)

	e.setStatus(id, "failed", "")
	_, err = e.svc.Retry(context.Background(), e.own, action(id, e.view(e.own, id), "r2"))
	wantRefusal(t, err, 409, tasks.ReasonTaskTerminal)
}

func report(p tasks.Principal, req string, outcome dbstate.ReportOutcome) tasks.ReportInput {
	return tasks.ReportInput{TaskID: p.TaskID, AttemptID: p.AttemptID, RequestID: req, Summary: "did a thing", Outcome: outcome}
}

func TestReportMovesTheTask(t *testing.T) {
	e := newEnv(t)
	id := e.codeTask()
	att := e.attempt(id, dbstate.AttemptSupervisor, dbstate.AttemptActive, "sup-1")
	e.setStatus(id, "running", "")
	sup := e.agent(dbstate.RoleSupervisor, "sup-1")
	ctx := context.Background()

	r, err := e.svc.Report(ctx, sup, report(sup, "r1", dbstate.OutcomeProgress))
	if err != nil || r.Replayed || r.Status != "running" || r.Version != 2 {
		t.Fatalf("progress = %+v, %v", r, err)
	}

	in := report(sup, "r2", dbstate.OutcomeBlocked)
	in.OwnerAction = "pick a database"
	in.EvidenceRefs = []string{"ref-1"}
	r, err = e.svc.Report(ctx, sup, in)
	if err != nil || r.Status != "blocked" || r.Version != 3 {
		t.Fatalf("blocked = %+v, %v", r, err)
	}
	v := e.view(e.own, id)
	if v.Task.Reason == nil || *v.Task.Reason != "owner_decision" || v.Task.OwnerDecision == nil ||
		v.Task.OwnerDecision.Action != "pick a database" || v.Task.OwnerDecision.AttemptId != att {
		t.Fatalf("task = %+v", v.Task)
	}
	if len(v.Reports) != 2 || v.Reports[1].Outcome != "blocked" || v.Reports[1].EvidenceRefs[0] != "ref-1" || *v.Reports[1].OwnerAction != "pick a database" {
		t.Fatalf("reports = %+v", v.Reports)
	}
	if v.Attempts[0].ResultRef == nil {
		t.Fatal("blocked report left no result ref")
	}

	// Progress while waiting for the owner resumes the task and clears the decision.
	r, err = e.svc.Report(ctx, sup, report(sup, "r3", dbstate.OutcomeProgress))
	if err != nil || r.Status != "running" {
		t.Fatalf("resume = %+v, %v", r, err)
	}
	if v := e.view(e.own, id); v.Task.OwnerDecision != nil {
		t.Fatal("decision survived progress")
	}

	r, err = e.svc.Report(ctx, sup, report(sup, "r4", dbstate.OutcomeFailed))
	if err != nil || r.Status != "blocked" {
		t.Fatalf("failed = %+v, %v", r, err)
	}
	if v := e.view(e.own, id); *v.Task.Reason != "reconciliation" || v.Task.OwnerDecision != nil {
		t.Fatalf("failed task = %+v", v.Task)
	}

	r, err = e.svc.Report(ctx, sup, report(sup, "r5", dbstate.OutcomeCompleted))
	if err != nil || r.Status != "waiting" {
		t.Fatalf("completed = %+v, %v", r, err)
	}
	if v := e.view(e.own, id); *v.Task.Reason != "publication" {
		t.Fatalf("completed task = %+v", v.Task)
	}
	if n := e.count(`SELECT count(*) FROM task_event_deliveries WHERE recipient_key = 'main'`); n != 2 {
		t.Fatalf("main deliveries = %d, want 2 (blocked, failed)", n)
	}
}

func TestReportIsIdempotent(t *testing.T) {
	e := newEnv(t)
	id := e.codeTask()
	e.attempt(id, dbstate.AttemptSupervisor, dbstate.AttemptActive, "sup-1")
	e.setStatus(id, "running", "")
	sup := e.agent(dbstate.RoleSupervisor, "sup-1")
	ctx := context.Background()
	first, err := e.svc.Report(ctx, sup, report(sup, "r1", dbstate.OutcomeProgress))
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.svc.Report(ctx, sup, report(sup, "r1", dbstate.OutcomeProgress))
	if err != nil || !again.Replayed || !reflect.DeepEqual(again.Report, first.Report) {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	if n := e.count(`SELECT count(*) FROM task_reports`); n != 1 {
		t.Fatalf("reports = %d", n)
	}
	if n := e.count(`SELECT count(*) FROM events WHERE type = 'task:updated'`); n != 2 { // create, report
		t.Fatalf("events = %d", n)
	}
	other := report(sup, "r1", dbstate.OutcomeProgress)
	other.Summary = "something else"
	_, err = e.svc.Report(ctx, sup, other)
	wantRefusal(t, err, 409, tasks.ReasonReportConflict)
}

func TestReportScopeAndRules(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.codeTask()
	e.attempt(id, dbstate.AttemptSupervisor, dbstate.AttemptActive, "sup-1")
	e.setStatus(id, "running", "")
	sup := e.agent(dbstate.RoleSupervisor, "sup-1")

	_, err := e.svc.Report(ctx, e.own, tasks.ReportInput{TaskID: id, AttemptID: "a", RequestID: "r", Summary: "s", Outcome: dbstate.OutcomeProgress})
	wantRefusal(t, err, 403, tasks.ReasonReportScope)
	bad := report(sup, "r", dbstate.OutcomeProgress)
	bad.AttemptID = "someone-elses"
	_, err = e.svc.Report(ctx, sup, bad)
	wantRefusal(t, err, 403, tasks.ReasonReportScope)
	bad = report(sup, "r", dbstate.OutcomeProgress)
	bad.OwnerAction = "ask"
	_, err = e.svc.Report(ctx, sup, bad)
	wantRefusal(t, err, 422, tasks.ReasonOwnerActionOutcome)

	// A coordination task cannot complete while a child is live.
	coord := e.mustCreate(e.own, tasks.CreateInput{})
	e.attempt(coord, dbstate.AttemptSupervisor, dbstate.AttemptActive, "sup-2")
	e.setStatus(coord, "running", "")
	sup2 := e.agent(dbstate.RoleSupervisor, "sup-2")
	e.mustCreate(sup2, tasks.CreateInput{})
	_, err = e.svc.Report(ctx, sup2, report(sup2, "r", dbstate.OutcomeCompleted))
	wantRefusal(t, err, 409, tasks.ReasonLiveChildren)
	if n := e.count(`SELECT count(*) FROM task_reports WHERE task_id = $1`, coord); n != 0 {
		t.Fatalf("a refused report left %d rows", n)
	}

	// A completed code task takes one completed report and nothing else.
	e.setStatus(id, "completed", "")
	_, err = e.svc.Report(ctx, sup, report(sup, "late", dbstate.OutcomeProgress))
	wantRefusal(t, err, 403, tasks.ReasonCompletedOutcome)
	if _, err := e.svc.Report(ctx, sup, report(sup, "done", dbstate.OutcomeCompleted)); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Report(ctx, sup, report(sup, "done-2", dbstate.OutcomeCompleted))
	wantRefusal(t, err, 409, tasks.ReasonCompletedRecorded)

	// A cancelled task takes no report.
	e.setStatus(id, "cancelled", "")
	_, err = e.svc.Report(ctx, sup, report(sup, "after-cancel", dbstate.OutcomeProgress))
	wantRefusal(t, err, 409, tasks.ReasonTaskTerminal)
}

func TestReportDeliveries(t *testing.T) {
	e := newEnv(t)
	root := e.mustCreate(e.own, tasks.CreateInput{})
	e.attempt(root, dbstate.AttemptSupervisor, dbstate.AttemptActive, "sup-1")
	sup := e.agent(dbstate.RoleSupervisor, "sup-1")
	child := e.mustCreate(sup, tasks.CreateInput{})
	e.attempt(child, dbstate.AttemptChild, dbstate.AttemptActive, "child-1")
	e.setStatus(child, "running", "")
	kid := e.agent(dbstate.RoleChild, "child-1")

	rec := func(req string, o dbstate.ReportOutcome) []string {
		r, err := e.svc.Report(context.Background(), kid, report(kid, req, o))
		if err != nil {
			t.Fatal(err)
		}
		rows, err := e.pool.Query(context.Background(), `SELECT d.recipient_key FROM task_event_deliveries d JOIN task_reports r ON r.id = d.report_id WHERE r.request_id = $1 AND r.task_id = $2 ORDER BY 1`, req, child)
		if err != nil {
			t.Fatal(err)
		}
		keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		_ = r
		return keys
	}
	if got := rec("p", dbstate.OutcomeProgress); len(got) != 1 || got[0] != "parent-task:"+root {
		t.Fatalf("progress deliveries = %v", got)
	}
	if got := rec("b", dbstate.OutcomeBlocked); len(got) != 2 || got[0] != "main" || got[1] != "parent-task:"+root {
		t.Fatalf("blocked deliveries = %v", got)
	}
	if n := e.count(`SELECT count(*) FROM task_event_deliveries WHERE state <> 'pending'`); n != 0 {
		t.Fatalf("%d deliveries are not pending", n)
	}
}

func TestAuthRefusesStaleAttempts(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.mustCreate(e.own, tasks.CreateInput{})
	e.attempt(id, dbstate.AttemptSupervisor, dbstate.AttemptActive, "sup-1")

	st, err := e.svc.Auth(ctx, dbstate.RoleSupervisor, dbstate.GrantCoordination, "sup-1")
	if err != nil || st.Depth != 1 || st.MergeStatusOnly {
		t.Fatalf("live = %+v, %v", st, err)
	}
	// A binding with the wrong role for its attempt, or none, is refused.
	for _, c := range []struct {
		role    dbstate.BindingRole
		binding string
	}{{dbstate.RoleChild, "sup-1"}, {dbstate.RoleSupervisor, "nobody"}} {
		if _, err := e.svc.Auth(ctx, c.role, dbstate.GrantCoordination, c.binding); !errors.Is(err, tasks.ErrInactive) {
			t.Fatalf("%v = %v", c, err)
		}
	}
	// An attempt that is no longer the task's current attempt, or one that is fenced, is refused.
	e.exec(`UPDATE tasks SET current_attempt_id = NULL WHERE id = $1`, id)
	if _, err := e.svc.Auth(ctx, dbstate.RoleSupervisor, dbstate.GrantCoordination, "sup-1"); !errors.Is(err, tasks.ErrInactive) {
		t.Fatalf("not current = %v", err)
	}
	e.exec(`UPDATE tasks SET current_attempt_id = (SELECT id FROM task_attempts WHERE binding_id = 'sup-1') WHERE id = $1`, id)
	e.exec(`UPDATE task_attempts SET state = 'fenced', capacity_held = false WHERE binding_id = 'sup-1'`)
	if _, err := e.svc.Auth(ctx, dbstate.RoleSupervisor, dbstate.GrantCoordination, "sup-1"); !errors.Is(err, tasks.ErrInactive) {
		t.Fatalf("fenced = %v", err)
	}
	_, err = e.svc.Resolve(ctx, tasks.Principal{OwnerID: owner, Role: dbstate.RoleSupervisor, BindingID: "sup-1"})
	wantRefusal(t, err, 403, tasks.ReasonInactivePrincipal)
}

func TestAuthMergeStatusOnly(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	root := e.mustCreate(e.own, tasks.CreateInput{ProjectID: "p1"})
	e.attempt(root, dbstate.AttemptSupervisor, dbstate.AttemptActive, "sup-1")
	sup := e.agent(dbstate.RoleSupervisor, "sup-1")
	id := e.mustCreate(sup, tasks.CreateInput{Mode: dbstate.TaskModeCode, Checkout: &tasks.Checkout{Branch: "b"}})
	e.attempt(id, dbstate.AttemptChild, dbstate.AttemptCompleted, "kid-1")
	// The attempt is done but the task is not.
	if _, err := e.svc.Auth(ctx, dbstate.RoleChild, dbstate.GrantWorkspace, "kid-1"); !errors.Is(err, tasks.ErrInactive) {
		t.Fatalf("task not completed = %v", err)
	}
	e.setStatus(id, "completed", "")
	st, err := e.svc.Auth(ctx, dbstate.RoleChild, dbstate.GrantWorkspace, "kid-1")
	if err != nil || !st.MergeStatusOnly || st.Depth != 2 {
		t.Fatalf("completed = %+v, %v", st, err)
	}
	if _, err := e.svc.Auth(ctx, dbstate.RoleChild, dbstate.GrantCoordination, "kid-1"); !errors.Is(err, tasks.ErrInactive) {
		t.Fatalf("coordination grant = %v", err)
	}
	e.svc.MergeTools = false
	if _, err := e.svc.Auth(ctx, dbstate.RoleChild, dbstate.GrantWorkspace, "kid-1"); !errors.Is(err, tasks.ErrInactive) {
		t.Fatalf("merge tools off = %v", err)
	}
}

// task:updated is written in the transaction of the change: a failure to publish rolls the
// change back, and a rolled-back change publishes nothing.
func TestTaskUpdatedSharesTheTransaction(t *testing.T) {
	e := newEnv(t)
	id := e.mustCreate(e.own, tasks.CreateInput{})
	var payload string
	if err := e.pool.QueryRow(context.Background(), `SELECT payload::text FROM events WHERE type = 'task:updated'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"task_id": "` + id + `"`, `"version": 1`, `"root_task_id": "` + id + `"`, `"type": "task:updated"`, `"attempt_id": null`} {
		if !contains(payload, want) {
			t.Errorf("payload %s lacks %s", payload, want)
		}
	}

	// Publishing fails: the whole change is rolled back.
	e.exec(`CREATE FUNCTION fail_events() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'events are down'; END $$`)
	e.exec(`CREATE TRIGGER fail_events BEFORE INSERT ON events FOR EACH ROW EXECUTE FUNCTION fail_events()`)
	before := e.count(`SELECT count(*) FROM tasks`) + e.count(`SELECT count(*) FROM task_operations`)
	if _, err := e.create(e.own, tasks.CreateInput{}); err == nil {
		t.Fatal("create succeeded with events down")
	}
	if _, err := e.svc.Cancel(context.Background(), e.own, action(id, e.view(e.own, id), "c")); err == nil {
		t.Fatal("cancel succeeded with events down")
	}
	if got := e.count(`SELECT count(*) FROM tasks`) + e.count(`SELECT count(*) FROM task_operations`); got != before {
		t.Fatalf("rows = %d, want %d", got, before)
	}
	if v := e.view(e.own, id); v.Task.Status != "queued" || v.Task.Version != 1 {
		t.Fatalf("task changed despite rollback: %+v", v.Task)
	}
	if n := e.count(`SELECT count(*) FROM events`); n != 1 {
		t.Fatalf("events = %d", n)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
