package events

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/reconcile"
)

const sourceBatch = 500

// Source reads new outbox rows and dispatches them to a hub. It keeps a cursor, so a missed
// notification, a dropped listener connection or a restart of the listener costs latency, never
// an event: the next wake (the reconnect wakes it) reads everything after the cursor.
type Source struct {
	Pool *pgxpool.Pool
	Hub  *Hub
	Log  *slog.Logger
	// Wake receives a signal when the listener says there may be new rows (and on reconnect).
	Wake <-chan struct{}
	// Poll is the backstop period.
	Poll time.Duration

	cursor int64
	ready  bool
}

// Run dispatches until ctx is cancelled. Events committed before it started are not dispatched;
// they are replay material only. It marks the hub ready once it has read its starting cursor: the
// hub refuses streams until then, so no stream can connect in a window where events go neither to
// it (not dispatched yet) nor to its replay (it started after them).
func (s *Source) Run(ctx context.Context) error {
	poll := s.Poll
	if poll <= 0 {
		poll = 5 * time.Second
	}
	var backoff reconcile.Backoff
	failures := 0
	for {
		for !s.ready {
			id, err := dbq.New(s.Pool).LatestEventID(ctx)
			if err == nil {
				s.cursor, s.ready = id, true
				s.Hub.MarkReady()
				break
			}
			if !s.wait(ctx, &failures, err, backoff) {
				return nil
			}
		}
		err := s.catchUp(ctx)
		wait := poll
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			failures++
			s.Log.WarnContext(ctx, "event source: read failed", "error", err, "failures", failures)
			wait = max(wait, backoff.Delay(failures))
		} else {
			failures = 0
		}
		select {
		case <-ctx.Done():
			return nil
		case <-s.Wake:
		case <-time.After(wait):
		}
	}
}

func (s *Source) wait(ctx context.Context, failures *int, err error, b reconcile.Backoff) bool {
	*failures++
	s.Log.WarnContext(ctx, "event source: read cursor failed", "error", err, "failures", *failures)
	select {
	case <-ctx.Done():
		return false
	case <-s.Wake: // a wake (the listener reconnected) is a reason to try again now
		return true
	case <-time.After(b.Delay(*failures)):
		return true
	}
}

// catchUp dispatches every row after the cursor, in id order.
func (s *Source) catchUp(ctx context.Context) error {
	q := dbq.New(s.Pool)
	for {
		rows, err := q.ListEventsAfter(ctx, dbq.ListEventsAfterParams{ID: s.cursor, Limit: sourceBatch})
		if err != nil {
			return fmt.Errorf("events: read outbox: %w", err)
		}
		for _, r := range rows {
			s.Hub.Dispatch(fromRow(r.ID, r.UserID, r.Type, r.Payload, r.CreatedAt))
			s.cursor = r.ID
		}
		if len(rows) < sourceBatch {
			return nil
		}
	}
}
