package screenshot

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// Retention: a screenshot's image is dropped after MaxImageAge; once a workspace holds more than
// KeepUnreported images that no report names, its oldest of those go first; and a workspace holds
// at most MaxHeldBytes of images, reported ones included, dropping unreported ones before
// reported ones and older before newer. The row stays, so a report still says what it showed; the
// image reads as expired. The chat copies a delivered report made go on the same schedule: with
// their screenshot's image, after MaxImageAge, and past MaxHeldBytes in one conversation.
const (
	MaxImageAge    = 7 * 24 * time.Hour
	KeepUnreported = 50
	MaxHeldBytes   = 100 << 20
)

// RetainSweep expires screenshot images, prunes their chat copies and the rate limit's old
// attempts.
func RetainSweep(pool *pgxpool.Pool) reconcile.Sweep {
	return reconcile.Sweep{
		Name:  "preview-screenshots-retention",
		Every: 10 * time.Minute,
		Run: func(ctx context.Context) error {
			q := dbq.New(pool)
			_, expireErr := q.ExpireScreenshotImages(ctx, dbq.ExpireScreenshotImagesParams{
				MaxAgeSeconds: MaxImageAge.Seconds(), Keep: KeepUnreported, MaxBytes: MaxHeldBytes,
			})
			// After the expiry, so a copy goes in the same pass as its screenshot's image.
			_, copiesErr := q.PruneScreenshotCopies(ctx, dbq.PruneScreenshotCopiesParams{
				MaxAgeSeconds: MaxImageAge.Seconds(), MaxBytes: MaxHeldBytes,
			})
			_, pruneErr := q.PruneScreenshotAttempts(ctx)
			return errors.Join(expireErr, copiesErr, pruneErr)
		},
	}
}
