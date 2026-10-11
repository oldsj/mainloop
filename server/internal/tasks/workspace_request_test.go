package tasks_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/tasks"
	"github.com/oldsj/mainloop/server/internal/workspaces"
)

// holdWorkspaceCreate stops a call before it writes its workspace, without holding a database
// transaction. A nil answer commits after release; an error returns without creating anything.
func holdWorkspaceCreate(r *rig, answer error) (release func(), entered <-chan struct{}) {
	r.t.Helper()
	gate, started := make(chan struct{}), make(chan struct{}, 1)
	r.ws.mu.Lock()
	r.ws.gate, r.ws.entered, r.ws.gateErr = gate, started, answer
	r.ws.mu.Unlock()
	var once sync.Once
	release = func() { once.Do(func() { close(gate) }) }
	r.t.Cleanup(release)
	return release, started
}

func waitWorkspaceCreate(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		t.Fatal("workspace create never started")
	}
}

func ownerWorkspace(r *rig) string {
	r.t.Helper()
	// Use a separate runtime so the owner's create bypasses the task call's gate and faults.
	manual := &fakeWorkspaces{pool: r.pool}
	lc, err := manual.Create(context.Background(), api.CreateWorkspaceRequest{
		ProjectId: ptr("p1"), Branch: ptr("feat"), AgentKind: ptr("claude"),
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return lc.WorkspaceId
}

func assertOwnerWorkspaceUntouched(r *rig, id string) {
	r.t.Helper()
	if r.count(`SELECT count(*) FROM workspaces WHERE id = $1`, id) != 1 ||
		r.str(`SELECT role FROM agent_bindings WHERE session_id = $1`, id) != string(dbstate.RoleAgent) ||
		r.count(`SELECT count(*) FROM session_messages WHERE session_id = $1`, id) != 0 ||
		r.count(`SELECT count(*) FROM task_attempts WHERE workspace_id = $1 OR session_id = $1`, id) != 0 {
		r.t.Fatal("the owner's workspace was adopted, briefed, or deleted")
	}
	r.ws.mu.Lock()
	defer r.ws.mu.Unlock()
	for _, deleted := range r.ws.removed {
		if deleted == id {
			r.t.Fatal("the owner's workspace was passed to Delete")
		}
	}
}

// G8b P2-1 adopt probe: a failed create followed by replay must create the task's own workspace,
// even when the owner made one on the same project/branch during the provisioning window.
func TestProbeReplayAdoptsOwnerWorkspace(t *testing.T) {
	r := newRig(t)
	r.run()
	_, sup := r.supervisor()
	release, entered := holdWorkspaceCreate(r, errors.New("connection lost before commit"))
	child := r.codeChild(sup, "feat")
	waitWorkspaceCreate(t, entered)
	attempt, _ := r.attemptOf(child)
	key := r.str(`SELECT workspace_request_id FROM task_attempts WHERE id = $1`, attempt)
	if key != tasks.WorkspaceRequestID(attempt) {
		t.Fatalf("durable workspace request id = %q", key)
	}
	manual := ownerWorkspace(r)
	release()
	r.eventually("child provisioned", func() bool { return r.opState(child) == "completed" })
	id := r.str(`SELECT workspace_id FROM task_attempts WHERE id = $1`, attempt)
	if id == manual || r.str(`SELECT request_id FROM workspaces WHERE id = $1`, id) != key || r.ws.madeCount() != 1 {
		t.Fatal("replay did not use the attempt's keyed workspace")
	}
	assertOwnerWorkspaceUntouched(r, manual)
	r.ws.mu.Lock()
	defer r.ws.mu.Unlock()
	if len(r.ws.requests) != 2 || r.ws.requests[0] != key || r.ws.requests[1] != key {
		t.Fatalf("create request ids = %v", r.ws.requests)
	}
}

// G8b P2-1 delete probe: a cancel while nothing has committed must leave the owner's workspace
// alone and keep resolving the task's own create until the held call has definitively failed.
func TestProbeCancelDeletesOwnerWorkspace(t *testing.T) {
	r := newRig(t)
	r.run()
	_, sup := r.supervisor()
	release, entered := holdWorkspaceCreate(r, &workspaces.RejectedError{Detail: "refused"})
	child := r.codeChild(sup, "feat")
	waitWorkspaceCreate(t, entered)
	manual := ownerWorkspace(r)
	if _, err := r.svc.Cancel(context.Background(), r.own, action(child, r.view(r.own, child), "cancel")); err != nil {
		t.Fatal(err)
	}
	r.eventually("cancel completed", func() bool {
		return r.str(`SELECT state FROM task_operations WHERE task_id = $1 AND kind = 'cancel'`, child) == "completed"
	})
	for range 3 {
		r.sweep(r.svc.SettleSweep())
	}
	if r.count(`SELECT count(*) FROM task_attempts WHERE task_id = $1 AND workspace_cleanup_confirmed`, child) != 0 {
		t.Fatal("an in-flight create was declared absent")
	}
	assertOwnerWorkspaceUntouched(r, manual)
	release()
	r.eventually("create returned", func() bool {
		return r.count(`SELECT workspace_creates_inflight FROM task_attempts WHERE task_id = $1`, child) == 0
	})
	r.sweep(r.svc.SettleSweep())
	if r.count(`SELECT count(*) FROM task_attempts WHERE task_id = $1 AND workspace_cleanup_confirmed`, child) != 1 {
		t.Fatal("the definitively failed create was not resolved")
	}
	assertOwnerWorkspaceUntouched(r, manual)
}

// G8b P2-2: create commits only after cancel has finished. Exercise the live return path and a
// lost reply after commit, where only a restarted service's settle sweep can find the workspace.
func TestProbeCancelRacesWorkspaceCreate(t *testing.T) {
	for _, lostReply := range []bool{false, true} {
		name := "returned"
		if lostReply {
			name = "lost-reply"
		}
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			stop := r.run()
			_, sup := r.supervisor()
			release, entered := holdWorkspaceCreate(r, nil)
			if lostReply {
				r.ws.mu.Lock()
				r.ws.crash = 1
				r.ws.mu.Unlock()
			}
			child := r.codeChild(sup, "feat")
			waitWorkspaceCreate(t, entered)
			manual := ownerWorkspace(r)
			if _, err := r.svc.Cancel(context.Background(), r.own, action(child, r.view(r.own, child), "cancel")); err != nil {
				t.Fatal(err)
			}
			r.eventually("cancel completed before commit", func() bool {
				return r.str(`SELECT state FROM task_operations WHERE task_id = $1 AND kind = 'cancel'`, child) == "completed"
			})
			if r.opState(child) != "blocked" || r.attemptState(child) != "cancelled" {
				t.Fatal("cancel did not end the create and attempt")
			}
			for range 3 {
				r.sweep(r.svc.SettleSweep())
			}
			if r.count(`SELECT count(*) FROM task_attempts WHERE task_id = $1 AND workspace_cleanup_confirmed`, child) != 0 {
				t.Fatal("cleanup stopped before the create returned")
			}
			release()
			r.eventually("late create returned", func() bool {
				return r.ws.madeCount() == 1 && r.count(`SELECT workspace_creates_inflight FROM task_attempts WHERE task_id = $1`, child) == 0
			})
			if !lostReply && r.count(`SELECT count(*) FROM workspaces`) != 1 {
				t.Fatal("the returning create did not delete the workspace after attachment was refused")
			}
			stop()
			// Fresh service state; there is no in-memory create handler or operation to retry.
			restarted := *r.svc
			if lostReply {
				r.ws.mu.Lock()
				r.ws.deleteErr = &workspaces.UnconfirmedError{Detail: "delete still in flight"}
				r.ws.mu.Unlock()
				var unconfirmed *workspaces.UnconfirmedError
				if err := restarted.SettleSweep().Run(context.Background()); !errors.As(err, &unconfirmed) {
					t.Fatalf("unconfirmed delete = %v", err)
				}
				if r.count(`SELECT count(*) FROM workspaces`) != 2 ||
					r.count(`SELECT count(*) FROM task_attempts WHERE task_id = $1 AND workspace_cleanup_confirmed`, child) != 0 {
					t.Fatal("cleanup stopped after an unconfirmed delete")
				}
				r.ws.mu.Lock()
				r.ws.deleteErr = nil
				r.ws.mu.Unlock()
				// The unresolved deletion is paced durably; model its next scheduled look.
				r.exec(`UPDATE task_attempts SET workspace_cleanup_after = now() WHERE task_id = $1`, child)
			}
			r.sweep(restarted.SettleSweep())
			if r.count(`SELECT count(*) FROM workspaces`) != 1 ||
				r.count(`SELECT count(*) FROM task_attempts WHERE task_id = $1 AND workspace_cleanup_confirmed`, child) != 1 {
				t.Fatal("late workspace was not deleted and cleanup confirmed")
			}
			if r.str(`SELECT workspace_id FROM task_attempts WHERE task_id = $1`, child) != "" {
				t.Fatal("cancelled attempt attached the late workspace")
			}
			if r.count(`SELECT count(*) FROM task_attempts WHERE task_id = $1 AND capacity_held`, child) != 0 ||
				r.count(`SELECT count(*) FROM workspace_writer_claims WHERE held`) != 0 {
				t.Fatal("cancel retained capacity or the writer claim")
			}
			assertOwnerWorkspaceUntouched(r, manual)
			r.ws.mu.Lock()
			removed := len(r.ws.removed)
			r.ws.mu.Unlock()
			r.sweep(restarted.SettleSweep())
			r.ws.mu.Lock()
			defer r.ws.mu.Unlock()
			if len(r.ws.removed) != removed {
				t.Fatal("resolved cleanup repeated the delete")
			}
		})
	}
}

