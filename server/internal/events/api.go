package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/api"
	"github.com/oldsj/mainloop/server/internal/db/dbq"
	"github.com/oldsj/mainloop/server/internal/httpx"
)

const (
	// HeartbeatInterval is how long a stream may be silent before a heartbeat frame.
	HeartbeatInterval = 30 * time.Second
	replayBatch       = 200
	writeTimeout      = 15 * time.Second
	// timestampLayout is UTC ISO-8601 without an offset, as the contract says.
	timestampLayout = "2006-01-02T15:04:05.999999"
)

// EventsAPI implements the events tag: GET /events.
type EventsAPI struct {
	pool *pgxpool.Pool
	hub  *Hub
	log  *slog.Logger
	// userID is the owner; the owner API has a single user.
	userID string
	// heartbeat is HeartbeatInterval, shortened by tests.
	heartbeat time.Duration
	now       func() time.Time

	replayBatchSize int
	afterBatch      func() // test hook, runs between replay batches
}

// NewAPI builds the handler. pool and hub may be nil (a process that does not serve events), in
// which case GET /events answers 503.
func NewAPI(pool *pgxpool.Pool, hub *Hub, log *slog.Logger, ownerID string) *EventsAPI {
	return &EventsAPI{pool: pool, hub: hub, log: log, userID: ownerID, heartbeat: HeartbeatInterval, now: time.Now, replayBatchSize: replayBatch}
}

// SetHeartbeat overrides the heartbeat interval. For tests.
func (a *EventsAPI) SetHeartbeat(d time.Duration) { a.heartbeat = d }

// SetReplay overrides the replay batch size and sets a hook that runs between batches. For tests.
func (a *EventsAPI) SetReplay(batch int, afterBatch func()) {
	a.replayBatchSize, a.afterBatch = batch, afterBatch
}

// errWindowMoved: retention pruned events the replay still needed.
var errWindowMoved = errors.New("events: retention window moved past the resume point")

// replayBatch reads the next batch of the user's events after `after`. The retention floor is read
// in the same snapshot: events are only ever pruned from the bottom, so floor <= after means every
// stored event above `after` is present in this batch's view. A floor above `after` means pruning
// overtook the replay; the stream then ends, and since the client's resume point is below the
// floor it starts fresh on reconnect rather than skipping the pruned events.
func (a *EventsAPI) replayBatch(ctx context.Context, userID string, after int64) ([]dbq.Event, error) {
	tx, err := a.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	q := dbq.New(tx)
	floor, err := q.EventFloor(ctx)
	if err != nil {
		return nil, err
	}
	if floor > after {
		return nil, errWindowMoved
	}
	return q.ListUserEventsAfter(ctx, dbq.ListUserEventsAfterParams{UserID: userID, ID: after, Limit: int32(a.replayBatchSize)}) //nolint:gosec // small constant
}

// stream is the response of GetEvents: it writes frames until the stream ends.
type stream struct {
	run func(w http.ResponseWriter)
}

func (s stream) VisitGetEventsResponse(w http.ResponseWriter) error {
	s.run(w)
	return nil
}

// GetEvents implements the contract operation. See the package comment for the rules.
func (a *EventsAPI) GetEvents(ctx context.Context, req api.GetEventsRequestObject) (api.GetEventsResponseObject, error) {
	if a.pool == nil || a.hub == nil {
		return nil, httpx.NewError(http.StatusServiceUnavailable, "Events unavailable")
	}
	// Subscribe before reading the database: an event dispatched after this point reaches the
	// subscription, and one dispatched before it is committed and so visible to the reads below.
	sub, err := a.hub.Subscribe(a.userID)
	if err != nil {
		// Not ready (the outbox reader is starting) or closed (shutting down). Clients retry with
		// backoff; native EventSource does not retry a 503.
		return nil, httpx.NewError(http.StatusServiceUnavailable, "Events unavailable")
	}
	lastEventID := ""
	if req.Params.LastEventID != nil {
		lastEventID = *req.Params.LastEventID
	}
	start, err := a.start(ctx, lastEventID)
	if err != nil {
		sub.Close()
		a.log.ErrorContext(ctx, "events: start stream", "request_id", httpx.RequestID(ctx), "error", err)
		return nil, httpx.NewError(http.StatusServiceUnavailable, "Events unavailable")
	}
	return stream{run: func(w http.ResponseWriter) {
		defer sub.Close()
		a.serve(ctx, w, sub, start)
	}}, nil
}

