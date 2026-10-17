package devloop_test

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbstate"
	"github.com/oldsj/mainloop/server/internal/devloop"
	"github.com/oldsj/mainloop/server/internal/devloop/devlooptest"
	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

// runOperations starts the devloop-operations loop against d's pool and stops it on cleanup. A
// short Lease and Interval keep the test fast.
func runOperations(t *testing.T, s *devloop.OperationsService, pool *testdb.DB) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil))
	r := reconcile.NewRunner(pool.Pool, log, nil)
	loop := s.Loop()
	loop.Interval, loop.Lease = 20*time.Millisecond, time.Second
	r.Add(loop)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

func newOperationsService(d *testdb.DB, pods devloop.PodClient, seed devloop.GitSeeder, health devloop.HealthChecker) *devloop.OperationsService {
	return &devloop.OperationsService{
		Pool: d.Pool, Pods: pods, Seed: seed, Health: health,
		HealthTimeout: 2 * time.Second, UncertainRetryDelay: 20 * time.Millisecond,
	}
}

// TestSyncStartIsIdempotent proves the behavior that fails without the change: two Start calls
// with the same idempotency key (an agent's retried dev_sync) return the same durable row and the
// work runs once, not twice.
func TestSyncStartIsIdempotent(t *testing.T) {
	d := testdb.New(t)
	pods := devlooptest.New()
	pods.BySelector["app=mainloop-backend"] = []devloop.PodStatus{{Name: "backend-0"}}
	pods.BySelector["app=mainloop-frontend"] = []devloop.PodStatus{{Name: "frontend-0"}}
	sha, pack := scratchRepo(t)
	seed := &devlooptest.Seeder{Payload: pack}
	health := &devlooptest.Health{}
	svc := newOperationsService(d, pods, seed, health)
	runOperations(t, svc, d)

	ctx := context.Background()
	first, err := svc.Start(ctx, dbstate.DevloopSync, "session-a", "proj-1", "key-1",
		devloop.SyncParams{Repo: "o/n", CommitSHA: sha})
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Start(ctx, dbstate.DevloopSync, "session-a", "proj-1", "key-1",
		devloop.SyncParams{Repo: "o/n", CommitSHA: sha})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("two Start calls with the same idempotency key made different rows: %s vs %s", first.ID, second.ID)
	}
	row, err := svc.Wait(ctx, first.ID, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != dbstate.DevloopSucceeded {
		t.Fatalf("sync state = %s, detail %v, want succeeded", row.State, row.Detail)
	}
	if n := seed.Calls; n != 1 {
		t.Errorf("seeder was called %d times, want exactly 1 (idempotent, not replayed)", n)
	}
	if n := pods.CopyCalls["backend"]; n != 1 {
		t.Errorf("backend was copied to %d times, want exactly 1", n)
	}
}

// TestSyncRetriesAnUncertainCopyToASuccessfulOutcome proves the behavior that fails without the
// change: when a copy's outcome is uncertain (the exec stream was cut), the row moves to
// `unknown` and the loop retries rather than failing the sync outright. The retry is safe without
// first observing remote state, unlike the reconcile package's general rule, because overwriting
// the sync directory with the same commit's tree is idempotent (see docs/specs/devloop.md).
func TestSyncRetriesAnUncertainCopyToASuccessfulOutcome(t *testing.T) {
	d := testdb.New(t)
	pods := devlooptest.New()
	pods.BySelector["app=mainloop-backend"] = []devloop.PodStatus{{Name: "backend-0"}}
	pods.BySelector["app=mainloop-frontend"] = []devloop.PodStatus{{Name: "frontend-0"}}
	// The frontend copy is cut once, then succeeds on retry.
	pods.QueueCopyResult("frontend", devlooptest.ErrStreamReset)
	sha, pack := scratchRepo(t)
	seed := &devlooptest.Seeder{Payload: pack}
	health := &devlooptest.Health{}
	svc := newOperationsService(d, pods, seed, health)
	runOperations(t, svc, d)

	ctx := context.Background()
	row, err := svc.Start(ctx, dbstate.DevloopSync, "session-a", "proj-1", "key-2",
		devloop.SyncParams{Repo: "o/n", CommitSHA: sha})
	if err != nil {
		t.Fatal(err)
	}
	final, err := svc.Wait(ctx, row.ID, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != dbstate.DevloopSucceeded {
		t.Fatalf("sync state = %s, detail %v, want succeeded after the retry", final.State, final.Detail)
	}
	if n := pods.CopyCalls["frontend"]; n != 2 {
		t.Errorf("frontend was copied to %d times, want exactly 2 (one cut, one retry)", n)
	}
	// A retried attempt re-fetches (a safe, read-only GitHub call) but must never push or create a
	// second pull request: there is nothing of that kind on this path to assert against here, but
	// FetchArchive and CopyTree are the only side effects runSync performs, and the backend/mcp/git
	// copies (never queued a failure) ran exactly once each attempt.
	if n := pods.CopyCalls["backend"]; n != 2 {
		t.Errorf("backend was copied to %d times, want exactly 2: the whole sync retries, not only the cut container", n)
	}
}

func TestStatusReportsPods(t *testing.T) {
	d := testdb.New(t)
	pods := devlooptest.New()
	pods.AllPods = []devloop.PodStatus{{Name: "backend-0", Phase: "Running", Ready: true}}
	svc := newOperationsService(d, pods, nil, nil)
	runOperations(t, svc, d)
	ctx := context.Background()
	row, err := svc.Start(ctx, dbstate.DevloopStatus, "session-a", "proj-1", "key-status", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	final, err := svc.Wait(ctx, row.ID, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != dbstate.DevloopSucceeded {
		t.Fatalf("status state = %s, detail %v, want succeeded", final.State, final.Detail)
	}
}
