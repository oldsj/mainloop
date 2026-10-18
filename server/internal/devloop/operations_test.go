package devloop_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
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

func newOperationsService(d *testdb.DB, pods devloop.PodClient, seed devloop.GitSeeder) *devloop.OperationsService {
	return &devloop.OperationsService{
		Pool: d.Pool, Pods: pods, Seed: seed,
		HealthTimeout: 5 * time.Second, SettleDelay: time.Millisecond, UncertainRetryDelay: 20 * time.Millisecond,
	}
}

// holdLease makes session the lease holder, as a task agent's dev_lease_acquire would before it
// calls dev_sync.
func holdLease(t *testing.T, d *testdb.DB, session string) {
	t.Helper()
	info, err := (&devloop.LeaseService{Pool: d.Pool}).Acquire(context.Background(), session)
	if err != nil || info.Status != devloop.LeaseAcquired {
		t.Fatalf("Acquire(%s) = %+v, %v, want acquired", session, info, err)
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
	svc := newOperationsService(d, pods, seed)
	holdLease(t, d, "session-a")
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
	svc := newOperationsService(d, pods, seed)
	holdLease(t, d, "session-a")
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
	svc := newOperationsService(d, pods, nil)
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

func startSync(t *testing.T, svc *devloop.OperationsService, session, key, sha string) dbq.DevloopOperation {
	t.Helper()
	row, err := svc.Start(context.Background(), dbstate.DevloopSync, session, "proj-1", key, devloop.SyncParams{Repo: "o/n", CommitSHA: sha})
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func waitSync(t *testing.T, svc *devloop.OperationsService, id string) dbq.DevloopOperation {
	t.Helper()
	row, err := svc.Wait(context.Background(), id, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func syncPods() *devlooptest.Pods {
	pods := devlooptest.New()
	pods.BySelector["app=mainloop-backend"] = []devloop.PodStatus{{Name: "backend-0"}}
	pods.BySelector["app=mainloop-frontend"] = []devloop.PodStatus{{Name: "frontend-0"}}
	return pods
}

// TestSyncRunsAgainAfterTheEarlierRowFinished proves the behavior that fails without the change:
// the same session syncing the same commit a second time, after the first row succeeded, is
// copied again instead of being handed the stale finished row. A failed sync is retried the same
// way.
func TestSyncRunsAgainAfterTheEarlierRowFinished(t *testing.T) {
	d := testdb.New(t)
	pods := syncPods()
	sha, pack := scratchRepo(t)
	seed := &devlooptest.Seeder{Payload: pack}
	svc := newOperationsService(d, pods, seed)
	holdLease(t, d, "session-a")
	runOperations(t, svc, d)

	first := waitSync(t, svc, startSync(t, svc, "session-a", "key-again", sha).ID)
	if first.State != dbstate.DevloopSucceeded {
		t.Fatalf("first sync = %s (%v), want succeeded", first.State, first.Detail)
	}
	second := waitSync(t, svc, startSync(t, svc, "session-a", "key-again", sha).ID)
	if second.State != dbstate.DevloopSucceeded || second.ID != first.ID {
		t.Fatalf("second sync = %s id %s (%v), want succeeded on the same row %s", second.State, second.ID, second.Detail, first.ID)
	}
	if n := pods.CopyCalls["backend"]; n != 2 {
		t.Errorf("backend was copied to %d times, want 2 (once per finished request)", n)
	}

	seed.Err = errors.New("github is down")
	failed := waitSync(t, svc, startSync(t, svc, "session-a", "key-again", sha).ID)
	if failed.State != dbstate.DevloopFailed {
		t.Fatalf("sync with the fetch failing = %s, want failed", failed.State)
	}
	seed.Err = nil
	healed := waitSync(t, svc, startSync(t, svc, "session-a", "key-again", sha).ID)
	if healed.State != dbstate.DevloopSucceeded {
		t.Fatalf("sync after the fetch recovered = %s (%v), want succeeded: a failed row must not be sticky", healed.State, healed.Detail)
	}
}

// TestSyncRefusesASessionThatLostTheLease proves the claim-time recheck: a sync queued by a
// session that no longer holds the lease copies nothing.
func TestSyncRefusesASessionThatLostTheLease(t *testing.T) {
	d := testdb.New(t)
	pods := syncPods()
	sha, pack := scratchRepo(t)
	seed := &devlooptest.Seeder{Payload: pack}
	svc := newOperationsService(d, pods, seed)
	holdLease(t, d, "session-b") // session-a never held it, or lost it
	runOperations(t, svc, d)

	row := waitSync(t, svc, startSync(t, svc, "session-a", "key-lost", sha).ID)
	if row.State != dbstate.DevloopFailed || row.Detail == nil || !strings.Contains(*row.Detail, "lease") {
		t.Fatalf("sync without the lease = %s (%v), want failed naming the lease", row.State, row.Detail)
	}
	if seed.Calls != 0 || pods.CopyCalls["backend"] != 0 {
		t.Errorf("a sync without the lease fetched %d times and copied %d times, want neither", seed.Calls, pods.CopyCalls["backend"])
	}
}

// TestSyncWaitsForTheBackendToComeBackHealthy proves the health wait: a backend that fails its
// first checks (restarting under watchexec) is polled until it answers, and the result says so.
func TestSyncWaitsForTheBackendToComeBackHealthy(t *testing.T) {
	d := testdb.New(t)
	pods := syncPods()
	pods.HealthFailFirst = 2
	sha, pack := scratchRepo(t)
	svc := newOperationsService(d, pods, &devlooptest.Seeder{Payload: pack})
	holdLease(t, d, "session-a")
	runOperations(t, svc, d)

	row := waitSync(t, svc, startSync(t, svc, "session-a", "key-health", sha).ID)
	if row.State != dbstate.DevloopSucceeded || !strings.Contains(string(row.Result), `"health_ok": true`) {
		t.Fatalf("sync = %s result %s (%v), want succeeded with health_ok", row.State, row.Result, row.Detail)
	}
	if pods.HealthCalls != 3 {
		t.Errorf("health was checked %d times, want 3 (two failures, then ok)", pods.HealthCalls)
	}
}

// TestSyncReportsAnUnhealthyBackendWithItsLogs proves a backend that never comes up (a compile
// error under `go run`) still finishes the sync, with health_ok false and the log tail.
func TestSyncReportsAnUnhealthyBackendWithItsLogs(t *testing.T) {
	d := testdb.New(t)
	pods := syncPods()
	pods.HealthFailFirst = 1 << 30
	pods.LogLines["backend-0/backend"] = "cmd/mainloop/main.go:1: undefined: nope"
	sha, pack := scratchRepo(t)
	svc := newOperationsService(d, pods, &devlooptest.Seeder{Payload: pack})
	svc.HealthTimeout = 100 * time.Millisecond
	holdLease(t, d, "session-a")
	runOperations(t, svc, d)

	row := waitSync(t, svc, startSync(t, svc, "session-a", "key-sick", sha).ID)
	if row.State != dbstate.DevloopSucceeded || !strings.Contains(string(row.Result), `"health_ok": false`) {
		t.Fatalf("sync = %s result %s, want succeeded with health_ok false", row.State, row.Result)
	}
	if row.Detail == nil || !strings.Contains(*row.Detail, "undefined: nope") {
		t.Errorf("detail = %v, want the backend's last log lines", row.Detail)
	}
}

// TestSyncGivesUpOnACopyThatKeepsBeingCut proves the retry cap: an exec stream that is cut every
// time ends in failed, not in an endless unknown row.
func TestSyncGivesUpOnACopyThatKeepsBeingCut(t *testing.T) {
	d := testdb.New(t)
	pods := syncPods()
	for range 20 {
		pods.QueueCopyResult("frontend", devlooptest.ErrStreamReset)
	}
	sha, pack := scratchRepo(t)
	svc := newOperationsService(d, pods, &devlooptest.Seeder{Payload: pack})
	holdLease(t, d, "session-a")
	runOperations(t, svc, d)

	deadline := time.Now().Add(20 * time.Second)
	var row dbq.DevloopOperation
	id := startSync(t, svc, "session-a", "key-cut", sha).ID
	for time.Now().Before(deadline) {
		row = waitSync(t, svc, id)
		if row.State == dbstate.DevloopFailed {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if row.State != dbstate.DevloopFailed || row.Detail == nil || !strings.Contains(*row.Detail, "gave up") {
		t.Fatalf("sync with every copy cut = %s (%v), want failed with a gave-up detail", row.State, row.Detail)
	}
}

// waitState polls a row until it reaches state.
func waitState(t *testing.T, d *testdb.DB, id string, state dbstate.DevloopOperationState) dbq.DevloopOperation {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		row, err := dbq.New(d.Pool).GetDevloopOperation(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if row.State == state {
			return row
		}
		if time.Now().After(deadline) {
			t.Fatalf("row %s is %s (%v), want %s", id, row.State, row.Detail, state)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func receivedMain(pods *devlooptest.Pods, container string) string {
	pods.Lock()
	defer pods.Unlock()
	return string(pods.Received[container])
}

// TestReviewOlderUncertainSyncOverwritesNewerOne is the reviewer's repro: dev_sync(c1) is cut and
// waits to retry, dev_sync(c2) succeeds, and c1's retry must not then copy c1 over c2.
func TestReviewOlderUncertainSyncOverwritesNewerOne(t *testing.T) {
	d := testdb.New(t)
	pods := syncPods()
	pods.QueueCopyResult("backend", nil)
	pods.QueueCopyResult("mcp", nil)
	pods.QueueCopyResult("git", nil)
	pods.QueueCopyResult("frontend", devlooptest.ErrStreamReset)
	c1, c2, pack := scratchRepoTwoCommits(t)
	svc := newOperationsService(d, pods, &devlooptest.Seeder{Payload: pack})
	svc.UncertainRetryDelay = 800 * time.Millisecond
	holdLease(t, d, "session-a")
	runOperations(t, svc, d)

	first := startSync(t, svc, "session-a", "key-c1", c1)
	waitState(t, d, first.ID, dbstate.DevloopUnknown)
	second := waitSync(t, svc, startSync(t, svc, "session-a", "key-c2", c2).ID)
	if second.State != dbstate.DevloopSucceeded {
		t.Fatalf("second sync = %s (%v), want succeeded", second.State, second.Detail)
	}
	time.Sleep(1500 * time.Millisecond) // past c1's retry time
	old, err := dbq.New(d.Pool).GetDevloopOperation(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.State != dbstate.DevloopFailed || old.Detail == nil || !strings.Contains(*old.Detail, "superseded") {
		t.Fatalf("older sync = %s (%v), want failed as superseded", old.State, old.Detail)
	}
	if got := receivedMain(pods, "backend"); !strings.Contains(got, "package main // second") {
		t.Errorf("backend ended on the older commit; its main.go is not the second commit's")
	}
	if n := pods.Copies("backend"); n != 2 {
		t.Errorf("backend was copied to %d times, want 2 (c1 once, c2 once, no retry of c1)", n)
	}
}

// TestSyncFailsWhenANewerSyncAlreadyRan proves the claim-time check on its own: an older row that
// was not superseded when the newer one was created (another replica, a race) still does not run
// once a newer sync from the same session is waiting or done.
func TestSyncFailsWhenANewerSyncAlreadyRan(t *testing.T) {
	d := testdb.New(t)
	pods := syncPods()
	c1, _, pack := scratchRepoTwoCommits(t)
	svc := newOperationsService(d, pods, &devlooptest.Seeder{Payload: pack})
	holdLease(t, d, "session-a")
	older := startSync(t, svc, "session-a", "key-old", c1)
	if _, err := d.Pool.Exec(context.Background(), `INSERT INTO devloop_operations
		(id, idempotency_key, kind, session_id, project_id, params, state)
		VALUES ('newer', 'newer', 'sync', 'session-a', 'proj-1', '{}', 'succeeded')`); err != nil {
		t.Fatal(err)
	}
	runOperations(t, svc, d)

	row := waitSync(t, svc, older.ID)
	if row.State != dbstate.DevloopFailed || row.Detail == nil || !strings.Contains(*row.Detail, "superseded") {
		t.Fatalf("older sync = %s (%v), want failed as superseded", row.State, row.Detail)
	}
	if n := pods.Copies("backend"); n != 0 {
		t.Errorf("backend was copied to %d times, want 0", n)
	}
}

// TestOnlyOneSyncCopiesAtATime proves the sync lock: while one sync is copying, a second sync
// (a newer commit from the same session, so it is not superseded: the first is running) does not
// start copying until the first is done. Without the lock both extract into the same directories.
func TestOnlyOneSyncCopiesAtATime(t *testing.T) {
	d := testdb.New(t)
	pods := syncPods()
	c1, c2, pack := scratchRepoTwoCommits(t)
	var (
		inFlight, maxInFlight atomic.Int32
		entered               = make(chan struct{})
		release               = make(chan struct{})
		once                  sync.Once
	)
	pods.CopyHook = func(_ context.Context, _ string) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		once.Do(func() {
			close(entered)
			<-release
		})
		time.Sleep(20 * time.Millisecond)
	}
	svc := newOperationsService(d, pods, &devlooptest.Seeder{Payload: pack})
	holdLease(t, d, "session-a")
	runOperations(t, svc, d)
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock) // runs before the loop's stop, so a failed assertion cannot hang the test

	first := startSync(t, svc, "session-a", "key-c1", c1)
	<-entered
	second := startSync(t, svc, "session-a", "key-c2", c2)
	time.Sleep(1500 * time.Millisecond) // several loop polls and lock attempts
	if n := maxInFlight.Load(); n != 1 {
		t.Fatalf("%d copies were in flight at once while the first sync was copying, want 1", n)
	}
	if row := waitStateOrNot(t, d, second.ID); row.State == dbstate.DevloopSucceeded {
		t.Fatalf("second sync finished while the first was still copying")
	}
	unblock()
	if row := waitSync(t, svc, first.ID); row.State != dbstate.DevloopSucceeded {
		t.Fatalf("first sync = %s (%v), want succeeded", row.State, row.Detail)
	}
	if row := waitSync(t, svc, second.ID); row.State != dbstate.DevloopSucceeded {
		t.Fatalf("second sync = %s (%v), want succeeded after the first", row.State, row.Detail)
	}
	if n := maxInFlight.Load(); n != 1 {
		t.Errorf("%d copies were in flight at once, want 1", n)
	}
	if got := receivedMain(pods, "backend"); !strings.Contains(got, "package main // second") {
		t.Errorf("the newer commit did not end up last")
	}
}

func waitStateOrNot(t *testing.T, d *testdb.DB, id string) dbq.DevloopOperation {
	t.Helper()
	row, err := dbq.New(d.Pool).GetDevloopOperation(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// TestSyncLockHeldByACrashedSyncExpires proves a sync lock left by a replica that died does not
// block syncs for good: a live holder defers the sync, an expired one does not.
func TestSyncLockHeldByACrashedSyncExpires(t *testing.T) {
	d := testdb.New(t)
	pods := syncPods()
	sha, pack := scratchRepo(t)
	svc := newOperationsService(d, pods, &devlooptest.Seeder{Payload: pack})
	holdLease(t, d, "session-a")
	ctx := context.Background()
	if _, err := d.Pool.Exec(ctx, `UPDATE devloop_sync_lock SET op_id = 'ghost', held_until = now() + interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	runOperations(t, svc, d)

	row := startSync(t, svc, "session-a", "key-lock", sha)
	time.Sleep(700 * time.Millisecond)
	if got := waitStateOrNot(t, d, row.ID); got.State != dbstate.DevloopPending || pods.Copies("backend") != 0 {
		t.Fatalf("sync under a live lock = %s with %d copies, want pending and none", got.State, pods.Copies("backend"))
	}
	if _, err := d.Pool.Exec(ctx, `UPDATE devloop_sync_lock SET held_until = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if got := waitSync(t, svc, row.ID); got.State != dbstate.DevloopSucceeded {
		t.Fatalf("sync after the lock expired = %s (%v), want succeeded", got.State, got.Detail)
	}
}

// TestSyncDeadlineRunsFromTheRequest proves the five-minute limit is for the whole sync: a request
// that is already past it fails without copying, and a copy that outlives the limit fails instead
// of being retried as an interrupted copy.
func TestSyncDeadlineRunsFromTheRequest(t *testing.T) {
	d := testdb.New(t)
	pods := syncPods()
	sha, pack := scratchRepo(t)
	svc := newOperationsService(d, pods, &devlooptest.Seeder{Payload: pack})
	holdLease(t, d, "session-a")
	ctx := context.Background()

	stale := startSync(t, svc, "session-a", "key-stale", sha)
	if _, err := d.Pool.Exec(ctx, `UPDATE devloop_operations SET created_at = now() - interval '10 minutes' WHERE id = $1`, stale.ID); err != nil {
		t.Fatal(err)
	}
	runOperations(t, svc, d)
	row := waitSync(t, svc, stale.ID)
	if row.State != dbstate.DevloopFailed || row.Detail == nil || !strings.Contains(*row.Detail, "did not finish") || pods.Copies("backend") != 0 {
		t.Fatalf("sync past its deadline = %s (%v) with %d copies, want failed without copying", row.State, row.Detail, pods.Copies("backend"))
	}

	// A copy that hangs until the deadline: it must fail, not come back as unknown.
	var hungTries atomic.Int32
	pods.CopyHook = func(ctx context.Context, _ string) { hungTries.Add(1); <-ctx.Done() }
	hung := startSync(t, svc, "session-a", "key-hung", sha)
	if _, err := d.Pool.Exec(ctx, `UPDATE devloop_operations SET created_at = now() - interval '5 minutes' + interval '600 milliseconds' WHERE id = $1`, hung.ID); err != nil {
		t.Fatal(err)
	}
	row = waitSync(t, svc, hung.ID)
	if row.State != dbstate.DevloopFailed || row.Detail == nil || !strings.Contains(*row.Detail, "did not finish") {
		t.Fatalf("hung copy = %s (%v), want failed at the deadline", row.State, row.Detail)
	}
	if n := hungTries.Load(); n != 1 {
		t.Errorf("a hung copy was tried %d times, want 1", n)
	}
}

// TestADefiniteTarFailureIsNotRetried proves the classifier looks at the exit status, not at the
// message: a tar that ran and failed fails the sync at once even when its message holds a word
// like "closed", "reset" or "EOF" (a file name is enough).
func TestADefiniteTarFailureIsNotRetried(t *testing.T) {
	d := testdb.New(t)
	pods := syncPods()
	pods.QueueCopyResult("backend", devlooptest.ExitError{Code: 2, Msg: "command terminated with exit code 2: tar: server/reset/closed_EOF_timeout.go: Cannot open"})
	sha, pack := scratchRepo(t)
	svc := newOperationsService(d, pods, &devlooptest.Seeder{Payload: pack})
	holdLease(t, d, "session-a")
	runOperations(t, svc, d)

	row := waitSync(t, svc, startSync(t, svc, "session-a", "key-exit", sha).ID)
	if row.State != dbstate.DevloopFailed || row.Detail == nil || !strings.Contains(*row.Detail, "status 2") {
		t.Fatalf("sync with tar exiting 2 = %s (%v), want failed naming the status", row.State, row.Detail)
	}
	if n := pods.Copies("backend"); n != 1 {
		t.Errorf("backend was copied to %d times, want 1 (no retry)", n)
	}
}

// TestRepeatCallOfAnOlderSyncDoesNotFailTheNewerOne is the re-review's repro: c1 is mid-copy when
// c2 is requested, c1's copy is cut and waits to retry, and the agent calls dev_sync(c1) again.
// That repeat call finds an unfinished row, so it is not a new request and must not supersede c2.
func TestRepeatCallOfAnOlderSyncDoesNotFailTheNewerOne(t *testing.T) {
	d := testdb.New(t)
	c1, c2, pack := scratchRepoTwoCommits(t)
	svc := newOperationsService(d, syncPods(), &devlooptest.Seeder{Payload: pack})
	holdLease(t, d, "session-a")
	ctx := context.Background()
	older := startSync(t, svc, "session-a", "key-c1", c1)
	// c1 is claimed when c2 is requested, so Start's supersede skips it.
	if _, err := d.Pool.Exec(ctx, `UPDATE devloop_operations SET lease_owner = 'x', lease_until = now() + interval '1 minute' WHERE id = $1`, older.ID); err != nil {
		t.Fatal(err)
	}
	newer := startSync(t, svc, "session-a", "key-c2", c2)
	// c1's copy is cut: it goes back to unknown, unleased, waiting to retry.
	if _, err := d.Pool.Exec(ctx, `UPDATE devloop_operations SET state = 'unknown', lease_owner = NULL, lease_until = NULL WHERE id = $1`, older.ID); err != nil {
		t.Fatal(err)
	}
	startSync(t, svc, "session-a", "key-c1", c1) // the agent retries c1
	row := waitStateOrNot(t, d, newer.ID)
	if row.State == dbstate.DevloopFailed {
		t.Fatalf("re-calling the older commit failed the newer sync: %s", strings.TrimSpace(*row.Detail))
	}
	// And the newer sync still runs to success; the older row ends as superseded.
	runOperations(t, svc, d)
	if got := waitSync(t, svc, newer.ID); got.State != dbstate.DevloopSucceeded {
		t.Fatalf("newer sync = %s (%v), want succeeded", got.State, got.Detail)
	}
}
