package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// lockKey is the one place a lock name becomes a 64-bit advisory-lock key. $1 is the name. Every
// advisory lock in Mainloop goes through [TryLock] or [XactLock].
const lockKey = "hashtextextended($1, 0)"

const unlockTimeout = 5 * time.Second

// Lock is a session-scoped advisory lock held on one pinned pool connection. Session locks live
// on a connection, not in a transaction, so the connection must stay out of the pool until
// [Lock.Unlock].
type Lock struct {
	conn *pgxpool.Conn
	name string
}

// TryLock takes the named session lock without waiting. When another session holds it, it
// returns (nil, false, nil): a sweep that loses skips its turn. Call Unlock on the result.
func TryLock(ctx context.Context, pool *pgxpool.Pool, name string) (*Lock, bool, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("db: lock %q: acquire connection: %w", name, err)
	}
	var got bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock("+lockKey+")", name).Scan(&got); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("db: lock %q: %w", name, err)
	}
	if !got {
		conn.Release()
		return nil, false, nil
	}
	return &Lock{conn: conn, name: name}, true, nil
}

// Unlock releases the lock and returns the connection to the pool. It works even when the
// caller's context is already cancelled. If the lock cannot be released cleanly the connection is
// closed instead, which drops every lock it held.
func (l *Lock) Unlock(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unlockTimeout)
	defer cancel()
	var released bool
	err := l.conn.QueryRow(ctx, "SELECT pg_advisory_unlock("+lockKey+")", l.name).Scan(&released)
	if err != nil || !released {
		_ = l.conn.Conn().Close(ctx)
		l.conn.Release()
		if err != nil {
			return fmt.Errorf("db: unlock %q: %w", l.name, err)
		}
		return fmt.Errorf("db: unlock %q: lock was not held", l.name)
	}
	l.conn.Release()
	return nil
}

// XactLock takes the named lock for the rest of the transaction, waiting if another transaction
// holds it. Postgres releases it at commit or rollback.
func XactLock(ctx context.Context, tx pgx.Tx, name string) error {
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock("+lockKey+")", name); err != nil {
		return fmt.Errorf("db: xact lock %q: %w", name, err)
	}
	return nil
}
