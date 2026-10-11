package sessions

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/config"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// Ledger is the read side of the delivery ledger that the workspace lifecycle consults. It needs
// only the database, so the workspace service can be built before the session service, which in
// turn wakes workspaces: the two never need each other at construction. It implements
// workspaces.Deliveries.
type Ledger struct {
	Pool *pgxpool.Pool
	Opts Options
}

// NewLedger builds the ledger from settings.
func NewLedger(cfg *config.Config, pool *pgxpool.Pool) *Ledger {
	return &Ledger{Pool: pool, Opts: OptionsFrom(cfg.Sessions)}
}

// HasOpen reports whether the session has a turn that idle suspend must not interrupt: a delivery
// that is recorded, sending or delivered, or an uncertain one still inside its hold (a task may be
// running).
func (l *Ledger) HasOpen(ctx context.Context, sessionID string) (bool, error) {
	_, err := dbq.New(l.Pool).GetBlockingDelivery(ctx, dbq.GetBlockingDeliveryParams{
		SessionID: sessionID, HoldSeconds: l.Opts.withDefaults().UncertainHold.Seconds(),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// LastActivity is the newest update of any delivery of the session, or the zero time when it has
// none.
func (l *Ledger) LastActivity(ctx context.Context, sessionID string) (time.Time, error) {
	at, err := dbq.New(l.Pool).GetDeliveryActivity(ctx, sessionID)
	if err != nil {
		return time.Time{}, err
	}
	if at.Unix() <= 0 {
		return time.Time{}, nil
	}
	return at, nil
}
