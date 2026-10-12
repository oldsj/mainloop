package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// lockKey is the one place a lock name becomes a 64-bit advisory-lock key. $1 is the name. At
// run time every advisory lock is transaction-scoped ([XactLock]); only the migrator takes a
// session lock, on its own connection outside the pool. A reconcile sweep's exclusivity is a
// lease row (sweep_leases), so no pooled connection is pinned while a sweep calls out.
const lockKey = "hashtextextended($1, 0)"

// unlockTimeout bounds releasing the migrator's session lock.
const unlockTimeout = 5 * time.Second

// XactLock takes the named lock for the rest of the transaction, waiting if another transaction
// holds it. Postgres releases it at commit or rollback.
func XactLock(ctx context.Context, tx pgx.Tx, name string) error {
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock("+lockKey+")", name); err != nil {
		return fmt.Errorf("db: xact lock %q: %w", name, err)
	}
	return nil
}