// streamStart is where a stream begins.
type streamStart struct {
	// connectedID is the id of the connected frame: the resume point the client holds once it has
	// the frame.
	connectedID int64
	// after is the last id the client is considered to have; events after it are sent.
	after int64
	// replay says whether to send stored events after `after` before going live.
	replay bool
}

// start decides, from Last-Event-ID and the outbox, whether to resume or begin fresh.
func (a *EventsAPI) start(ctx context.Context, raw string) (streamStart, error) {
	q := dbq.New(a.pool)
	latest, err := q.LatestEventID(ctx)
	if err != nil {
		return streamStart{}, err
	}
	fresh := streamStart{connectedID: latest, after: latest}
	if raw == "" {
		return fresh, nil
	}
	last, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || last < 0 {
		return fresh, nil // not one of ours; ignore it
	}
	floor, err := q.EventFloor(ctx)
	if err != nil {
		return streamStart{}, err
	}
	switch {
	case last > latest:
		// Ids restart after a database reset: nothing to resume.
		a.log.InfoContext(ctx, "events: resume point is ahead of the outbox; starting fresh", "last_event_id", last, "latest", latest)
		return fresh, nil
	case last < floor:
		a.log.InfoContext(ctx, "events: resume point is older than the retained window; starting fresh", "last_event_id", last, "floor", floor)
		return fresh, nil
	}
	return streamStart{connectedID: last, after: last, replay: true}, nil
}

func (a *EventsAPI) serve(ctx context.Context, w http.ResponseWriter, sub *Subscription, st streamStart) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	send := func(frame []byte) bool {
		// A client that stops reading must not pin this goroutine; the deadline turns it into a
		// write error. Writers that cannot set a deadline just skip it.
		_ = rc.SetWriteDeadline(a.now().Add(writeTimeout))
		if _, err := w.Write(frame); err != nil {
			return false
		}
		err := rc.Flush()
		return err == nil || errors.Is(err, http.ErrNotSupported)
	}

	connected, _ := json.Marshal(struct {
		UserID    string `json:"user_id"`
		Timestamp string `json:"timestamp"`
	}{sub.UserID, a.now().UTC().Format(timestampLayout)})
	if !send(frame(st.connectedID, "connected", connected)) {
		return
	}
	lastSent := st.after
	heartbeat := time.NewTimer(a.heartbeat)
	defer heartbeat.Stop()
	resetHeartbeat := func() {
		if !heartbeat.Stop() {
			select {
			case <-heartbeat.C:
			default:
			}
		}
		heartbeat.Reset(a.heartbeat)
	}

	if st.replay {
		for {
			rows, err := a.replayBatch(ctx, sub.UserID, lastSent)
			if err != nil {
				if ctx.Err() == nil {
					a.log.WarnContext(ctx, "events: replay failed; the client will reconnect", "error", err)
				}
				return
			}
			for _, r := range rows {
				if !send(frame(r.ID, r.Type, r.Payload)) {
					return
				}
				lastSent = r.ID
			}
			if len(rows) < a.replayBatchSize {
				break
			}
			if a.afterBatch != nil {
				a.afterBatch()
			}
			select {
			case <-ctx.Done():
				return
			case <-sub.Done():
				return
			default:
			}
		}
		resetHeartbeat()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.Done():
			return
		case ev := <-sub.C:
			// Rows the replay already sent can arrive again from the hub.
			if ev.ID <= lastSent {
				continue
			}
			if !send(frame(ev.ID, string(ev.Type), ev.Payload)) {
				return
			}
			lastSent = ev.ID
			resetHeartbeat()
		case <-heartbeat.C:
			beat, _ := json.Marshal(struct {
				Timestamp string `json:"timestamp"`
			}{a.now().UTC().Format(timestampLayout)})
			if !send(frame(noID, "heartbeat", beat)) {
				return
			}
			heartbeat.Reset(a.heartbeat)
		}
	}
}

// noID marks a frame without an id line, which leaves the client's Last-Event-ID as it was.
// Heartbeats carry none.
const noID = -1

// frame encodes one SSE frame.
func frame(id int64, event string, data []byte) []byte {
	var b bytes.Buffer
	if id >= 0 {
		fmt.Fprintf(&b, "id: %d\n", id)
	}
	fmt.Fprintf(&b, "event: %s\n", event)
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		b.WriteString("data: ")
		b.Write(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return b.Bytes()
}
