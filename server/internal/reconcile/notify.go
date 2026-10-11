package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Notifier holds one LISTEN connection for the process and turns notifications into wake-ups.
// A wake carries no payload: whoever wakes re-reads its table. Every subscriber is also woken
// after the connection is (re)established, because notifications sent while it was down are
// gone, so a subscriber always catches up from the table after a drop.
type Notifier struct {
	pool *pgxpool.Pool
	log  *slog.Logger
	// Backoff paces reconnects.
	Backoff Backoff
	// PingInterval is how long the connection may be quiet before it is pinged.
	PingInterval time.Duration

	mu      sync.Mutex
	started bool
	subs    map[string][]chan struct{}
}

// NewNotifier builds a Notifier. Subscribe, then Run.
func NewNotifier(pool *pgxpool.Pool, log *slog.Logger) *Notifier {
	return &Notifier{
		pool: pool, log: log, subs: map[string][]chan struct{}{},
		Backoff:      Backoff{Base: 250 * time.Millisecond, Max: 10 * time.Second, Jitter: 0.2},
		PingInterval: 30 * time.Second,
	}
}

// Subscribe returns a channel that receives a wake whenever channel is notified, coalescing
// wakes that arrive while one is pending. It must be called before Run, since LISTEN is issued
// once per connection; it panics otherwise.
func (n *Notifier) Subscribe(channel string) <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.started {
		panic("reconcile: Notifier.Subscribe after Run")
	}
	ch := make(chan struct{}, 1)
	n.subs[channel] = append(n.subs[channel], ch)
	return ch
}

func (n *Notifier) wake(channel string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, ch := range n.subs[channel] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (n *Notifier) wakeAll() {
	n.mu.Lock()
	channels := make([]string, 0, len(n.subs))
	for c := range n.subs {
		channels = append(channels, c)
	}
	n.mu.Unlock()
	for _, c := range channels {
		n.wake(c)
	}
}

// Run listens until ctx is cancelled, reconnecting with backoff after any failure. It returns
// nil on cancellation.
func (n *Notifier) Run(ctx context.Context) error {
	n.mu.Lock()
	n.started = true
	channels := make([]string, 0, len(n.subs))
	for c := range n.subs {
		channels = append(channels, c)
	}
	n.mu.Unlock()

	failures := 0
	for ctx.Err() == nil {
		listening, err := n.listen(ctx, channels)
		if ctx.Err() != nil {
			return nil
		}
		if listening {
			failures = 0
		}
		failures++
		n.log.WarnContext(ctx, "notify listener dropped; reconnecting", "error", err, "failures", failures)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(n.Backoff.Delay(failures)):
		}
	}
	return nil
}

// listen runs one connection until it fails. listening reports whether LISTEN succeeded.
func (n *Notifier) listen(ctx context.Context, channels []string) (listening bool, err error) {
	pc, err := n.pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire connection: %w", err)
	}
	// Take the connection out of the pool: it carries LISTEN state for its whole life.
	conn := pc.Hijack()
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()
	for _, c := range channels {
		if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{c}.Sanitize()); err != nil {
			return false, fmt.Errorf("listen %s: %w", c, err)
		}
	}
	// Anything notified before LISTEN took effect is only in the tables.
	n.wakeAll()
	for {
		wctx, cancel := context.WithTimeout(ctx, n.PingInterval)
		note, err := conn.WaitForNotification(wctx)
		cancel()
		switch {
		case err == nil:
			n.wake(note.Channel)
		case ctx.Err() != nil:
			return true, errors.Join(ctx.Err(), err)
		case errors.Is(err, context.DeadlineExceeded):
			// Quiet for a while. A half-open connection would otherwise sit unnoticed until TCP
			// gives up; a round trip finds out now (and a failure reconnects and catches up).
			pctx, pcancel := context.WithTimeout(ctx, n.PingInterval)
			err := conn.Ping(pctx)
			pcancel()
			if err != nil {
				return true, fmt.Errorf("ping: %w", err)
			}
		default:
			return true, err
		}
	}
}
