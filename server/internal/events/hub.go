package events

import (
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Why a subscription ended.
var (
	// ErrSlowConsumer: the subscriber's buffer filled, so it was dropped. The client resumes
	// from its last event id.
	ErrSlowConsumer = errors.New("events: subscriber too slow")
	// ErrHubClosed: the process is shutting down.
	ErrHubClosed = errors.New("events: hub closed")
	// ErrHubNotReady: the outbox reader has not started, so a stream could miss events.
	ErrHubNotReady = errors.New("events: hub not ready")
	// ErrUnsubscribed: the subscriber left.
	ErrUnsubscribed = errors.New("events: unsubscribed")
)

// Hub fans events out to subscribers. Dispatch never blocks.
type Hub struct {
	log    *slog.Logger
	buffer int

	ready chan struct{}

	mu      sync.Mutex
	subs    map[*Subscription]struct{}
	closed  bool
	isReady bool
}

// NewHub builds a Hub whose subscribers each buffer up to buffer events.
func NewHub(log *slog.Logger, buffer int) *Hub {
	return &Hub{log: log, buffer: max(buffer, 1), subs: map[*Subscription]struct{}{}, ready: make(chan struct{})}
}

// MarkReady lets subscribers in. The [Source] calls it once it has its cursor: an event committed
// before that is neither dispatched nor replayed to a stream that connected earlier, so streams
// are refused until then (503; clients retry with backoff, native EventSource does not retry a
// 503).
func (h *Hub) MarkReady() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.isReady {
		h.isReady = true
		close(h.ready)
	}
}

// Ready is closed once [Hub.MarkReady] has been called.
func (h *Hub) Ready() <-chan struct{} { return h.ready }

// Subscription receives the events of one user. Read C until Done is closed; Err then says why.
type Subscription struct {
	UserID string
	C      <-chan Event

	ch   chan Event
	hub  *Hub
	done chan struct{}
	err  error // set before done is closed
}

// Done is closed when the subscription has ended.
func (s *Subscription) Done() <-chan struct{} { return s.done }

// Err says why the subscription ended; it is nil until Done is closed.
func (s *Subscription) Err() error {
	select {
	case <-s.done:
		return s.err
	default:
		return nil
	}
}

// Close leaves the hub. It is safe to call more than once.
func (s *Subscription) Close() {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	s.hub.endLocked(s, ErrUnsubscribed)
}

// Subscribe registers a subscriber for userID. It fails with [ErrHubClosed] during shutdown and
// [ErrHubNotReady] before the source has started.
func (h *Hub) Subscribe(userID string) (*Subscription, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrHubClosed
	}
	if !h.isReady {
		return nil, ErrHubNotReady
	}
	ch := make(chan Event, h.buffer)
	s := &Subscription{UserID: userID, C: ch, ch: ch, hub: h, done: make(chan struct{})}
	h.subs[s] = struct{}{}
	return s, nil
}

// Dispatch offers ev to every subscriber of its user. A subscriber with a full buffer is dropped
// instead of waited for.
func (h *Hub) Dispatch(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		if s.UserID != ev.UserID {
			continue
		}
		select {
		case s.ch <- ev:
		default:
			h.log.Warn("dropping a slow event subscriber; it will resume from its last event id",
				"user_id", s.UserID, "event_id", ev.ID, "buffer", cap(s.ch))
			h.endLocked(s, ErrSlowConsumer)
		}
	}
}

// DispatchLive offers a live event, one that is not stored, to every subscriber of its user. It is
// Dispatch with the Live flag set, so a stream can tell it from an outbox row.
func (h *Hub) DispatchLive(userID string, typ Type, payload json.RawMessage) {
	h.Dispatch(Event{UserID: userID, Type: typ, Payload: payload, Live: true, CreatedAt: time.Now()})
}

// Close ends every subscription and refuses new ones. Streams return, so the HTTP server's
// shutdown does not wait for them.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for s := range h.subs {
		h.endLocked(s, ErrHubClosed)
	}
}

// Subscribers is the number of live subscriptions.
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

func (h *Hub) endLocked(s *Subscription, err error) {
	if _, ok := h.subs[s]; !ok {
		return
	}
	delete(h.subs, s)
	s.err = err
	close(s.done)
}
