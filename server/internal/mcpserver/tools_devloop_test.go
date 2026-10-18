package mcpserver_test

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/agentauth/authtest"
	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/devloop"
	"github.com/oldsj/mainloop/server/internal/mcpserver"
)

// TestDevLoopToolsAreHiddenWithTheFlagOff proves the behavior that fails without the change: an
// operator who has not set MAINLOOP_DEVLOOP_ENABLED=true gets no devloop tools in discovery, and
// a call to one answers that the tool does not exist, never reaching the handler.
func TestDevLoopToolsAreHiddenWithTheFlagOff(t *testing.T) {
	h := buildHarness(t, true, true, func(srv *mcpserver.Server, pool *pgxpool.Pool) {
		srv.DevLease = &devloop.LeaseService{Pool: pool}
		srv.DevOps = &devloop.OperationsService{Pool: pool}
		// Policy.DevLoop left false: the flag is off.
	})
	a := seedDevloopAgent(t, h)

	names := h.list(t, ordinary, a.Token)
	for _, tool := range []string{"dev_lease_acquire", "dev_lease_release", "dev_sync", "dev_status"} {
		if slices.Contains(names, tool) {
			t.Errorf("tools/list with the flag off includes %s", tool)
		}
	}

	_, r := h.call(ordinary, a.Token, "dev_lease_acquire", map[string]any{})
	if r.Error == nil {
		t.Fatalf("dev_lease_acquire with the flag off = %+v, want a tool-not-found error", r)
	}
}

// TestDevLoopToolsRefuseAnotherProject proves the behavior that fails without the change: a task
// agent bound to a project other than oldsj/mainloop is refused even though the flag is on and its
// role and grant would otherwise qualify.
func TestDevLoopToolsRefuseAnotherProject(t *testing.T) {
	h := newDevloopHarness(t)
	ctx := context.Background()
	if _, err := h.env.Pool.Exec(ctx, `INSERT INTO projects (id, user_id, owner, name, full_name, html_url, default_branch)
		VALUES ('other-p', 'owner', 'someone', 'else', 'someone/else', 'https://x', 'main') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	a := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	if _, err := h.env.Pool.Exec(ctx, `UPDATE sessions SET project_id = 'other-p' WHERE id = $1`, a.SessionID); err != nil {
		t.Fatal(err)
	}

	// Still listed (the flag is on; project is a handler-level check), but every call is refused.
	if names := h.list(t, ordinary, a.Token); !slices.Contains(names, "dev_lease_acquire") {
		t.Fatalf("tools/list = %v, want dev_lease_acquire listed (visibility does not vary by project)", names)
	}
	_, r := h.call(ordinary, a.Token, "dev_lease_acquire", map[string]any{})
	if !r.Result.IsError || r.text() != "[forbidden] devloop tools are limited to oldsj/mainloop" {
		t.Fatalf("dev_lease_acquire for another project = error %v, %q, want the forbidden refusal", r.Result.IsError, r.text())
	}

	// A session with no project at all is refused the same way.
	b := h.env.Seed(t, authtest.Agent{Role: dbstate.RoleAgent, Grant: dbstate.GrantWorkspace})
	_, r2 := h.call(ordinary, b.Token, "dev_sync", map[string]any{"commit_sha": prTip})
	if !r2.Result.IsError || r2.text() != "[forbidden] this session has no project" {
		t.Fatalf("dev_sync with no project = error %v, %q, want the no-project refusal", r2.Result.IsError, r2.text())
	}
}

// TestDevSyncRequiresTheLease proves the behavior that fails without the change: dev_sync from a
// session that never acquired the lease is refused before any operation row is written.
func TestDevSyncRequiresTheLease(t *testing.T) {
	h := newDevloopHarness(t)
	a := seedDevloopAgent(t, h)

	_, r := h.call(ordinary, a.Token, "dev_sync", map[string]any{"commit_sha": prTip})
	if !r.Result.IsError || r.text() != "[lease_required] acquire the mainloop-dev lease with dev_lease_acquire first" {
		t.Fatalf("dev_sync without the lease = error %v, %q, want the lease_required refusal", r.Result.IsError, r.text())
	}
	var n int
	if err := h.env.Pool.QueryRow(context.Background(), `SELECT count(*) FROM devloop_operations`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("devloop_operations rows = %d, %v, want none", n, err)
	}
}
