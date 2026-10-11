package tasks_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

func TestCancelActiveChildSettlesItsAttempt(t *testing.T) {
	r := newRig(t)
	r.run()
	_, sup := r.supervisor()
	child := r.codeChild(sup, "feat")
	r.eventually("child", func() bool { return r.opState(child) == "completed" })
	attempt, binding := r.attemptOf(child)

	op, err := r.svc.Cancel(context.Background(), r.own, action(child, r.view(r.own, child), "c1"))
	if err != nil || op.State != "draining" {
		t.Fatalf("cancel = %+v, %v", op, err)
	}
	r.eventually("settled", func() bool { return r.taskStatus(child) == "cancelled" && r.attemptState(child) == "cancelled" })
	r.eventually("op completed", func() bool {
		return r.str(`SELECT state FROM task_operations WHERE task_id = $1 AND kind = 'cancel'`, child) == "completed"
	})

	// Runtime told to stop, branch free, slot free.
	if r.ses.cancelCount() == 0 {
		t.Fatal("the session was not cancelled")
	}
	if n := r.count(`SELECT count(*) FROM workspace_writer_claims WHERE attempt_id = $1 AND held`, attempt); n != 0 {
		t.Fatal("branch claim still held")
	}
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND capacity_held`, attempt); n != 0 {
		t.Fatal("slot still held")
	}
	// A cancelled task takes no more reports and nothing is left to deliver.
	kid := tasks.Principal{OwnerID: owner, Role: dbstate.RoleChild, BindingID: binding}
	if _, err := r.svc.Resolve(context.Background(), kid); err == nil {
		t.Fatal("a cancelled attempt still resolves")
	}
}

func TestSettleSweepFinishesStuckAttempts(t *testing.T) {
	r := newRig(t)
	r.run()
	_, sup := r.supervisor()
	child := r.codeChild(sup, "feat")
	r.eventually("child", func() bool { return r.opState(child) == "completed" })
	attempt, _ := r.attemptOf(child)
	// The task ended by other means (a crash after the task changed, before the attempt did).
	r.exec(`UPDATE tasks SET status = 'failed', reason = NULL, version = version + 1 WHERE id = $1`, child)
	r.sweep(r.svc.SettleSweep())
	if got := r.attemptState(child); got != "failed" {
		t.Fatalf("attempt = %s", got)
	}
	// The sweep repeated changes nothing.
	n := r.ses.cancelCount()
	r.sweep(r.svc.SettleSweep())
	if r.ses.cancelCount() != n || r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND capacity_held`, attempt) != 0 {
		t.Fatal("the sweep repeated work")
	}
}

// A code task that reports completed waits for its merge: its attempt stays live for merge status,
// and the settle sweep leaves it alone.
func TestCompletedReportKeepsTheCodeAttemptLive(t *testing.T) {
	r := newRig(t)
	r.run()
	_, sup := r.supervisor()
	child := r.codeChild(sup, "feat")
	r.eventually("child", func() bool { return r.opState(child) == "completed" })
	_, binding := r.attemptOf(child)
	kid := r.agent(dbstate.RoleChild, binding)
	if _, err := r.svc.Report(context.Background(), kid, report(kid, "done", dbstate.OutcomeCompleted)); err != nil {
		t.Fatal(err)
	}
	if got := r.taskStatus(child); got != "waiting" {
		t.Fatalf("status = %s", got)
	}
	r.sweep(r.svc.SettleSweep())
	if got := r.attemptState(child); got != "active" {
		t.Fatalf("attempt = %s", got)
	}
}

