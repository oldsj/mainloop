package preview

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestLongHTTPRequestHoldsALeaseUntilItEnds(t *testing.T) {
	e := newEnv(t)
	e.proxy.HTTPLeaseWait = 20 * time.Millisecond
	e.proxy.RenewEvery = 10 * time.Millisecond
	release := make(chan struct{})
	started := make(chan struct{})
	e.devHandler.Store(http.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "done")
	})))
	done := make(chan int, 1)
	go func() { done <- e.do("GET", wsHost, "/slow", nil, nil).Code }()
	<-started
	e.waitLeases(1)
	close(release)
	if code := <-done; code != http.StatusOK {
		t.Fatalf("got %d", code)
	}
	e.waitLeases(0)
	var rows int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM preview_leases`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("lease rows left after release: %d %v", rows, err)
	}
}

func TestShortRequestWritesNoLease(t *testing.T) {
	e := newEnv(t)
	e.do("GET", wsHost, "/", nil, nil)
	var rows int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM preview_leases`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rows = %d %v", rows, err)
	}
}

func TestLeaseIsRenewedWhileHeld(t *testing.T) {
	e := newEnv(t)
	e.proxy.LeaseTTL = 300 * time.Millisecond
	e.proxy.RenewEvery = 20 * time.Millisecond
	l := e.proxy.holdLease("ws1", true)
	t.Cleanup(l.release)
	e.waitLeases(1)
	time.Sleep(600 * time.Millisecond) // two TTLs: only renewal keeps it alive
	if n := e.activeLeases(); n != 1 {
		t.Fatalf("leases after two TTLs = %d, want 1 (renewed)", n)
	}
}

// A holder that dies stops renewing; its lease lapses by itself and the sweep prunes the row.
func TestAbandonedLeaseLapsesAndIsPruned(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `INSERT INTO preview_leases (id, workspace_id, holder, expires_at)
		VALUES ('dead', 'ws1', 'crashed', now() - interval '10 minutes'),
		       ('live', 'ws1', 'other', now() + interval '1 minute')`); err != nil {
		t.Fatal(err)
	}
	if n := e.activeLeases(); n != 1 {
		t.Fatalf("active leases = %d, want only the unexpired one", n)
	}
	if err := e.proxy.KeepAwake(ctx); err != nil {
		t.Fatal(err)
	}
	var ids []string
	rows, err := e.pool.Query(ctx, `SELECT id FROM preview_leases ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if len(ids) != 1 || ids[0] != "live" {
		t.Fatalf("rows after sweep = %v, want [live]", ids)
	}
}

func TestKeepAwakeSweepTouchesLeasedWorkspacesOnly(t *testing.T) {
	e := newEnv(t)
	e.seed("ws3", owner, "kagent-sess-3", `[]`)
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO preview_leases (id, workspace_id, holder, expires_at)
		VALUES ('a', 'ws1', 'h', now() + interval '1 minute'), ('b', 'ws1', 'h', now() + interval '1 minute'),
		       ('c', 'ws3', 'h', now() - interval '1 second')`); err != nil {
		t.Fatal(err)
	}
	if err := e.proxy.KeepAwake(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.waker.mu.Lock()
	defer e.waker.mu.Unlock()
	if len(e.waker.touches) != 1 || e.waker.touches[0] != "ws1" {
		t.Fatalf("touched %v, want [ws1] once", e.waker.touches)
	}
}

func TestLeasesGoWithTheWorkspace(t *testing.T) {
	e := newEnv(t)
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO preview_leases (id, workspace_id, holder, expires_at)
		VALUES ('a', 'ws1', 'h', now() + interval '1 minute')`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM sessions WHERE id = 'ws1'`); err != nil {
		t.Fatal(err)
	}
	if n := e.activeLeases(); n != 0 {
		t.Fatalf("leases after the workspace was deleted = %d", n)
	}
}
