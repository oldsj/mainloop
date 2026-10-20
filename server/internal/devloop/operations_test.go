package devloop_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

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
	t.Parallel()
	d := testdb.New(t)
	pods := devlooptest.New()
	pods.BySelector["app=mainloop-backend"] = []devloop.PodStatus{{Name: "backend-0", Phase: "Running", Ready: true}}
	pods.BySelector["app=mainloop-frontend"] = []devloop.PodStatus{{Name: "frontend-0", Phase: "Running", Ready: true}}
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
	t.Parallel()
	d := testdb.New(t)
	pods := devlooptest.New()
	pods.BySelector["app=mainloop-backend"] = []devloop.PodStatus{{Name: "backend-0", Phase: "Running", Ready: true}}
	pods.BySelector["app=mainloop-frontend"] = []devloop.PodStatus{{Name: "frontend-0", Phase: "Running", Ready: true}}
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
	t.Parallel()
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
	pods.BySelector["app=mainloop-backend"] = []devloop.PodStatus{{Name: "backend-0", Phase: "Running", Ready: true}}
	pods.BySelector["app=mainloop-frontend"] = []devloop.PodStatus{{Name: "frontend-0", Phase: "Running", Ready: true}}
	return pods
}

// TestSyncRunsAgainAfterTheEarlierRowFinished proves the behavior that fails without the change:
// the same session syncing the same commit a second time, after the first row succeeded, is
// copied again instead of being handed the stale finished row. A failed sync is retried the same
// way.
func TestSyncRunsAgainAfterTheEarlierRowFinished(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	for _, want := range []string{`"migrate_ok": true`, `"passed": true`, "summary: 8 ok, 0 fail, 2 skip"} {
		if !strings.Contains(string(row.Result), want) {
			t.Errorf("result %s lacks %s", row.Result, want)
		}
	}
	if pods.HealthCalls != 3 {
		t.Errorf("health was checked %d times, want 3 (two failures, then ok)", pods.HealthCalls)
	}
}

// TestSyncFailsWhenTheBackendIsUnhealthy proves a backend that never comes up (a compile error
// under `go run`) fails the sync: the state is failed, not succeeded, the detail carries the log
// tail, and neither the smoke run nor anything after the health wait happens.
func TestSyncFailsWhenTheBackendIsUnhealthy(t *testing.T) {
	t.Parallel()
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
	if row.State != dbstate.DevloopFailed || !strings.Contains(string(row.Result), `"health_ok": false`) {
		t.Fatalf("sync = %s result %s, want failed with health_ok false", row.State, row.Result)
	}
	if row.Detail == nil || !strings.Contains(*row.Detail, "undefined: nope") {
		t.Errorf("detail = %v, want the backend's last log lines", row.Detail)
	}
	if slices.Contains(pods.Seen(), "smoke") {
		t.Errorf("smoke ran against an unhealthy backend: %v", pods.Seen())
	}
}

// TestSyncFailsWhenMigrateFails proves `migrate up` failing fails the sync with the command's
// output, before the health wait.
func TestSyncFailsWhenMigrateFails(t *testing.T) {
	t.Parallel()
	d := testdb.New(t)
	pods := syncPods()
	pods.MigrateErr = devlooptest.ExitError{Code: 1, Msg: "command terminated with exit code 1"}
	pods.MigrateOut = "migrate: 00251_x.sql: column \"y\" does not exist"
	sha, pack := scratchRepo(t)
	svc := newOperationsService(d, pods, &devlooptest.Seeder{Payload: pack})
	holdLease(t, d, "session-a")
	runOperations(t, svc, d)

	row := waitSync(t, svc, startSync(t, svc, "session-a", "key-migrate", sha).ID)
	if row.State != dbstate.DevloopFailed || row.Detail == nil ||
		!strings.Contains(*row.Detail, "migrate up failed") || !strings.Contains(*row.Detail, `column "y" does not exist`) {
		t.Fatalf("sync = %s (%v), want failed naming migrate and its output", row.State, row.Detail)
	}
	if !strings.Contains(string(row.Result), `"migrate_ok": false`) {
		t.Errorf("result = %s, want migrate_ok false", row.Result)
	}
	if seen := pods.Seen(); slices.Contains(seen, "health") || slices.Contains(seen, "smoke") {
		t.Errorf("health or smoke ran after a failed migrate: %v", seen)
	}
}

