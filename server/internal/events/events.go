package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/oldsj/mainloop/server/internal/db"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
)

// Channel is the notification channel Publish signals and the Source listens on. The payload is
// the event id; readers do not trust it and re-read the table.
const Channel = "mainloop_events"

const lockName = "mainloop:events-outbox"

// Type is a stored SSE event name. `connected` and `heartbeat` are frames of one stream and are
// never stored.
type Type string

// The persisted event names, matching the x-sse-events table in api/openapi.yaml.
const (
	TypeInboxUpdated     Type = "inbox:updated"
	TypeScheduleUpdated  Type = "schedule:updated"
	TypeSessionMessage   Type = "session:message"
	TypeTaskUpdated      Type = "task:updated"
	TypeWorkspaceUpdated Type = "workspace:updated"
)

var persisted = map[Type]bool{
	TypeInboxUpdated: true, TypeScheduleUpdated: true, TypeSessionMessage: true,
	TypeTaskUpdated: true, TypeWorkspaceUpdated: true,
}

// Event is one outbox row.
type Event struct {
	ID        int64
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
// Call it last in the transaction (see the package comment).
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
	if err := db.XactLock(ctx, tx, lockName); err != nil {
		return 0, err
	}
	q := dbq.New(tx)
	row, err := q.InsertEvent(ctx, dbq.InsertEventParams{UserID: userID, Type: string(typ), Payload: body})
	if err != nil {
		return 0, fmt.Errorf("events: publish %s: %w", typ, err)
	}
	if err := q.NotifyEvent(ctx, dbq.NotifyEventParams{Channel: Channel, Payload: strconv.FormatInt(row.ID, 10)}); err != nil {
		return 0, fmt.Errorf("events: notify: %w", err)
	}
	return row.ID, nil
}
