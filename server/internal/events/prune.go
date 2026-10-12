package events

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// pruneBatch bounds one delete, so the first prune after a long outage does not hold one huge
// transaction.
const pruneBatch = 5000

// Prune deletes events created before cutoff, oldest first in batches, and records the newest
// deleted id with each batch, so a resume from inside the pruned range is recognised. It returns
// how many rows it deleted.
func Prune(ctx context.Context, pool *pgxpool.Pool, cutoff time.Time) (int, error) {
	total := 0
	for {
		var row dbq.DeleteEventsBeforeRow
		err := db.InTx(ctx, pool, func(tx pgx.Tx) (err error) {
			q := dbq.New(tx)
			if row, err = q.DeleteEventsBefore(ctx, dbq.DeleteEventsBeforeParams{Cutoff: cutoff, Batch: pruneBatch}); err != nil || row.Deleted == 0 {
				return err
			}
			return q.RaiseEventFloor(ctx, row.MaxID)
		})
		if err != nil {
			return total, fmt.Errorf("events: prune: %w", err)
		}
		total += int(row.Deleted)
		if row.Deleted < pruneBatch {
			return total, nil
		}
	}
}

// PruneSweep is the retention sweep: it keeps events for the given duration.
func PruneSweep(pool *pgxpool.Pool, retention time.Duration) reconcile.Sweep {
	return reconcile.Sweep{
		Name:  "events-retention",
		Every: 10 * time.Minute,
		Run: func(ctx context.Context) error {
			_, err := Prune(ctx, pool, time.Now().Add(-retention))
			return err
		},
	}
}
