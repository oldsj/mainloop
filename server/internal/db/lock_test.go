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
