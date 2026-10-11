package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func TestTryLockContention(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	first, ok, err := db.TryLock(ctx, d.Pool, "sweep:test")
	if err != nil || !ok {
		t.Fatalf("first TryLock = %v, %v", ok, err)
	}
	second, ok, err := db.TryLock(ctx, d.Pool, "sweep:test")
	if err != nil || ok || second != nil {
		t.Fatalf("second TryLock = %v, %v, %v; want it refused", second, ok, err)
	}
	// A different name is independent.
	other, ok, err := db.TryLock(ctx, d.Pool, "sweep:other")
	if err != nil || !ok {
		t.Fatalf("other TryLock = %v, %v", ok, err)
	}
	if err := other.Unlock(ctx); err != nil {
		t.Fatal(err)
	}

	if err := first.Unlock(ctx); err != nil {
		t.Fatal(err)
	}
	again, ok, err := db.TryLock(ctx, d.Pool, "sweep:test")
	if err != nil || !ok {
		t.Fatalf("TryLock after unlock = %v, %v", ok, err)
	}
	if err := again.Unlock(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestUnlockAfterCancelledContext(t *testing.T) {
	d := testdb.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	l, ok, err := db.TryLock(ctx, d.Pool, "sweep:cancel")
	if err != nil || !ok {
		t.Fatalf("TryLock = %v, %v", ok, err)
	}
	cancel()
	if err := l.Unlock(ctx); err != nil {
		t.Fatalf("Unlock with cancelled context: %v", err)
	}
	again, ok, err := db.TryLock(context.Background(), d.Pool, "sweep:cancel")
	if err != nil || !ok {
		t.Fatalf("lock still held after unlock: %v, %v", ok, err)
	}
	_ = again.Unlock(context.Background())
}

func TestXactLockReleasedOnRollback(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	holder, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.XactLock(ctx, holder, "xact:test"); err != nil {
		t.Fatal(err)
	}
	// A second transaction cannot take the lock while the first holds it.
	waiter, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got bool
	if err := waiter.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))", "xact:test").Scan(&got); err != nil || got {
		t.Fatalf("lock taken while held: %v, %v", got, err)
	}
	_ = waiter.Rollback(ctx)

	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	err = db.InTx(ctx, d.Pool, func(tx pgx.Tx) error {
		return db.XactLock(ctx, tx, "xact:test")
	})
	if err != nil {
		t.Fatalf("XactLock after rollback: %v", err)
	}
}

func TestInTxRollsBackOnError(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	boom := errors.New("boom")
	err := db.InTx(ctx, d.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO topics (id, user_id, name) VALUES ('t', 'u', 'n')`); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	var n int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM topics`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("topics = %d, err = %v", n, err)
	}
}

func TestUnlockTwiceIsNoOp(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)
	l, ok, err := db.TryLock(ctx, d.Pool, "sweep:twice")
	if err != nil || !ok {
		t.Fatalf("TryLock = %v, %v", ok, err)
	}
	if err := l.Unlock(ctx); err != nil {
		t.Fatal(err)
	}
	if err := l.Unlock(ctx); err != nil {
		t.Fatalf("second Unlock: %v", err)
	}
	// The pool connection was released once, not twice: the pool still works.
	if _, err := d.Pool.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
}

func TestWithTryLock(t *testing.T) {
	ctx := context.Background()
	d := testdb.New(t)

	var inside bool
	ran, err := db.WithTryLock(ctx, d.Pool, "sweep:with", func(ctx context.Context) error {
		inside = true
		// The lock is held while fn runs: a second attempt loses.
		ran, err := db.WithTryLock(ctx, d.Pool, "sweep:with", func(context.Context) error {
			t.Error("nested fn ran while the lock was held")
			return nil
		})
		if err != nil || ran {
			t.Errorf("nested WithTryLock = %v, %v", ran, err)
		}
		return nil
	})
	if err != nil || !ran || !inside {
		t.Fatalf("WithTryLock = %v, %v (inside %v)", ran, err, inside)
	}

	// fn's error comes back, and the lock is released anyway.
	boom := errors.New("boom")
	ran, err = db.WithTryLock(ctx, d.Pool, "sweep:with", func(context.Context) error { return boom })
	if !ran || !errors.Is(err, boom) {
		t.Fatalf("WithTryLock = %v, %v; want ran with boom", ran, err)
	}
	ran, err = db.WithTryLock(ctx, d.Pool, "sweep:with", func(context.Context) error { return nil })
	if err != nil || !ran {
		t.Fatalf("lock not released after fn error: %v, %v", ran, err)
	}
}