// The supervisor settles (locking its child tasks) while its children report and cancel. Lock
// order is tasks before attempts, so none of it deadlocks and the end state is consistent
// whichever side wins.
func TestSupervisorSettlementAgainstChildWrites(t *testing.T) {
	for i := 0; i < 5; i++ {
		r := newRig(t)
		ctx := context.Background()
		r.run()
		root, sup := r.supervisor()
		var kids []tasks.Principal
		var ids []string
		for k := 0; k < 3; k++ {
			id := r.codeChild(sup, "feat-"+strconv.Itoa(k))
			ids = append(ids, id)
		}
		for _, id := range ids {
			r.eventually("child", func() bool { return r.opState(id) == "completed" })
			_, b := r.attemptOf(id)
			kids = append(kids, r.agent(dbstate.RoleChild, b))
		}
		var wg sync.WaitGroup
		errs := make(chan error, 16)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.svc.Cancel(ctx, r.own, action(root, r.view(r.own, root), "c-root"))
			errs <- err
		}()
		for k, kid := range kids {
			wg.Add(1)
			go func() {
				defer wg.Done()
				outcome := dbstate.OutcomeProgress
				if k == 1 {
					outcome = dbstate.OutcomeCompleted
				}
				_, err := r.svc.Report(ctx, kid, report(kid, "r", outcome))
				errs <- err
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.svc.Cancel(ctx, r.own, action(ids[0], r.view(r.own, ids[0]), "c-kid"))
			errs <- err
		}()
		wg.Wait()
		close(errs)
		for err := range errs {
			var ref *tasks.Error
			if err != nil && !errors.As(err, &ref) {
				t.Fatalf("round %d: %v", i, err)
			}
		}
		r.eventually("everything settled", func() bool {
			r.sweep(r.svc.SettleSweep())
			return r.taskStatus(root) == "cancelled" && r.count(`SELECT count(*) FROM task_attempts WHERE capacity_held`) == 0
		})
		for _, id := range ids {
			if s := r.taskStatus(id); s != "cancelled" && s != "completed" {
				t.Fatalf("round %d: child %s = %s", i, id, s)
			}
		}
		if n := r.count(`SELECT count(*) FROM workspace_writer_claims WHERE held`); n != 0 {
			t.Fatalf("round %d: held claims = %d", i, n)
		}
	}
}

// An agent sees the reports of its own current attempt only, no artifacts, no approval ids.
func TestAgentViewsAreNarrowed(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	root := e.mustCreate(e.own, tasks.CreateInput{})
	e.attempt(root, dbstate.AttemptSupervisor, dbstate.AttemptActive, "sup-1")
	e.setStatus(root, "running", "")
	sup := e.agent(dbstate.RoleSupervisor, "sup-1")
	id := e.mustCreate(sup, tasks.CreateInput{})
	old := e.attempt(id, dbstate.AttemptChild, dbstate.AttemptActive, "kid-1")
	e.setStatus(id, "running", "")
	kid := e.agent(dbstate.RoleChild, "kid-1")
	if _, err := e.svc.Report(ctx, kid, report(kid, "old", dbstate.OutcomeProgress)); err != nil {
		t.Fatal(err)
	}
	// A later attempt replaces it (as a retry would, once handoff exists).
	e.exec(`UPDATE task_attempts SET state = 'draining' WHERE id = $1`, old)
	e.exec(`UPDATE task_attempts SET state = 'fenced', capacity_held = false WHERE id = $1`, old)
	e.exec(`UPDATE task_attempts SET state = 'superseded' WHERE id = $1`, old)
	fresh := uuid.NewString()
	e.exec(`INSERT INTO task_attempts (id, task_id, number, profile_id, native_provider, configuration_revision,
	        agent_namespace, agent_name, role, depth, state, binding_id, session_id, capacity_held)
	        VALUES ($1, $2, 2, 'claude', 'claude', 'native-v1', 'kagent', 'agent', 'child', 2, 'active', 'kid-2', 'kid-2', true)`, fresh, id)
	e.exec(`UPDATE tasks SET current_attempt_id = $1 WHERE id = $2`, fresh, id)
	kid2 := e.agent(dbstate.RoleChild, "kid-2")
	if _, err := e.svc.Report(ctx, kid2, report(kid2, "new", dbstate.OutcomeProgress)); err != nil {
		t.Fatal(err)
	}

	owner := e.view(e.own, id)
	if len(owner.Reports) != 2 {
		t.Fatalf("owner reports = %d", len(owner.Reports))
	}
	for name, p := range map[string]tasks.Principal{"child": kid2, "supervisor": sup} {
		v := e.view(p, id)
		if len(v.Reports) != 1 || v.Reports[0].AttemptId != fresh {
			t.Errorf("%s sees reports %+v", name, v.Reports)
		}
		if len(v.Artifacts) != 0 || len(v.Projection.PendingApprovalIds) != 0 {
			t.Errorf("%s sees artifacts %v approvals %v", name, v.Artifacts, v.Projection.PendingApprovalIds)
		}
		for _, a := range v.Attempts {
			if a.CheckpointRef != nil || a.ManifestRef != nil || len(a.EvidenceRefs) != 0 {
				t.Errorf("%s sees attempt internals %+v", name, a)
			}
		}
	}
}
