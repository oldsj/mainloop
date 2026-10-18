package devloop_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/agentauth"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/devloop"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

func TestLeaseAcquireIsGrantedWhenFree(t *testing.T) {
	d := testdb.New(t)
	s := &devloop.LeaseService{Pool: d.Pool}
	info, err := s.Acquire(context.Background(), "session-a")
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != devloop.LeaseAcquired || info.HolderSession != "session-a" {
		t.Fatalf("Acquire = %+v, want acquired by session-a", info)
	}
	if info.AcquiredAt == nil || info.LastUsedAt == nil {
		t.Fatalf("Acquire = %+v, want timestamps set", info)
	}
}

func TestLeaseAcquireIsIdempotentForTheHolder(t *testing.T) {
	d := testdb.New(t)
	s := &devloop.LeaseService{Pool: d.Pool}
	ctx := context.Background()
	first, err := s.Acquire(ctx, "session-a")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	second, err := s.Acquire(ctx, "session-a")
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != devloop.LeaseAcquired || second.HolderSession != "session-a" {
		t.Fatalf("re-acquire = %+v, want still acquired by session-a", second)
	}
	if !second.LastUsedAt.After(*first.LastUsedAt) {
		t.Errorf("re-acquire did not heartbeat: first %v, second %v", first.LastUsedAt, second.LastUsedAt)
	}
}

// TestLeaseQueuesASecondCallerFIFO is the behavior that fails without the change: a second
// session asking for a held lease is queued, not granted, and is told its place in line.
func TestLeaseQueuesASecondCallerFIFO(t *testing.T) {
	d := testdb.New(t)
	s := &devloop.LeaseService{Pool: d.Pool}
	ctx := context.Background()
	if _, err := s.Acquire(ctx, "session-a"); err != nil {
		t.Fatal(err)
	}
	b, err := s.Acquire(ctx, "session-b")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != devloop.LeaseQueued || b.HolderSession != "session-a" || b.Position != 1 {
		t.Fatalf("second caller = %+v, want queued behind session-a at position 1", b)
	}
	c, err := s.Acquire(ctx, "session-c")
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != devloop.LeaseQueued || c.Position != 2 || c.QueueLength != 2 {
		t.Fatalf("third caller = %+v, want queued at position 2 of 2", c)
	}
}

// TestLeaseReleaseHandsOffToTheEarliestQueuedCaller proves release does not grant the lease to
// whichever session happens to call next: it is held free until the earliest queued caller asks.
func TestLeaseReleaseHandsOffToTheEarliestQueuedCaller(t *testing.T) {
	d := testdb.New(t)
	s := &devloop.LeaseService{Pool: d.Pool}
	ctx := context.Background()
	if _, err := s.Acquire(ctx, "session-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire(ctx, "session-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire(ctx, "session-c"); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, "session-a"); err != nil {
		t.Fatal(err)
	}
	// session-c calls first but must not jump session-b's place in line.
	c, err := s.Acquire(ctx, "session-c")
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != devloop.LeaseQueued || c.HolderSession != "session-b" {
		t.Fatalf("session-c after release = %+v, want still queued behind session-b", c)
	}
	b, err := s.Acquire(ctx, "session-b")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != devloop.LeaseAcquired || b.HolderSession != "session-b" {
		t.Fatalf("session-b after release = %+v, want acquired", b)
	}
}

// TestLeaseExpiredIdleLeaseIsTakenOver proves an idle-expired holder loses the lease to the
// earliest queued caller, without anyone calling Release.
func TestLeaseExpiredIdleLeaseIsTakenOver(t *testing.T) {
	d := testdb.New(t)
	s := &devloop.LeaseService{Pool: d.Pool}
	ctx := context.Background()
	if _, err := s.Acquire(ctx, "session-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire(ctx, "session-b"); err != nil {
		t.Fatal(err)
	}
	// Simulate 20+ minutes of inactivity by backdating the lease and shortening its timeout,
	// without going through Acquire/Release: this is the "expired, not released" case.
	if _, err := d.Pool.Exec(ctx,
		`UPDATE devloop_lease SET last_used_at = now() - interval '1 hour', idle_timeout_seconds = 1 WHERE id = $1`,
		devloop.LeaseID); err != nil {
		t.Fatal(err)
	}
	b, err := s.Acquire(ctx, "session-b")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != devloop.LeaseAcquired || b.HolderSession != "session-b" {
		t.Fatalf("session-b after expiry = %+v, want acquired", b)
	}
	status, err := s.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.HolderSession != "session-b" {
		t.Fatalf("Status = %+v, want held by session-b", status)
	}
}