// A reclaimed operation creates the keyed workspace while its old call is still held. Cancel
// deletes that first workspace, then the old call arrives after deletion. Deleting once must not
// stop cleanup while another reservation can still commit under the same request ID.
func TestCancelledCreateCleansLateReplayAfterFirstWorkspaceDeleted(t *testing.T) {
	r := newRig(t)
	r.ws.unready = true
	r.run()
	_, sup := r.supervisor()
	release, entered := holdWorkspaceCreate(r, nil)
	child := r.codeChild(sup, "feat")
	waitWorkspaceCreate(t, entered)
	r.exec(`UPDATE task_operations SET lease_owner = NULL, lease_until = NULL WHERE task_id = $1 AND kind = 'create'`, child)
	r.eventually("reclaimed create attached its workspace", func() bool {
		return r.ws.madeCount() == 1 && r.str(`SELECT workspace_id FROM task_attempts WHERE task_id = $1`, child) != "" &&
			r.count(`SELECT workspace_creates_inflight FROM task_attempts WHERE task_id = $1`, child) == 1
	})
	manual := ownerWorkspace(r)
	if _, err := r.svc.Cancel(context.Background(), r.own, action(child, r.view(r.own, child), "cancel")); err != nil {
		t.Fatal(err)
	}
	r.eventually("cancel deleted the first workspace", func() bool {
		return r.attemptState(child) == "cancelled" && r.count(`SELECT count(*) FROM workspaces`) == 1
	})
	for range 3 {
		r.sweep(r.svc.SettleSweep())
	}
	if r.count(`SELECT count(*) FROM task_attempts WHERE task_id = $1 AND workspace_cleanup_confirmed`, child) != 0 {
		t.Fatal("first deletion resolved cleanup while the old create could still commit")
	}
	release()
	r.eventually("old create returned", func() bool {
		return r.ws.madeCount() == 2 && r.count(`SELECT workspace_creates_inflight FROM task_attempts WHERE task_id = $1`, child) == 0
	})
	r.sweep(r.svc.SettleSweep())
	if r.count(`SELECT count(*) FROM workspaces`) != 1 ||
		r.count(`SELECT count(*) FROM task_attempts WHERE task_id = $1 AND workspace_cleanup_confirmed`, child) != 1 {
		t.Fatal("late replay leaked a workspace")
	}
	assertOwnerWorkspaceUntouched(r, manual)
	r.ws.mu.Lock()
	defer r.ws.mu.Unlock()
	if len(r.ws.removed) != 2 {
		t.Fatalf("deleted workspaces = %v", r.ws.removed)
	}
}
