package merge_test

import (
	"context"
	"errors"
	"testing"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/merge"
)

// mainSession adds the owner's main thread, which is bound to no project.
func (e *env) mainSession() string {
	e.t.Helper()
	e.exec(`INSERT INTO sessions (id, user_id, title, description, prompt) VALUES ('m1', $1, 'main', 'd', 'p')`, owner)
	e.exec(`INSERT INTO agent_bindings (session_id, kind, role, kagent_session_id) VALUES ('m1', 'claude', 'main', 'km1')`)
	return "m1"
}

// The main thread merges by PR number in any of the owner's projects, naming the repository and
// leaving the head to be read: the proposal pins the head it read.
func TestMainMergesByRepositoryAndNumber(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.run()
	m := e.mainSession()
	ctx := context.Background()
	in := merge.PrepareInput{OwnerID: owner, Repository: "O/N", SessionID: m, PRNumber: 1, RequestID: "r1"}
	v, err := e.svc.Merge(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if v.State != dbstate.MergeMerged || v.Route != dbstate.RouteAuto || e.puts() != 1 {
		t.Fatalf("view = %+v, puts = %d", v, e.puts())
	}
	if v.HeadSHA != tip {
		t.Fatalf("pinned head = %s, want %s", v.HeadSHA, tip)
	}
	if again, err := e.svc.Merge(ctx, in); err != nil || again.ProposalID != v.ProposalID || e.puts() != 1 {
		t.Fatalf("replay = %+v, %v, puts %d", again, err, e.puts())
	}
	// Main reads its proposal's status with no project of its own.
	if _, err := e.svc.Status(ctx, owner, "", true, v.ProposalID, "r1"); err != nil {
		t.Fatalf("status: %v", err)
	}
	// A repository the owner has not imported is not found.
	if _, err := e.svc.Merge(ctx, merge.PrepareInput{OwnerID: owner, Repository: "x/y", SessionID: m, PRNumber: 1, RequestID: "r2"}); !errors.Is(err, merge.ErrNotFound) {
		t.Fatalf("unknown repository: %v", err)
	}
}

// Main's approval call passes the project gate (it is refused for want of consent, not authority).
func TestMainApprovalIsNotProjectScoped(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	m := e.mainSession()
	e.policy("approval")
	ctx := context.Background()
	v, err := e.svc.Merge(ctx, merge.PrepareInput{OwnerID: owner, Repository: "o/n", SessionID: m, PRNumber: 1, RequestID: "prep"})
	if err != nil || v.Status != "approval_required" {
		t.Fatalf("view = %+v, %v", v, err)
	}
	_, err = e.svc.MergeApproved(ctx, merge.ApprovedInput{OwnerID: owner, ActorMain: true, SessionID: m, ProposalID: v.ProposalID, RequestID: "call"})
	if refusalOf(err) != "approval_required" {
		t.Fatalf("approval: %v", err)
	}
}
