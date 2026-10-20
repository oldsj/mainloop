package tasks_test

import (
	"context"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// A late handler with a live context sees attachment refused because the reclaimed operation has
// already activated the attempt. That is not a stopped attempt and grants no cleanup authority.
func TestLateWorkspaceReplayDoesNotDeleteAnActiveAttempt(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	stop := r.run()
	_, sup := r.supervisor()
	stop()
	release, entered := holdWorkspaceCreate(r, nil)
	child := r.codeChild(sup, "feat")
	loop := r.svc.OpLoop()
	claim := func() reconcile.Claim {
		t.Helper()
		rows, err := loop.Table.Claim(context.Background(), r.pool, "late-create-probe", time.Minute, 1)
		if err != nil || len(rows) != 1 {
			t.Fatalf("claim = %v, %v", rows, err)
		}
		return rows[0]
	}
	if _, err := loop.Handle(context.Background(), claim()); err != nil {
		t.Fatal(err)
	}
	c := claim()
	returned := make(chan error, 1)
	go func() {
		_, err := loop.Handle(context.Background(), c)
		returned <- err
	}()
	t.Cleanup(func() {
		release()
		select {
		case err := <-returned:
			if err != nil {
				t.Errorf("late create: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("late create did not return")
		}
	})
	waitWorkspaceCreate(t, entered)
	r.exec(`UPDATE task_operations SET lease_owner = NULL, lease_until = NULL WHERE task_id = $1 AND kind = 'create'`, child)
	r.run()
	r.eventually("reclaimed create briefed its active attempt", func() bool { return r.opState(child) == "completed" })
	_, binding := r.attemptOf(child)
	release()
	// Do not consume the completion channel here: cleanup also waits for it.
	r.eventually("late call finished", func() bool {
		return r.count(`SELECT workspace_creates_inflight FROM task_attempts WHERE task_id = $1`, child) == 0
	})
	if r.attemptState(child) != "active" || r.taskStatus(child) != "running" ||
		r.count(`SELECT count(*) FROM workspaces WHERE id = $1`, binding) != 1 ||
		r.count(`SELECT count(*) FROM session_messages WHERE session_id = $1`, binding) != 1 {
		t.Fatal("late replay changed the active attempt or its workspace")
	}
	r.ws.mu.Lock()
	defer r.ws.mu.Unlock()
	if len(r.ws.made) != 1 || len(r.ws.removed) != 0 {
		t.Fatal("late replay created or deleted another workspace")
	}
}