func TestLeaseReleaseByNonHolderIsANoOp(t *testing.T) {
	d := testdb.New(t)
	s := &devloop.LeaseService{Pool: d.Pool}
	ctx := context.Background()
	if _, err := s.Acquire(ctx, "session-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, "session-b"); err != nil {
		t.Fatal(err)
	}
	status, err := s.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.HolderSession != "session-a" {
		t.Fatalf("Status after a stranger's release = %+v, want still held by session-a", status)
	}
}

// TestLeaseUseHeartbeatsTheHolderAndRefusesEveryoneElse proves dev_sync's lease check: the holder
// is accepted and its idle clock restarts, any other session is refused, and a holder whose idle
// timeout ran out is refused once another session is next in line.
func TestLeaseUseHeartbeatsTheHolderAndRefusesEveryoneElse(t *testing.T) {
	d := testdb.New(t)
	s := &devloop.LeaseService{Pool: d.Pool}
	ctx := context.Background()
	if err := s.Use(ctx, "session-a"); !errors.Is(err, devloop.ErrNotHolder) {
		t.Fatalf("Use with a free lease = %v, want ErrNotHolder", err)
	}
	if _, err := s.Acquire(ctx, "session-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Use(ctx, "session-b"); !errors.Is(err, devloop.ErrNotHolder) {
		t.Fatalf("Use by a stranger = %v, want ErrNotHolder", err)
	}
	if _, err := d.Pool.Exec(ctx, `UPDATE devloop_lease SET last_used_at = now() - interval '10 minutes' WHERE id = $1`, devloop.LeaseID); err != nil {
		t.Fatal(err)
	}
	if err := s.Use(ctx, "session-a"); err != nil {
		t.Fatalf("Use by the holder = %v", err)
	}
	var idle float64
	if err := d.Pool.QueryRow(ctx, `SELECT extract(epoch FROM now() - last_used_at) FROM devloop_lease WHERE id = $1`, devloop.LeaseID).Scan(&idle); err != nil {
		t.Fatal(err)
	}
	if idle > 60 {
		t.Errorf("Use left the lease idle for %.0fs, want its clock restarted", idle)
	}

	// Expired with nobody waiting: the holder keeps it. Expired with another session waiting: lost.
	if _, err := d.Pool.Exec(ctx, `UPDATE devloop_lease SET last_used_at = now() - interval '1 hour' WHERE id = $1`, devloop.LeaseID); err != nil {
		t.Fatal(err)
	}
	if err := s.Use(ctx, "session-a"); err != nil {
		t.Fatalf("Use by an expired holder with an empty queue = %v, want accepted", err)
	}
	if _, err := s.Acquire(ctx, "session-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `UPDATE devloop_lease SET last_used_at = now() - interval '1 hour' WHERE id = $1`, devloop.LeaseID); err != nil {
		t.Fatal(err)
	}
	if err := s.Use(ctx, "session-a"); !errors.Is(err, devloop.ErrNotHolder) {
		t.Fatalf("Use by an expired holder with session-b waiting = %v, want ErrNotHolder", err)
	}
}

// backdateQueueEntry makes session's queue entry look like it was last polled age ago.
func backdateQueueEntry(t *testing.T, d *testdb.DB, session string, age time.Duration) {
	t.Helper()
	if _, err := d.Pool.Exec(context.Background(),
		`UPDATE devloop_lease_queue SET last_seen_at = now() - make_interval(secs => $2) WHERE session_id = $1`,
		session, age.Seconds()); err != nil {
		t.Fatal(err)
	}
}

