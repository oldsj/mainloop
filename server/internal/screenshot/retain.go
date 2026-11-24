package screenshot

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

// Retention: a screenshot's image is dropped after MaxImageAge, and once a workspace holds more
// than KeepUnreported images that no report names, its oldest of those go first. The row stays,
// so a report still says what it showed; the image reads as expired.
const (
	MaxImageAge    = 7 * 24 * time.Hour
	KeepUnreported = 50
)

// RetainSweep expires screenshot images and prunes the rate limit's old attempts.
func RetainSweep(pool *pgxpool.Pool) reconcile.Sweep {
	return reconcile.Sweep{
		Name:  "preview-screenshots-retention",
		Every: 10 * time.Minute,
		Run: func(ctx context.Context) error {
			q := dbq.New(pool)
			_, expireErr := q.ExpireScreenshotImages(ctx, dbq.ExpireScreenshotImagesParams{
				MaxAgeSeconds: MaxImageAge.Seconds(), Keep: KeepUnreported,
			})
			_, pruneErr := q.PruneScreenshotAttempts(ctx)
			return errors.Join(expireErr, pruneErr)
		},
	}
}