// TestSyncFailsWhenSmokeFails proves a failing read-only smoke run fails the sync and attaches
// the run's output, even though the backend is healthy.
func TestSyncFailsWhenSmokeFails(t *testing.T) {
	t.Parallel()
	d := testdb.New(t)
	pods := syncPods()
	pods.SmokeErr = devlooptest.ExitError{Code: 1, Msg: "command terminated with exit code 1"}
	pods.SmokeOut = "ok   health\nFAIL projects: GET /projects: status 500\nsummary: 1 ok, 1 fail, 0 skip\n"
	sha, pack := scratchRepo(t)
	svc := newOperationsService(d, pods, &devlooptest.Seeder{Payload: pack})
	holdLease(t, d, "session-a")
	runOperations(t, svc, d)

	row := waitSync(t, svc, startSync(t, svc, "session-a", "key-smoke", sha).ID)
	if row.State != dbstate.DevloopFailed || row.Detail == nil ||
		!strings.Contains(*row.Detail, "smoke") || !strings.Contains(*row.Detail, "GET /projects: status 500") {
		t.Fatalf("sync = %s (%v), want failed with the smoke output", row.State, row.Detail)
	}
	if !strings.Contains(string(row.Result), `"health_ok": true`) || !strings.Contains(string(row.Result), `"passed": false`) {
		t.Errorf("result = %s, want health_ok true and smoke passed false", row.Result)
	}
}

// TestSyncMigratesAfterTheCopiesAndSmokesAfterHealth proves the order: every copy, then migrate,
// then /health, then smoke.
func TestSyncMigratesAfterTheCopiesAndSmokesAfterHealth(t *testing.T) {
	t.Parallel()
	d := testdb.New(t)
	pods := syncPods()
	sha, pack := scratchRepo(t)
	svc := newOperationsService(d, pods, &devlooptest.Seeder{Payload: pack})
	holdLease(t, d, "session-a")
	runOperations(t, svc, d)

	row := waitSync(t, svc, startSync(t, svc, "session-a", "key-order", sha).ID)
	if row.State != dbstate.DevloopSucceeded {
		t.Fatalf("sync = %s (%v), want succeeded", row.State, row.Detail)
	}
	got := strings.Join(pods.Seen(), ",")
	if want := "tar:backend,tar:mcp,tar:git,tar:frontend,marker,migrate,health,smoke"; got != want {
		t.Errorf("exec order = %s, want %s", got, want)
	}
}

// TestSyncPicksARunningPodThatIsNotBeingDeleted proves a terminating, pending or evicted pod that
// matches the label is not a copy target, and that no such pod at all fails the sync.
func TestSyncPicksARunningPodThatIsNotBeingDeleted(t *testing.T) {
	t.Parallel()
	d := testdb.New(t)
	pods := syncPods()
	pods.BySelector["app=mainloop-backend"] = []devloop.PodStatus{
		{Name: "backend-old", Phase: "Running", Ready: true, Deleting: true},
		{Name: "backend-evicted", Phase: "Failed"},
		{Name: "backend-new", Phase: "Running", Ready: true},
	}
	sha, pack := scratchRepo(t)
	svc := newOperationsService(d, pods, &devlooptest.Seeder{Payload: pack})
	holdLease(t, d, "session-a")
	runOperations(t, svc, d)

	row := waitSync(t, svc, startSync(t, svc, "session-a", "key-pick", sha).ID)
	if row.State != dbstate.DevloopSucceeded || pods.LastPod["backend"] != "backend-new" {
		t.Fatalf("sync = %s (%v) copied to %q, want succeeded on backend-new", row.State, row.Detail, pods.LastPod["backend"])
	}

	pods.BySelector["app=mainloop-backend"] = []devloop.PodStatus{{Name: "backend-old", Phase: "Running", Deleting: true}}
	row = waitSync(t, svc, startSync(t, svc, "session-a", "key-nopod", sha).ID)
	if row.State != dbstate.DevloopFailed || row.Detail == nil || !strings.Contains(*row.Detail, "no running pod") {
		t.Fatalf("sync with only a terminating pod = %s (%v), want failed naming the missing pod", row.State, row.Detail)
	}
}

