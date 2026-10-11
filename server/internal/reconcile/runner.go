package reconcile

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"runtime/debug"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db"
)

// Outcome is what a handler wants done with the row it handled.
type Outcome struct {
	kind  outcomeKind
	state string
	after time.Duration
}

type outcomeKind int

const (
	outcomeRetry outcomeKind = iota // the zero Outcome
	outcomeDone
	outcomeRetryAfter
	outcomeRecorded
)

// Done moves the row to state and releases the lease.
func Done(state string) Outcome { return Outcome{kind: outcomeDone, state: state} }

// Retry keeps the row in its state and makes it due again after the loop's backoff for the
// attempt count.
func Retry() Outcome { return Outcome{kind: outcomeRetry} }

// RetryAfter is [Retry] with a delay the handler chose.
func RetryAfter(d time.Duration) Outcome { return Outcome{kind: outcomeRetryAfter, after: d} }

// Recorded means the handler already recorded the outcome itself, usually in a transaction that
// also wrote the domain change, using [Claim.Finish] or [Claim.RetryAfter] on that transaction.
func Recorded() Outcome { return Outcome{kind: outcomeRecorded} }

// Handler does the side effect for one claimed row. It runs with no transaction open. Its ctx is
// cancelled if the lease is lost or the runner stops. An error counts as [Retry] (or as the
// [RetryAfter] returned with it); so does a panic. A handler must be safe to run again for the same row (see the package comment).
type Handler func(ctx context.Context, c Claim) (Outcome, error)

// Loop drives one intent table.
type Loop struct {
	Name   string
	Table  Table
	Handle Handler
	// Concurrency is how many rows are handled at once (default 4).
	Concurrency int
	// Lease is how long a claim holds a row before another claimer may take it (default 30s). A
	// keepalive extends it while a handler runs, so it only needs to cover a crash.
	Lease time.Duration
	// Interval is the poll backstop for wake-ups that never arrive (default 5s).
	Interval time.Duration
	// Wake lists notification channels that make the loop look for work now.
	Wake []string
	// Backoff paces retries; the zero value is [DefaultBackoff].
	Backoff Backoff
}

// Sweep is periodic housekeeping that one process at a time runs.
type Sweep struct {
	Name string
	// Every is the period (the next run is jittered by +-10%).
	Every time.Duration
	Run   func(ctx context.Context) error
}

// Runner runs loops and sweeps until its context ends. A failure in one loop or sweep never
// stops the others.
type Runner struct {
	pool     *pgxpool.Pool
	log      *slog.Logger
	notifier *Notifier
	owner    string
	loops    []Loop
	sweeps   []Sweep
	wakes    [][]<-chan struct{}
}

// NewRunner builds a Runner. notifier may be nil, which leaves only the poll. Add loops before
// Run; Notifier subscriptions are taken by Add, so the Notifier must not be running yet.
func NewRunner(pool *pgxpool.Pool, log *slog.Logger, notifier *Notifier) *Runner {
	return &Runner{pool: pool, log: log, notifier: notifier, owner: newOwner()}
}

// Owner is the lease owner name this runner claims under.
func (r *Runner) Owner() string { return r.owner }

// newOwner is unique per runner, so two runners in one process (tests) or two pods never share
// a lease.
func newOwner() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return "runner-" + hex.EncodeToString(b)
}

// Add registers a loop.
func (r *Runner) Add(l Loop) {
	if l.Name == "" || l.Handle == nil {
		panic("reconcile: a loop needs a name and a handler")
	}
	var wakes []<-chan struct{}
	if r.notifier != nil {
		for _, c := range l.Wake {
			wakes = append(wakes, r.notifier.Subscribe(c))
		}
	}
	r.loops = append(r.loops, l)
	r.wakes = append(r.wakes, wakes)
}

// AddSweep registers a sweep.
func (r *Runner) AddSweep(s Sweep) {
	if s.Name == "" || s.Run == nil || s.Every <= 0 {
		panic("reconcile: a sweep needs a name, a period and a function")
	}
	r.sweeps = append(r.sweeps, s)
}

// Run runs everything until ctx is cancelled and the handlers in flight have returned.
func (r *Runner) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for i, l := range r.loops {
		wg.Go(func() { r.runLoop(ctx, l, r.wakes[i]) })
	}
	for _, s := range r.sweeps {
		wg.Go(func() { r.runSweep(ctx, s) })
	}
	wg.Wait()
	return nil
}

