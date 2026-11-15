package tasks_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/github"
	"github.com/oldsj/mainloop/server/internal/tasks"
)

// fakeBranches is origin as GitHub shows it to a code task's handoff.
type fakeBranches struct {
	mu sync.Mutex
	// tips are the branches on origin and their heads; bases resolve the other refs.
	tips, bases map[string]string
	// err fails every read.
	err error
	// onRead runs before each branch read answers.
	onRead func()
	// gate holds the next branch read until it is closed; entered is told when one is waiting.
	gate, entered chan struct{}
	reads         int
}

func (f *fakeBranches) BranchTip(_ context.Context, repo github.Repo, branch string) (string, error) {
	f.mu.Lock()
	hook, gate, entered := f.onRead, f.gate, f.entered
	f.gate = nil
	f.mu.Unlock()
	if gate != nil {
		entered <- struct{}{}
		<-gate
	}
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.err != nil {
		return "", f.err
	}
	if sha, ok := f.tips[branch]; ok && repo.FullName() == "o/p1" {
		return sha, nil
	}
	return "", github.ErrNotFound
}

func (f *fakeBranches) CommitSHA(_ context.Context, repo github.Repo, ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	if sha, ok := f.bases[ref]; ok && repo.FullName() == "o/p1" {
		return sha, nil
	}
	return "", github.ErrNotFound
}

func (f *fakeBranches) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

// codeHandoffTask is a provisioned code child on branch feature, with b as origin.
func (r *rig) codeHandoffTask(b *fakeBranches) (id, attempt, session string) {
	r.t.Helper()
	r.svc.Branches, r.svc.CodeHandoff = b, true
	r.run()
	_, sup := r.supervisor()
	id = r.codeChild(sup, "feature")
	r.eventually("code child provisioned", func() bool { return r.opState(id) == "completed" })
	attempt, session = r.attemptOf(id)
	return id, attempt, session
}

func (r *rig) madeRefs() []string {
	r.ws.mu.Lock()
	defer r.ws.mu.Unlock()
	return append([]string(nil), r.ws.refs...)
}

// A code task's retry starts its successor from the task branch's head on origin, read only once
// the old attempt is fenced, and the successor carries on with the branch and its pull request.
func TestCodeRetryStartsFromThePushedBranch(t *testing.T) {
	r := newRig(t)
	head := strings.Repeat("a", 40)
	b := &fakeBranches{tips: map[string]string{"feature": head}}
	id, oldAttempt, oldSession := r.codeHandoffTask(b)
	var atRead []string
	b.mu.Lock()
	b.onRead = func() {
		atRead = append(atRead, r.str(`SELECT state FROM task_attempts WHERE id = $1`, oldAttempt))
	}
	b.mu.Unlock()
	r.created("pr-1", oldSession, 41, "2026-01-01T00:00:00Z")

	v := r.view(r.own, id)
	if !v.Actions["retry"].Available || !v.Actions["reassign"].Available {
		t.Fatalf("a running code task offers no handoff: %+v", v.Actions)
	}
	op, err := r.svc.Retry(context.Background(), r.own, action(id, v, "retry-1"))
	if err != nil {
		t.Fatal(err)
	}
	if op.State != "draining" {
		t.Fatalf("op = %+v", op)
	}
	r.handedOff(id)

	newAttempt, newSession := r.attemptOf(id)
	if len(atRead) == 0 || atRead[0] != "superseded" {
		t.Fatalf("old attempt at the branch read = %v, want superseded (fenced) first", atRead)
	}
	if got := r.str(`SELECT initial_ref FROM task_attempts WHERE id = $1`, newAttempt); got != head {
		t.Fatalf("successor initial_ref = %q, want the branch head", got)
	}
	if got := r.str(`SELECT checkpoint_ref FROM task_operations WHERE task_id = $1 AND kind = 'retry'`, id); got != "refs/heads/feature" {
		t.Fatalf("checkpoint_ref = %q", got)
	}
	if refs := r.madeRefs(); len(refs) != 2 || refs[1] != head {
		t.Fatalf("workspace refs = %v, want the successor's at %s", refs, head)
	}
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND state = 'superseded' AND successor_id = $2`, oldAttempt, newAttempt); n != 1 {
		t.Fatal("the old attempt is not superseded by the new one")
	}
	if n := r.count(`SELECT count(*) FROM task_attempts o, task_attempts s WHERE o.id = $1 AND s.id = $2
		AND s.writer_generation > o.writer_generation`, oldAttempt, newAttempt); n != 1 {
		t.Fatal("the successor does not hold the branch after the old attempt")
	}
	brief := r.firstMessage(newSession)
	for _, want := range []string{"## Continuation", "This task was retried.",
		"Your workspace starts at " + head + ", the head of branch `feature` on origin: it has what attempt 1 pushed"} {
		if !strings.Contains(brief, want) {
			t.Fatalf("brief lacks %q:\n%s", want, brief)
		}
	}
	if strings.Contains(brief, "Child tasks") {
		t.Fatalf("a code task's brief speaks of children:\n%s", brief)
	}

	// The successor's projection keeps the pull request the old attempt opened on the branch.
	r.project()
	if got := r.lastExpected(); got != 41 {
		t.Fatalf("projection expected PR = %d, want the old attempt's 41", got)
	}
}

// A branch that was never pushed starts the successor from the task's base, pinned to its commit;
// a reassign moves the code task to the target provider.
func TestCodeReassignOfAnUnpushedBranchStartsFromTheBase(t *testing.T) {
	r := newRig(t)
	base := strings.Repeat("b", 40)
	id, _, _ := r.codeHandoffTask(&fakeBranches{bases: map[string]string{"HEAD": base}})
	in := action(id, r.view(r.own, id), "reassign-1")
	in.TargetProfileID = "codex"
	if _, err := r.svc.Reassign(context.Background(), r.own, in); err != nil {
		t.Fatal(err)
	}
	r.handedOff(id)
	newAttempt, newSession := r.attemptOf(id)
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND profile_id = 'codex' AND initial_ref = $2`, newAttempt, base); n != 1 {
		t.Fatal("the successor is not on codex at the base commit")
	}
	if got := r.str(`SELECT checkpoint_ref FROM task_operations WHERE task_id = $1 AND kind = 'reassign'`, id); got != "HEAD" {
		t.Fatalf("checkpoint_ref = %q, want the base", got)
	}
	brief := r.firstMessage(newSession)
	for _, want := range []string{"This task was reassigned from provider claude.",
		"Attempt 1 never pushed branch `feature`, so none of its work is in your workspace: it starts at " + base +
			", the head of the repository's default branch."} {
		if !strings.Contains(brief, want) {
			t.Fatalf("brief lacks %q:\n%s", want, brief)
		}
	}
}

