package tasks_test

import (
	"strings"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/providers"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

func (r *rig) refusals(reason string) int {
	r.t.Helper()
	return r.count(`SELECT coalesce((SELECT total FROM task_capacity_refusals WHERE reason = $1), 0)`, reason)
}

func (r *rig) waiting(id string) bool {
	r.t.Helper()
	return r.count(`SELECT count(*) FROM tasks WHERE id = $1 AND runtime_wait_since IS NOT NULL`, id) == 1
}

func (r *rig) tier(id string) string {
	return r.str(`SELECT capacity_tier FROM tasks WHERE id = $1`, id)
}

// With no overflow tier, admission is the single global limit, every child is base and never
// waits, and each refusal is counted once under its reason.
func TestNoOverflowTierIsTheGlobalLimit(t *testing.T) {
	r := newRig(t)
	r.svc.Limits.MaxChildrenPerParent, r.svc.Limits.MaxChildrenGlobal = 1, 2
	mk := func(binding string) tasks.Principal {
		root := r.mustCreate(r.own, tasks.CreateInput{ProjectID: "p1"})
		r.attempt(root, dbstate.AttemptSupervisor, dbstate.AttemptActive, binding)
		r.setStatus(root, "running", "")
		return r.agent(dbstate.RoleSupervisor, binding)
	}
	a, b, c := mk("sup-a"), mk("sup-b"), mk("sup-c")
	ca := r.mustCreate(a, tasks.CreateInput{})
	_, err := r.create(a, tasks.CreateInput{})
	wantRefusal(t, err, 409, tasks.ReasonChildCapacity)
	cb := r.mustCreate(b, tasks.CreateInput{})
	_, err = r.create(c, tasks.CreateInput{})
	wantRefusal(t, err, 409, tasks.ReasonGlobalCapacity)
	for _, id := range []string{ca, cb} {
		if r.tier(id) != "base" || r.waiting(id) {
			t.Fatalf("child %s: tier %s waiting %v", id, r.tier(id), r.waiting(id))
		}
	}
	if r.refusals("child_capacity") != 1 || r.refusals("global_capacity") != 1 {
		t.Fatalf("refusals: child %d global %d", r.refusals("child_capacity"), r.refusals("global_capacity"))
	}
}

// Above the base limit a child gets an overflow slot, up to base + overflow; it waits for runtime
// capacity until kagent has accepted a message for its attempt.
func TestOverflowTierAdmitsAndWaitsForAcceptance(t *testing.T) {
	r := newRig(t)
	r.svc.Limits.MaxChildrenGlobal, r.svc.Limits.MaxChildrenOverflow = 1, 1
	r.svc.Limits.OverflowWait = time.Hour
	r.run()
	_, sup := r.supervisor()
	base := r.mustCreate(sup, tasks.CreateInput{})
	over := r.mustCreate(sup, tasks.CreateInput{})
	_, err := r.create(sup, tasks.CreateInput{})
	wantRefusal(t, err, 409, tasks.ReasonGlobalCapacity)
	if r.refusals("global_capacity") != 1 {
		t.Fatalf("global refusals = %d", r.refusals("global_capacity"))
	}
	if r.tier(base) != "base" || r.waiting(base) || r.tier(over) != "overflow" || !r.waiting(over) {
		t.Fatalf("base %s/%v, overflow %s/%v", r.tier(base), r.waiting(base), r.tier(over), r.waiting(over))
	}
	v := r.view(r.own, over)
	if v.Task.CapacityTier == nil || *v.Task.CapacityTier != "overflow" || v.Task.RuntimeWaitSince == nil {
		t.Fatalf("view = %+v", v.Task)
	}
	r.eventually("overflow child provisioned", func() bool { return r.opState(over) == "completed" })

	// The brief is recorded but kagent has not taken it: still waiting.
	r.sweep(r.svc.RuntimeWaitSweep())
	if !r.waiting(over) {
		t.Fatal("wait ended before kagent accepted a message")
	}
	attempt, session := r.attemptOf(over)
	if session == "" {
		session = r.str(`SELECT session_id FROM task_attempts WHERE id = $1`, attempt)
	}
	r.exec(`UPDATE deliveries SET task_id = 'kagent-task' WHERE session_id = $1`, session)
	r.sweep(r.svc.RuntimeWaitSweep())
	if r.waiting(over) || r.taskStatus(over) == "failed" {
		t.Fatalf("after acceptance: waiting %v status %s", r.waiting(over), r.taskStatus(over))
	}
	if r.tier(over) != "overflow" {
		t.Fatalf("tier = %s", r.tier(over))
	}
}

// A task still waiting after the window fails, its attempt is stopped with evidence, and the wait
// ends with the task.
func TestOverflowWaitGivesUp(t *testing.T) {
	r := newRig(t)
	r.svc.Limits.MaxChildrenGlobal, r.svc.Limits.MaxChildrenOverflow = 1, 1
	r.svc.Limits.OverflowWait = time.Minute
	r.run()
	_, sup := r.supervisor()
	r.mustCreate(sup, tasks.CreateInput{})
	over := r.mustCreate(sup, tasks.CreateInput{})
	r.eventually("overflow child provisioned", func() bool { return r.opState(over) == "completed" })
	r.exec(`UPDATE tasks SET runtime_wait_since = now() - interval '2 minutes' WHERE id = $1`, over)
	r.sweep(r.svc.RuntimeWaitSweep())
	if got := r.taskStatus(over); got != "failed" || r.taskReason(over) != "" {
		t.Fatalf("task = %s/%s", got, r.taskReason(over))
	}
	if r.waiting(over) {
		t.Fatal("a failed task still waits")
	}
	attempt, _ := r.attemptOf(over)
	if st := r.attemptState(over); st == "active" || st == "creating" {
		t.Fatalf("attempt still %s", st)
	}
	ev := r.str(`SELECT array_to_string(evidence_refs, '|') FROM task_attempts WHERE id = $1`, attempt)
	if !strings.Contains(ev, "provision-aborted: no runtime capacity within 1m0s") {
		t.Fatalf("evidence = %q", ev)
	}
	// A second pass has nothing left to do.
	r.sweep(r.svc.RuntimeWaitSweep())
}

// A code child given an overflow slot runs on its profile's agent-overflow Agent; a base child,
// and an overflow child of a profile without one, on the workspace Agent.
func TestOverflowCodeChildRunsOnTheOverflowAgent(t *testing.T) {
	r := newRig(t)
	cfg := mustConfig(t)
	cfg.Kagent.WorkspaceClaudeOverflowAgent = "claude-workspace-overflow"
	reg, err := providers.NewRegistry(cfg.Kagent)
	if err != nil {
		t.Fatal(err)
	}
	r.svc.Providers = reg
	r.svc.Limits.MaxChildrenGlobal, r.svc.Limits.MaxChildrenOverflow = 1, 2
	r.svc.Limits.OverflowWait = time.Hour
	r.run()
	_, sup := r.supervisor()
	base := r.codeChild(sup, "feat-a")
	over := r.codeChild(sup, "feat-b")
	r.eventually("children provisioned", func() bool {
		return r.opState(base) == "completed" && r.opState(over) == "completed"
	})
	agent := func(id string) string {
		return r.str(`SELECT a.agent_name FROM task_attempts a JOIN tasks t ON t.current_attempt_id = a.id WHERE t.id = $1`, id)
	}
	if got := agent(base); got != cfg.Kagent.WorkspaceClaudeAgent {
		t.Fatalf("base child on %s", got)
	}
	if got := agent(over); got != "claude-workspace-overflow" {
		t.Fatalf("overflow child on %s", got)
	}
	if got := r.ws.agentFor(r.str(`SELECT workspace_request_id FROM task_attempts a JOIN tasks t ON t.current_attempt_id = a.id WHERE t.id = $1`, over)); got != "claude-workspace-overflow" {
		t.Fatalf("overflow workspace on %q", got)
	}
	if _, ok := reg.Profile("claude"); !ok {
		t.Fatal("no claude profile")
	}
	p, _ := reg.Profile("claude")
	if p.Agents[config.AgentOverflowRole].Name != "claude-workspace-overflow" {
		t.Fatalf("profile agents = %+v", p.Agents)
	}
}
