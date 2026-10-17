package devloop_test

import (
	"context"
	"os"
	"testing"
	"time"

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
