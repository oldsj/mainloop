package events

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// Prune deletes events created before cutoff and records the newest deleted id, so a resume from
// inside the pruned range is recognised. It returns how many rows it deleted.
func Prune(ctx context.Context, pool *pgxpool.Pool, cutoff time.Time) (int, error) {
	var n int
	err := db.InTx(ctx, pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		ids, err := q.DeleteEventsBefore(ctx, cutoff)
		if err != nil || len(ids) == 0 {
			return err
		}
		n = len(ids)
		return q.RaiseEventFloor(ctx, slices.Max(ids))
	})
	if err != nil {
		return 0, fmt.Errorf("events: prune: %w", err)
	}
	return n, nil
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