// TestReviewAbandonedQueueEntryIsGrantedTheLease is the reviewer's repro: a caller queues once and
// never polls again. When the holder releases, the lease must go to the live caller, not sit
// reserved for the one that left.
func TestReviewAbandonedQueueEntryIsGrantedTheLease(t *testing.T) {
	d := testdb.New(t)
	s := &devloop.LeaseService{Pool: d.Pool}
	ctx := context.Background()
	if _, err := s.Acquire(ctx, "holder"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire(ctx, "gone"); err != nil { // queues once, then stops polling
		t.Fatal(err)
	}
	backdateQueueEntry(t, d, "gone", 3*devloop.QueueStaleAfter)
	if err := s.Release(ctx, "holder"); err != nil {
		t.Fatal(err)
	}
	live, err := s.Acquire(ctx, "live")
	if err != nil {
		t.Fatal(err)
	}
	if live.Status != devloop.LeaseAcquired || live.HolderSession != "live" {
		t.Fatalf("live after release = %+v, want acquired (not queued behind an abandoned entry)", live)
	}
}

// TestLeaseQueuePollKeepsAnEntryFresh proves polling is what keeps a place in line: an entry that
// is polled again within the stale window keeps its place; one that is not loses it.
func TestLeaseQueuePollKeepsAnEntryFresh(t *testing.T) {
	d := testdb.New(t)
	s := &devloop.LeaseService{Pool: d.Pool}
	ctx := context.Background()
	for _, id := range []string{"holder", "polling", "silent"} {
		if _, err := s.Acquire(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	backdateQueueEntry(t, d, "polling", devloop.QueueStaleAfter-10*time.Second)
	backdateQueueEntry(t, d, "silent", devloop.QueueStaleAfter+10*time.Second)
	if _, err := s.Acquire(ctx, "polling"); err != nil { // the poll bumps last_seen_at
		t.Fatal(err)
	}
	status, err := s.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.QueueLength != 1 {
		t.Fatalf("queue length = %d, want 1 (the silent entry is stale)", status.QueueLength)
	}
	if err := s.Release(ctx, "holder"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Acquire(ctx, "polling")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != devloop.LeaseAcquired {
		t.Fatalf("polling after release = %+v, want acquired", got)
	}
}

// TestLeaseHoldsAppliesTheIdleExpiryRuleOfUse proves Holds (the claim-time check of a sync) agrees
// with Use: an idle-expired holder with someone fresh waiting no longer holds, and an expired
// holder with nobody waiting still does.
func TestLeaseHoldsAppliesTheIdleExpiryRuleOfUse(t *testing.T) {
	d := testdb.New(t)
	s := &devloop.LeaseService{Pool: d.Pool}
	ctx := context.Background()
	if _, err := s.Acquire(ctx, "session-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx,
		`UPDATE devloop_lease SET last_used_at = now() - interval '1 hour', idle_timeout_seconds = 1 WHERE id = $1`,
		devloop.LeaseID); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Holds(ctx, "session-a"); err != nil || !ok {
		t.Fatalf("Holds with an expired lease and nobody waiting = %v, %v, want true", ok, err)
	}
	if _, err := s.Acquire(ctx, "session-b"); err != nil && !errors.Is(err, devloop.ErrNotHolder) {
		t.Fatal(err)
	}
	// session-b's Acquire took the expired lease over; session-a no longer holds it.
	if ok, err := s.Holds(ctx, "session-a"); err != nil || ok {
		t.Fatalf("Holds for the displaced holder = %v, %v, want false", ok, err)
	}
	// An expired lease with a fresh waiter that has not taken over yet: Holds and Use must agree.
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO devloop_lease_queue (lease_id, session_id) VALUES ($1, 'session-c')`, devloop.LeaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx,
		`UPDATE devloop_lease SET last_used_at = now() - interval '1 hour', idle_timeout_seconds = 1 WHERE id = $1`,
		devloop.LeaseID); err != nil {
		t.Fatal(err)
	}
	holds, _ := s.Holds(ctx, "session-b")
	useErr := s.Use(ctx, "session-b")
	if holds != (useErr == nil) || holds {
		t.Fatalf("Holds = %v, Use err = %v, want both to refuse the expired holder", holds, useErr)
	}
}

// TestSessionEndDropsItsQueueEntryAndHoldership proves RevokeInTx, the hook every session-ending
// path calls, releases the lease the session held and removes its place in line, so a session that
// ended does not keep the dev environment blocked for the idle timeout.
func TestSessionEndDropsItsQueueEntryAndHoldership(t *testing.T) {
	d := testdb.New(t)
	s := &devloop.LeaseService{Pool: d.Pool}
	ctx := context.Background()
	for _, id := range []string{"holder", "waiting"} {
		if _, err := s.Acquire(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := agentauth.RevokeInTx(ctx, dbq.New(d.Pool), "waiting"); err != nil {
		t.Fatal(err)
	}
	if status, err := s.Status(ctx); err != nil || status.QueueLength != 0 {
		t.Fatalf("after the waiting session ended: %+v, %v, want an empty queue", status, err)
	}
	if _, err := agentauth.RevokeInTx(ctx, dbq.New(d.Pool), "holder"); err != nil {
		t.Fatal(err)
	}
	status, err := s.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.HolderSession != "" {
		t.Fatalf("after the holder ended: %+v, want the lease free", status)
	}
	next, err := s.Acquire(ctx, "next")
	if err != nil || next.Status != devloop.LeaseAcquired {
		t.Fatalf("Acquire after the holder ended = %+v, %v, want acquired", next, err)
	}
}