// TestSyncToleratesHarmlessBusyBoxTarWarnings proves a BusyBox tar that warns about PAX headers
// and exits 0 does not fail the sync, while any other stderr still does.
func TestSyncToleratesHarmlessBusyBoxTarWarnings(t *testing.T) {
	t.Parallel()
	d := testdb.New(t)
	pods := syncPods()
	pods.QueueCopyStderr("frontend", "tar: warning: skipping header 'x'\ntar: removing leading '/' from member names\n")
	sha, pack := scratchRepo(t)
	svc := newOperationsService(d, pods, &devlooptest.Seeder{Payload: pack})
	holdLease(t, d, "session-a")
	runOperations(t, svc, d)

	row := waitSync(t, svc, startSync(t, svc, "session-a", "key-busybox", sha).ID)
	if row.State != dbstate.DevloopSucceeded {
		t.Fatalf("sync with BusyBox warnings = %s (%v), want succeeded", row.State, row.Detail)
	}

	pods.QueueCopyStderr("frontend", "tar: warning: skipping header 'x'\ntar: can't open 'src/a.ts': Permission denied\n")
	row = waitSync(t, svc, startSync(t, svc, "session-a", "key-realerr", sha).ID)
	if row.State != dbstate.DevloopFailed || row.Detail == nil || !strings.Contains(*row.Detail, "Permission denied") {
		t.Fatalf("sync with a real tar error = %s (%v), want failed naming it", row.State, row.Detail)
	}
	if strings.Contains(*row.Detail, "skipping header") {
		t.Errorf("detail = %q, should not repeat the harmless warning", *row.Detail)
	}
}

