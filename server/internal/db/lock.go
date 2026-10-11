package db

import (
	"context"
	"errors"
	"fmt"
	"sync"
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
	mu   sync.Mutex
	conn *pgxpool.Conn // nil once unlocked
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

// Unlock releases the lock and returns the connection to the pool. A second call is a no-op. It
// works even when the caller's context is already cancelled. If the lock cannot be released
// cleanly the connection is closed instead, which drops every lock it held.
func (l *Lock) Unlock(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn == nil {
		return nil
	}
	conn := l.conn
	l.conn = nil
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unlockTimeout)
	defer cancel()
	var released bool
	err := conn.QueryRow(ctx, "SELECT pg_advisory_unlock("+lockKey+")", l.name).Scan(&released)
	if err != nil || !released {
		_ = conn.Conn().Close(ctx)
		conn.Release()
		if err != nil {
			return fmt.Errorf("db: unlock %q: %w", l.name, err)
		}
		return fmt.Errorf("db: unlock %q: lock was not held", l.name)
	}
	conn.Release()
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

// WithTryLock runs fn while holding the named session lock, and releases it afterwards. When
// another session holds the lock it returns (false, nil) without running fn. Prefer it to
// [TryLock]: the lock cannot be forgotten, and a forgotten lock pins a pool connection for the
// life of the process.
func WithTryLock(ctx context.Context, pool *pgxpool.Pool, name string, fn func(ctx context.Context) error) (ran bool, err error) {
	l, ok, err := TryLock(ctx, pool, name)
	if err != nil || !ok {
		return false, err
	}
	defer func() { err = errors.Join(err, l.Unlock(ctx)) }()
	return true, fn(ctx)
}
