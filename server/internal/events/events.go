package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// Channel is the notification channel Publish signals and the Source listens on. The payload is
// the event id; readers do not trust it and re-read the table.
const Channel = "mainloop_events"

// Type is a stored SSE event name. `connected` and `heartbeat` are frames of one stream and are
// never stored.
type Type string

// The persisted event names, matching the x-sse-events table in api/openapi.yaml.
const (
	TypeInboxUpdated    Type = "inbox:updated"
	TypeScheduleUpdated Type = "schedule:updated"
	TypeSessionMessage  Type = "session:message"
	// TypeSessionProgress is live, never stored: it is dispatched to subscribers and carries no id.
	TypeSessionProgress  Type = "session:progress"
	TypeTaskUpdated      Type = "task:updated"
	TypeWorkspaceUpdated Type = "workspace:updated"
)

var persisted = map[Type]bool{
	TypeInboxUpdated: true, TypeScheduleUpdated: true, TypeSessionMessage: true,
	TypeTaskUpdated: true, TypeWorkspaceUpdated: true,
}

// Event is one outbox row, or a live event ([Event.Live]).
type Event struct {
	ID int64
	// Live marks an event that is not stored: it has no id, is not replayed and is sent without an
	// `id:` line, so it never moves the client's resume point.
	Live      bool
	UserID    string
	Type      Type
	Payload   json.RawMessage
	CreatedAt time.Time
}

func fromRow(id int64, userID, typ string, payload json.RawMessage, at time.Time) Event {
	return Event{ID: id, UserID: userID, Type: Type(typ), Payload: payload, CreatedAt: at}
}

// Publish writes an event for userID in tx and signals listeners when tx commits. payload is
// marshalled to a JSON object matching the event's schema in the contract. It returns the id.
//
// Call it last in the transaction, or use [InTxPublish], which does (see the package comment).
func Publish(ctx context.Context, tx pgx.Tx, userID string, typ Type, payload any) (int64, error) {
	if userID == "" {
		return 0, errors.New("events: publish: empty user id")
	}
	if !persisted[typ] {
		return 0, fmt.Errorf("events: publish: %q is not a stored event type", typ)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("events: publish %s: %w", typ, err)
	}
	if len(body) == 0 || body[0] != '{' {
		return 0, fmt.Errorf("events: publish %s: payload must be a JSON object", typ)
	}
	q := dbq.New(tx)
	// The insert takes the outbox lock itself, before the id is drawn.
	row, err := q.InsertEvent(ctx, dbq.InsertEventParams{UserID: userID, Type: string(typ), Payload: body})
	if err != nil {
		return 0, fmt.Errorf("events: publish %s: %w", typ, err)
	}
	if err := q.NotifyEvent(ctx, dbq.NotifyEventParams{Channel: Channel, Payload: strconv.FormatInt(row.ID, 10)}); err != nil {
		return 0, fmt.Errorf("events: notify: %w", err)
	}
	return row.ID, nil
}

// Draft is an event a domain function wants published with its change.
type Draft struct {
	UserID  string
	Type    Type
	Payload any
}

// InTxPublish runs a domain change and publishes its events in one transaction. fn does the
// domain writes and returns the events they imply; they are published after fn returns, as the
// last statements before commit, which is where the outbox lock must be taken. Domain slices use
// this instead of calling [Publish] themselves.
func InTxPublish(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) ([]Draft, error)) error {
	return db.InTx(ctx, pool, func(tx pgx.Tx) error {
		drafts, err := fn(tx)
		if err != nil {
			return err
		}
		for _, d := range drafts {
			if _, err := Publish(ctx, tx, d.UserID, d.Type, d.Payload); err != nil {
				return err
			}
		}
		return nil
	})
}