// TestSyncGivesUpOnACopyThatKeepsBeingCut proves the retry cap: an exec stream that is cut every
// time ends in failed, not in an endless unknown row.
func TestSyncGivesUpOnACopyThatKeepsBeingCut(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	d := testdb.New(t)
	pods := syncPods()
	sha, pack := scratchRepo(t)
	svc := newOperationsService(d, pods, &devlooptest.Seeder{Payload: pack})
	holdLease(t, d, "session-a")
	ctx := context.Background()

	stale := startSync(t, svc, "session-a", "key-stale", sha)
	if _, err := d.Pool.Exec(ctx, `UPDATE devloop_operations SET created_at = now() - interval '20 minutes' WHERE id = $1`, stale.ID); err != nil {
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
	if _, err := d.Pool.Exec(ctx, `UPDATE devloop_operations SET created_at = now() - interval '10 minutes' + interval '600 milliseconds' WHERE id = $1`, hung.ID); err != nil {
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
	t.Parallel()
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
	t.Parallel()
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

// runningBackend is syncPods' backend pod with a restart count and a container to read logs from.
func runningBackend(name string, restarts int32) []devloop.PodStatus {
	return []devloop.PodStatus{{Name: name, Phase: "Running", Ready: true, RestartCount: restarts, Containers: []string{"backend"},
		ContainerRestarts: map[string]int32{"backend": restarts}}}
}

// TestSyncFailsWhenTheBackendRestartsAfterTheCopy proves the case the verdict exists to catch: a
// container that restarts after the copy starts again from the image's tree, so its /health and
// the smoke would pass for code that is not the commit under test. The restart count rising at the
// first health probe, or during the smoke, fails the sync, with the previous instance's log.
func TestSyncFailsWhenTheBackendRestartsAfterTheCopy(t *testing.T) {
	t.Parallel()
	for _, at := range []string{"health", "smoke"} {
		t.Run(at, func(t *testing.T) {
			d := testdb.New(t)
			pods := syncPods()
			pods.SetPods("app=mainloop-backend", runningBackend("backend-0", 0))
			pods.LogLines["backend-0/backend/previous"] = "panic: nil pointer in newWiring"
			var once sync.Once
			pods.Hook = func(_ context.Context, event string) {
				if event == at {
					once.Do(func() { pods.SetPods("app=mainloop-backend", runningBackend("backend-0", 1)) })
				}
			}
			sha, pack := scratchRepo(t)
			svc := newOperationsService(d, pods, &devlooptest.Seeder{Payload: pack})
			holdLease(t, d, "session-a")
			runOperations(t, svc, d)

			row := waitSync(t, svc, startSync(t, svc, "session-a", "key-restart-"+at, sha).ID)
			if row.State != dbstate.DevloopFailed || row.Detail == nil ||
				!strings.Contains(*row.Detail, "restarted during the sync") || !strings.Contains(*row.Detail, "panic: nil pointer in newWiring") {
				t.Fatalf("sync with a restart at %s = %s (%v), want failed naming the restart and the previous log", at, row.State, row.Detail)
			}
		})
	}
}

// TestSyncFailsWhenThePodIsReplacedMidWait proves a pod replaced after the copy fails the sync: the
// new pod never got the copy, so asking it for /health would be asking about the image's tree.
func TestSyncFailsWhenThePodIsReplacedMidWait(t *testing.T) {
	t.Parallel()
	d := testdb.New(t)
	pods := syncPods()
	var once sync.Once
	pods.Hook = func(_ context.Context, event string) {
		if event == "health" {
			once.Do(func() { pods.SetPods("app=mainloop-backend", runningBackend("backend-1", 0)) })
		}
	}
	sha, pack := scratchRepo(t)
	svc := newOperationsService(d, pods, &devlooptest.Seeder{Payload: pack})
	holdLease(t, d, "session-a")
	runOperations(t, svc, d)

	row := waitSync(t, svc, startSync(t, svc, "session-a", "key-replaced", sha).ID)
	if row.State != dbstate.DevloopFailed || row.Detail == nil || !strings.Contains(*row.Detail, "backend-0 was replaced") {
		t.Fatalf("sync with a replaced pod = %s (%v), want failed naming the pod", row.State, row.Detail)
	}
}

// TestSyncFailsWhenTheCommitMarkerIsGone proves the same-exec marker check: a container restarted
// from the image has no marker for this commit, whatever its restart count says.
func TestSyncFailsWhenTheCommitMarkerIsGone(t *testing.T) {
	t.Parallel()
	d := testdb.New(t)
	pods := syncPods()
	pods.Hook = func(_ context.Context, event string) {
		if event == "migrate" {
			pods.ResetMarker()
		}
	}
	sha, pack := scratchRepo(t)
	svc := newOperationsService(d, pods, &devlooptest.Seeder{Payload: pack})
	holdLease(t, d, "session-a")
	runOperations(t, svc, d)

	row := waitSync(t, svc, startSync(t, svc, "session-a", "key-marker", sha).ID)
	if row.State != dbstate.DevloopFailed || row.Detail == nil || !strings.Contains(*row.Detail, "not the synced commit") {
		t.Fatalf("sync with the marker gone = %s (%v), want failed saying the tree is not the synced commit", row.State, row.Detail)
	}
	if slices.Contains(pods.Seen(), "smoke") {
		t.Errorf("smoke ran after the marker check failed: %v", pods.Seen())
	}
}

// TestSyncNamesTheStepThatTimedOut proves each step's timeout names its own cap, and that the
// overall sync deadline is named as such instead of as the step's cap.
func TestSyncNamesTheStepThatTimedOut(t *testing.T) {
	t.Parallel()
	block := func(step string) func(context.Context, string) {
		return func(ctx context.Context, event string) {
			if event == step {
				<-ctx.Done()
			}
		}
	}
	for _, tc := range []struct{ step, want string }{
		{"migrate", "migrate up failed: did not finish within 50ms"},
		{"smoke", "did not finish within 50ms"},
	} {
		t.Run(tc.step, func(t *testing.T) {
			d := testdb.New(t)
			pods := syncPods()
			pods.Hook = block(tc.step)
			sha, pack := scratchRepo(t)
			svc := newOperationsService(d, pods, &devlooptest.Seeder{Payload: pack})
			svc.MigrateTimeout, svc.SmokeTimeout = 50*time.Millisecond, 50*time.Millisecond
			holdLease(t, d, "session-a")
			runOperations(t, svc, d)

			row := waitSync(t, svc, startSync(t, svc, "session-a", "key-timeout-"+tc.step, sha).ID)
			if row.State != dbstate.DevloopFailed || row.Detail == nil || !strings.Contains(*row.Detail, tc.want) {
				t.Fatalf("%s timeout = %s (%v), want failed with %q", tc.step, row.State, row.Detail, tc.want)
			}
		})
	}

	for _, step := range []string{"migrate", "smoke"} {
		t.Run(step+" past the sync deadline", func(t *testing.T) {
			d := testdb.New(t)
			pods := syncPods()
			pods.Hook = block(step)
			sha, pack := scratchRepo(t)
			svc := newOperationsService(d, pods, &devlooptest.Seeder{Payload: pack})
			holdLease(t, d, "session-a")
			runOperations(t, svc, d)

			sync := startSync(t, svc, "session-a", "key-deadline-"+step, sha)
			if _, err := d.Pool.Exec(context.Background(), `UPDATE devloop_operations SET created_at = now() - interval '10 minutes' + interval '1500 milliseconds' WHERE id = $1`, sync.ID); err != nil {
				t.Fatal(err)
			}
			row := waitSync(t, svc, sync.ID)
			if row.State != dbstate.DevloopFailed || row.Detail == nil ||
				!strings.Contains(*row.Detail, "the sync was cut off") || strings.Contains(*row.Detail, "did not finish within 2m30s") {
				t.Fatalf("%s past the deadline = %s (%v), want failed naming the sync deadline", step, row.State, row.Detail)
			}
		})
	}
}

// TestSyncNamesTheRecoveryForOutOfOrderMigrations proves goose's refusal (another branch's
// migration already applied in the shared dev database) comes with what the owner can do, and that
// the sync does not reset anything itself.
func TestSyncNamesTheRecoveryForOutOfOrderMigrations(t *testing.T) {
	t.Parallel()
	d := testdb.New(t)
	pods := syncPods()
	pods.MigrateErr = devlooptest.ExitError{Code: 1, Msg: "command terminated with exit code 1"}
	pods.MigrateOut = "migrate: detected 1 missing (out-of-order) migration lower than database version (270): version 260"
	sha, pack := scratchRepo(t)
	svc := newOperationsService(d, pods, &devlooptest.Seeder{Payload: pack})
	holdLease(t, d, "session-a")
	runOperations(t, svc, d)

	row := waitSync(t, svc, startSync(t, svc, "session-a", "key-ooo", sha).ID)
	if row.State != dbstate.DevloopFailed || row.Detail == nil ||
		!strings.Contains(*row.Detail, "MAINLOOP_ALLOW_RESET=1") || !strings.Contains(*row.Detail, "version 260") {
		t.Fatalf("sync with out-of-order migrations = %s (%v), want failed naming the owner's recovery", row.State, row.Detail)
	}
}

// TestSyncDetailStaysValidUTF8WhenTheTailIsCutMidCharacter proves a long multibyte output tail
// cannot make the detail something the database refuses (which would rerun the whole sync).
func TestSyncDetailStaysValidUTF8WhenTheTailIsCutMidCharacter(t *testing.T) {
	t.Parallel()
	d := testdb.New(t)
	pods := syncPods()
	pods.MigrateErr = devlooptest.ExitError{Code: 1, Msg: "command terminated with exit code 1"}
	pods.MigrateOut = strings.Repeat("é", 2000) + "x" // 4001 bytes on one line: the 3000-byte cut lands inside a character
	sha, pack := scratchRepo(t)
	svc := newOperationsService(d, pods, &devlooptest.Seeder{Payload: pack})
	holdLease(t, d, "session-a")
	runOperations(t, svc, d)

	row := waitSync(t, svc, startSync(t, svc, "session-a", "key-utf8", sha).ID)
	if row.State != dbstate.DevloopFailed || row.Detail == nil || !utf8.ValidString(*row.Detail) {
		t.Fatalf("sync with a multibyte tail = %s (%v), want failed with a valid UTF-8 detail", row.State, row.Detail)
	}
}

// TestSyncWaitsForTheMCPAndGitListeners proves a backend that answers while mcp or git still does
// not is not yet healthy: the health probe covers all three, so smoke cannot race their rebuilds.
func TestSyncWaitsForTheMCPAndGitListeners(t *testing.T) {
	t.Parallel()
	d := testdb.New(t)
	pods := syncPods()
	pods.HealthFailFirst = 2
	sha, pack := scratchRepo(t)
	svc := newOperationsService(d, pods, &devlooptest.Seeder{Payload: pack})
	holdLease(t, d, "session-a")
	runOperations(t, svc, d)

	row := waitSync(t, svc, startSync(t, svc, "session-a", "key-listeners", sha).ID)
	if row.State != dbstate.DevloopSucceeded || pods.HealthCalls != 3 {
		t.Fatalf("sync = %s (%v) after %d probes, want succeeded on the third", row.State, row.Detail, pods.HealthCalls)
	}
	for _, port := range []string{":8000/health", ":8002/health", ":8003/health"} {
		if !strings.Contains(pods.HealthScripts[0], port) {
			t.Errorf("health probe %q does not ask %s", pods.HealthScripts[0], port)
		}
	}
}

// TestSyncFailsOnARestartWhileHealthKeepsFailing proves the per-round restart check on its own. A
// restart at the first health probe, with health never recovering, is caught at the top of the
// next round: the detail names the restart, the smoke never ran, and the sync did not wait out the
// health deadline. Without the per-round check, only the check after the smoke would catch a
// restart, and a health that never passes never gets that far.
func TestSyncFailsOnARestartWhileHealthKeepsFailing(t *testing.T) {
	t.Parallel()
	d := testdb.New(t)
	pods := syncPods()
	pods.SetPods("app=mainloop-backend", runningBackend("backend-0", 0))
	pods.HealthFailFirst = 1000
	pods.LogLines["backend-0/backend/previous"] = "panic: nil pointer in newWiring"
	var once sync.Once
	pods.Hook = func(_ context.Context, event string) {
		if event == "health" {
			once.Do(func() { pods.SetPods("app=mainloop-backend", runningBackend("backend-0", 1)) })
		}
	}
	sha, pack := scratchRepo(t)
	svc := newOperationsService(d, pods, &devlooptest.Seeder{Payload: pack})
	svc.HealthTimeout = 20 * time.Second
	holdLease(t, d, "session-a")
	runOperations(t, svc, d)

	started := time.Now()
	row := waitSync(t, svc, startSync(t, svc, "session-a", "key-restart-unhealthy", sha).ID)
	if row.State != dbstate.DevloopFailed || row.Detail == nil ||
		!strings.Contains(*row.Detail, "restarted during the sync") || !strings.Contains(*row.Detail, "panic: nil pointer in newWiring") {
		t.Fatalf("sync = %s (%v), want failed naming the restart and the previous log", row.State, row.Detail)
	}
	if strings.Contains(*row.Detail, "did not report healthy") {
		t.Errorf("the detail is the health timeout, not the restart: %s", *row.Detail)
	}
	if slices.Contains(pods.Seen(), "smoke") {
		t.Errorf("smoke ran: %v", pods.Seen())
	}
	if pods.HealthCalls != 1 {
		t.Errorf("health was probed %d times, want 1: the restart should end the wait at the next round", pods.HealthCalls)
	}
	if took := time.Since(started); took > 10*time.Second {
		t.Errorf("sync took %s, so it waited for the %s health deadline", took, svc.HealthTimeout)
	}
}

// TestSyncReadsPreviousLogsOnlyForContainersThatRestarted proves the detail does not carry an
// older crash: the mcp container restarted in an earlier sync, and this sync only the backend's
// count rose.
func TestSyncReadsPreviousLogsOnlyForContainersThatRestarted(t *testing.T) {
	t.Parallel()
	d := testdb.New(t)
	pods := syncPods()
	pod := func(backend int32) []devloop.PodStatus {
		return []devloop.PodStatus{{
			Name: "backend-0", Phase: "Running", Ready: true, RestartCount: backend + 1,
			Containers:        []string{"backend", "mcp", "git"},
			ContainerRestarts: map[string]int32{"backend": backend, "mcp": 1, "git": 0},
		}}
	}
	pods.SetPods("app=mainloop-backend", pod(0))
	pods.LogLines["backend-0/backend/previous"] = "panic: backend crashed just now"
	pods.LogLines["backend-0/mcp/previous"] = "panic: mcp crashed in an earlier sync"
	var once sync.Once
	pods.Hook = func(_ context.Context, event string) {
		if event == "health" {
			once.Do(func() { pods.SetPods("app=mainloop-backend", pod(1)) })
		}
	}
	sha, pack := scratchRepo(t)
	svc := newOperationsService(d, pods, &devlooptest.Seeder{Payload: pack})
	holdLease(t, d, "session-a")
	runOperations(t, svc, d)

	row := waitSync(t, svc, startSync(t, svc, "session-a", "key-restart-which", sha).ID)
	if row.State != dbstate.DevloopFailed || row.Detail == nil {
		t.Fatalf("sync = %s (%v), want failed", row.State, row.Detail)
	}
	if !strings.Contains(*row.Detail, "backend crashed just now") {
		t.Errorf("the detail lacks the container that restarted: %s", *row.Detail)
	}
	if strings.Contains(*row.Detail, "mcp crashed in an earlier sync") {
		t.Errorf("the detail carries a container that did not restart this sync: %s", *row.Detail)
	}
}

// TestSyncCrashLogTailIsSixtyLinesCappedByBytes proves a long goroutine dump still shows the panic
// header 50 lines up, and that a log of very long lines is cut by bytes on a character boundary.
func TestSyncCrashLogTailIsSixtyLinesCappedByBytes(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, log string) string {
		t.Helper()
		d := testdb.New(t)
		pods := syncPods()
		pods.SetPods("app=mainloop-backend", runningBackend("backend-0", 0))
		pods.LogLines["backend-0/backend/previous"] = log
		var once sync.Once
		pods.Hook = func(_ context.Context, event string) {
			if event == "health" {
				once.Do(func() { pods.SetPods("app=mainloop-backend", runningBackend("backend-0", 1)) })
			}
		}
		sha, pack := scratchRepo(t)
		svc := newOperationsService(d, pods, &devlooptest.Seeder{Payload: pack})
		holdLease(t, d, "session-a")
		runOperations(t, svc, d)
		row := waitSync(t, svc, startSync(t, svc, "session-a", "key-crash-tail", sha).ID)
		if row.State != dbstate.DevloopFailed || row.Detail == nil {
			t.Fatalf("sync = %s (%v), want failed", row.State, row.Detail)
		}
		return *row.Detail
	}
	t.Run("lines", func(t *testing.T) {
		var lines []string
		lines = append(lines, "panic: header")
		for i := 1; i <= 79; i++ {
			lines = append(lines, fmt.Sprintf("goroutine frame %d", i))
		}
		detail := run(t, strings.Join(lines, "\n"))
		// 80 lines: the last 60 are frames 20 through 79.
		if !strings.Contains(detail, "goroutine frame 20\n") || !strings.Contains(detail, "goroutine frame 79") || strings.Contains(detail, "goroutine frame 19\n") {
			t.Errorf("detail does not hold exactly the last 60 lines:\n%s", detail)
		}
	})
	t.Run("bytes", func(t *testing.T) {
		line := strings.Repeat("é", 100) // 200 bytes
		var lines []string
		for i := 0; i < 60; i++ {
			lines = append(lines, line)
		}
		lines = append(lines, "last line")
		detail := run(t, strings.Join(lines, "\n"))
		if len(detail) > 8000 {
			t.Errorf("detail is %d bytes, want the crash log capped near 6000", len(detail))
		}
		if !strings.HasSuffix(detail, "last line") || !utf8.ValidString(detail) {
			t.Errorf("detail should end with the last line and stay valid UTF-8, got ...%q", detail[max(0, len(detail)-40):])
		}
	})
}