func (r *Runner) runLoop(ctx context.Context, l Loop, wakes []<-chan struct{}) {
	l = l.withDefaults()
	log := r.log.With("loop", l.Name)
	slots := make(chan struct{}, l.Concurrency)
	freed := make(chan struct{}, 1)
	var wg sync.WaitGroup
	defer wg.Wait()

	// Merge every wake channel into one.
	wake := make(chan struct{}, 1)
	for _, w := range wakes {
		wg.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-w:
					select {
					case wake <- struct{}{}:
					default:
					}
				}
			}
		})
	}
	failures := 0
	for ctx.Err() == nil {
		free := l.Concurrency - len(slots)
		wait := l.Interval
		if free > 0 {
			claims, err := l.Table.Claim(ctx, r.pool, r.owner, l.Lease, free)
			switch {
			case ctx.Err() != nil:
				return
			case err != nil:
				failures++
				wait = max(wait, l.Backoff.Delay(failures))
				log.WarnContext(ctx, "claim failed", "error", err)
			default:
				failures = 0
				for _, c := range claims {
					slots <- struct{}{}
					wg.Go(func() {
						defer func() {
							<-slots
							select {
							case freed <- struct{}{}:
							default:
							}
						}()
						r.handle(ctx, l, log, c)
					})
				}
				if len(claims) == free {
					continue // the table may hold more
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-freed:
		case <-time.After(wait):
		}
	}
}

func (l Loop) withDefaults() Loop {
	if l.Concurrency <= 0 {
		l.Concurrency = 4
	}
	if l.Lease <= 0 {
		l.Lease = 30 * time.Second
	}
	if l.Interval <= 0 {
		l.Interval = 5 * time.Second
	}
	return l
}

// handle runs the handler for one claim with a lease keepalive, then records the outcome.
func (r *Runner) handle(ctx context.Context, l Loop, log *slog.Logger, c Claim) {
	log = log.With("id", c.ID, "attempt", c.Attempts)
	hctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopKeepalive := r.keepalive(hctx, cancel, l.Lease, log, c)
	out, err := runHandler(hctx, l.Handle, c)
	stopKeepalive()
	if err != nil {
		log.WarnContext(ctx, "handler failed", "error", err)
		// A failure never settles the row, but the handler may have picked the retry delay.
		if out.kind != outcomeRetryAfter {
			out = Retry()
		}
	}

	// Record even when shutting down: the handler's result is not lost to a cancelled context.
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer rcancel()
	switch out.kind {
	case outcomeRecorded:
		return
	case outcomeDone:
		err = c.Finish(rctx, r.pool, out.state)
	case outcomeRetryAfter:
		err = c.RetryAfter(rctx, r.pool, out.after)
	default:
		err = c.RetryAfter(rctx, r.pool, l.Backoff.Delay(c.Attempts))
	}
	switch {
	case errors.Is(err, ErrLeaseLost):
		log.WarnContext(ctx, "lease lost before the outcome was recorded", "error", err)
	case err != nil:
		// The lease expires and the row is claimed again.
		log.ErrorContext(ctx, "could not record the outcome", "error", err)
	}
}

// runHandler turns a panic into an error.
func runHandler(ctx context.Context, h Handler, c Claim) (out Outcome, err error) {
	defer func() {
		if p := recover(); p != nil {
			out, err = Retry(), fmt.Errorf("handler panic: %v\n%s", p, debug.Stack())
		}
	}()
	return h(ctx, c)
}

// keepalive extends the lease every third of a lease until the returned stop function is called.
// If the lease is lost it cancels the handler.
func (r *Runner) keepalive(ctx context.Context, cancel context.CancelFunc, lease time.Duration, log *slog.Logger, c Claim) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(lease / 3)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				_, err := c.Extend(ctx, r.pool, lease)
				if errors.Is(err, ErrLeaseLost) {
					log.WarnContext(ctx, "lease lost while handling", "error", err)
					cancel()
					return
				}
				if err != nil && ctx.Err() == nil {
					log.WarnContext(ctx, "lease extension failed", "error", err)
				}
			}
		}
	}()
	return func() { close(done); <-finished }
}

func (r *Runner) runSweep(ctx context.Context, s Sweep) {
	log := r.log.With("sweep", s.Name)
	// Spread the first runs so a restart does not fire every sweep at once.
	next := time.Duration(mrand.Float64() * float64(min(s.Every, 10*time.Second))) //nolint:gosec // jitter
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(next):
		}
		r.sweepOnce(ctx, log, s)
		next = time.Duration(float64(s.Every) * (0.9 + 0.2*mrand.Float64())) //nolint:gosec // jitter
	}
}

// sweepOnce runs the sweep if no other session is running it. A failure or panic is logged and
// the next period tries again.
func (r *Runner) sweepOnce(ctx context.Context, log *slog.Logger, s Sweep) {
	ran, err := db.WithTryLock(ctx, r.pool, "sweep:"+s.Name, func(ctx context.Context) (err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("sweep panic: %v\n%s", p, debug.Stack())
			}
		}()
		return s.Run(ctx)
	})
	switch {
	case ctx.Err() != nil:
	case err != nil:
		log.WarnContext(ctx, "sweep failed", "error", err)
	case !ran:
		log.DebugContext(ctx, "sweep skipped: another process holds it")
	}
}

// RunSweep runs one sweep once under its lock and reports whether it ran. It exists for tests
// and for one-shot commands such as sweep-orphans.
func (r *Runner) RunSweep(ctx context.Context, s Sweep) (bool, error) {
	return db.WithTryLock(ctx, r.pool, "sweep:"+s.Name, s.Run)
}