// When origin cannot be read, the successor never starts from the base instead: the read is tried
// again until the provisioning window ends, then the task fails with no second workspace.
func TestCodeRetryWithoutAReadableOriginFails(t *testing.T) {
	r := newRig(t)
	r.svc.Limits.ProvisionWindow = 2 * time.Second
	b := &fakeBranches{} // neither the branch nor the base: what lost access looks like
	id, _, _ := r.codeHandoffTask(b)
	if _, err := r.svc.Retry(context.Background(), r.own, action(id, r.view(r.own, id), "retry-1")); err != nil {
		t.Fatal(err)
	}
	r.eventually("task failed", func() bool {
		r.sweep(r.svc.SettleSweep())
		return r.taskStatus(id) == "failed" &&
			r.count(`SELECT count(*) FROM task_attempts WHERE task_id = $1 AND state IN ('creating', 'active', 'draining', 'fenced')`, id) == 0
	})
	if r.ws.madeCount() != 1 {
		t.Fatalf("workspaces made = %d, want only the first attempt's", r.ws.madeCount())
	}
	if b.readCount() < 2 {
		t.Fatalf("branch reads = %d, want the read retried", b.readCount())
	}
	newAttempt, _ := r.attemptOf(id)
	if n := r.count(`SELECT count(*) FROM task_attempts WHERE id = $1 AND number = 2 AND initial_ref IS NULL
		AND array_to_string(evidence_refs, ' ') LIKE '%branch head could not be read in time%'`, newAttempt); n != 1 {
		t.Fatal("the successor started somewhere, or does not say why it failed")
	}
	if got := r.str(`SELECT state || '/' || coalesce(reason, '') FROM task_operations WHERE task_id = $1 AND kind = 'retry'`, id); got != "blocked/provisioning_unavailable" {
		t.Fatalf("op = %s", got)
	}
}

// A cancel while the branch head is being read wins: the read's answer is not recorded, the
// successor is cancelled and it never gets a workspace.
func TestCancelDuringTheBranchReadWins(t *testing.T) {
	r := newRig(t)
	b := &fakeBranches{tips: map[string]string{"feature": strings.Repeat("a", 40)}}
	id, _, _ := r.codeHandoffTask(b)
	gate, entered := make(chan struct{}), make(chan struct{}, 1)
	b.mu.Lock()
	b.gate, b.entered = gate, entered
	b.mu.Unlock()
	if _, err := r.svc.Retry(context.Background(), r.own, action(id, r.view(r.own, id), "retry-1")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		t.Fatal("the branch was never read")
	}
	for try := 0; ; try++ {
		_, err := r.svc.Cancel(context.Background(), r.own, action(id, r.view(r.own, id), fmt.Sprintf("cancel-%d", try)))
		var e *tasks.Error
		if err == nil {
			break
		}
		if !errors.As(err, &e) || (e.Reason != tasks.ReasonStaleVersion && e.Reason != tasks.ReasonStaleAttempt) || try == 20 {
			t.Fatal(err)
		}
	}
	close(gate)
	r.eventually("task cancelled with nothing running", func() bool {
		r.sweep(r.svc.SettleSweep())
		return r.taskStatus(id) == "cancelled" &&
			r.count(`SELECT count(*) FROM task_attempts WHERE task_id = $1 AND state IN ('creating', 'active', 'draining', 'fenced')`, id) == 0 &&
			r.count(`SELECT count(*) FROM task_operations WHERE task_id = $1 AND state NOT IN ('completed', 'blocked')`, id) == 0
	})
	if r.ws.madeCount() != 1 {
		t.Fatalf("workspaces made = %d, want only the first attempt's", r.ws.madeCount())
	}
	if got := r.str(`SELECT string_agg(number || ':' || state || ':' || coalesce(initial_ref, '-'), ' ' ORDER BY number) FROM task_attempts WHERE task_id = $1`, id); got != "1:superseded: 2:cancelled:-" {
		t.Fatalf("attempts = %s, want the successor cancelled without a start", got)
	}
}
