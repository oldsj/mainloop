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
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/oldsj/mainloop/server/internal/db/dbq"
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

// Sweep is periodic housekeeping that one process at a time runs. Exclusivity is a lease row
// (sweep_leases) that the run takes, extends while it works and deletes when it ends; no database
// connection is held while the sweep calls out.
type Sweep struct {
	Name string
	// Every is the period (the next run is jittered by +-10%).
	Every time.Duration
	Run   func(ctx context.Context) error
	// Timeout bounds one run (default [DefaultSweepTimeout]). Run's ctx expires then, so a sweep
	// stuck on a dependency or on the pool ends instead of waiting forever.
	Timeout time.Duration
}

// DefaultSweepTimeout is how long one sweep run may take unless the sweep says otherwise.
const DefaultSweepTimeout = 5 * time.Minute

// SweepLease is how long a sweep lease holds before another process may take the sweep over. The
// run extends it every third of that while it works, so it only has to cover a crash.
const SweepLease = 30 * time.Second

// sweepTakeTimeout bounds taking or releasing a sweep lease: both are one short statement.
const sweepTakeTimeout = 10 * time.Second

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

	// extend pushes a claim's lease; a field so tests can stall it.
	extend func(ctx context.Context, c Claim, d time.Duration) (time.Time, error)
	// sweepLease is how long a sweep lease holds (a field so tests can shorten it).
	sweepLease time.Duration
	// extendSweep pushes a sweep lease; a field so tests can stall it.
	extendSweep func(ctx context.Context, name, holder string, d time.Duration) error
	runs        atomic.Uint64
	// afterClaim runs after each claim returns; a test hook.
	afterClaim func()
}

// NewRunner builds a Runner. notifier may be nil, which leaves only the poll. Add loops before
// Run; Notifier subscriptions are taken by Add, so the Notifier must not be running yet.
func NewRunner(pool *pgxpool.Pool, log *slog.Logger, notifier *Notifier) *Runner {
	r := &Runner{pool: pool, log: log, notifier: notifier, owner: newOwner(), sweepLease: SweepLease}
	r.extendSweep = func(ctx context.Context, name, holder string, d time.Duration) error {
		_, err := dbq.New(r.pool).ExtendSweepLease(ctx, dbq.ExtendSweepLeaseParams{Name: name, Holder: holder, LeaseSeconds: d.Seconds()})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: sweep %s held by another run", ErrLeaseLost, name)
		}
		return err
	}
	r.extend = func(ctx context.Context, c Claim, d time.Duration) (time.Time, error) {
		return c.Extend(ctx, r.pool, d)
	}
	return r
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
			sent := time.Now() // before the claim is sent: the lease runs from no earlier than this
			claims, err := l.Table.Claim(ctx, r.pool, r.owner, l.Lease, free)
			if r.afterClaim != nil {
				r.afterClaim()
			}
			switch {
			case ctx.Err() != nil:
				// Shutting down with rows already leased: hand them back instead of making the
				// next process wait out the lease.
				r.release(ctx, log, claims)
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
						r.handle(ctx, l, log, c, sent)
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
func (r *Runner) handle(ctx context.Context, l Loop, log *slog.Logger, c Claim, claimSent time.Time) {
	log = log.With("id", c.ID, "attempt", c.Attempts)
	hctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopKeepalive := r.keepalive(hctx, cancel, l.Lease, log, func(ctx context.Context) error {
		_, err := r.extend(ctx, c, l.Lease)
		return err
	}, claimSent)
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

// release makes claimed rows due again at once.
func (r *Runner) release(ctx context.Context, log *slog.Logger, claims []Claim) {
	if len(claims) == 0 {
		return
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	for _, c := range claims {
		if err := c.RetryAfter(rctx, r.pool, 0); err != nil && !errors.Is(err, ErrLeaseLost) {
			log.WarnContext(ctx, "could not release a claim on shutdown", "id", c.ID, "error", err)
		}
	}
}

// keepalive extends a lease (a claim's or a sweep's) every third of a lease until the returned stop
// function is called.
//
// It fences the handler on local time: a lease confirmed by an Extend sent at T holds until
// T+lease, so when that passes with no newer confirmation (the database is unreachable, or an
// Extend hangs) the handler is cancelled before the lease can have expired for another claimer;
// the fence is a tenth of a lease early to cover the cancellation itself. Each Extend is bounded
// by the time left until the fence. If the lease is lost outright the handler is cancelled too.
func (r *Runner) keepalive(ctx context.Context, cancel context.CancelFunc, lease time.Duration, log *slog.Logger, extend func(context.Context) error, claimSent time.Time) (stop func()) {
	kctx, kcancel := context.WithCancel(ctx)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		margin := lease / 10
		fence := claimSent.Add(lease - margin) // monotonic clock
		t := time.NewTicker(lease / 3)
		defer t.Stop()
		for {
			expired := time.NewTimer(time.Until(fence))
			select {
			case <-kctx.Done():
				expired.Stop()
				return
			case <-expired.C:
				log.ErrorContext(kctx, "lease could not be confirmed in time; cancelling the handler")
				cancel()
				return
			case <-t.C:
				expired.Stop()
			}
			sent := time.Now()
			ectx, ecancel := context.WithDeadline(kctx, fence)
			err := extend(ectx)
			ecancel()
			switch {
			case err == nil:
				fence = sent.Add(lease - margin)
			case kctx.Err() != nil:
				return
			case errors.Is(err, ErrLeaseLost):
				log.WarnContext(kctx, "lease lost while handling", "error", err)
				cancel()
				return
			default:
				log.WarnContext(kctx, "lease extension failed", "error", err)
			}
			if !time.Now().Before(fence) {
				log.ErrorContext(kctx, "lease could not be confirmed in time; cancelling the handler")
				cancel()
				return
			}
		}
	}()
	return func() { kcancel(); <-finished }
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

// sweepOnce runs the sweep if no other process is running it. A failure or panic is logged and
// the next period tries again.
func (r *Runner) sweepOnce(ctx context.Context, log *slog.Logger, s Sweep) {
	ran, err := r.RunSweep(ctx, s)
	switch {
	case ctx.Err() != nil:
	case err != nil:
		log.WarnContext(ctx, "sweep failed", "error", err)
	case !ran:
		log.DebugContext(ctx, "sweep skipped: another process holds it")
	}
}

// RunSweep runs one sweep once under its lease and reports whether it ran: false with no error
// means another run holds the lease. It exists for tests and one-shot commands as well as the
// runner's own schedule.
//
// The lease is taken, extended and released in short statements, so no connection is pinned while
// s.Run works. The lease is extended every third of [SweepLease]; if that cannot be confirmed in
// time, or another process took the sweep over, s.Run's context is cancelled. A holder that dies
// stops extending and the lease lapses on its own. s.Run's context also ends at the sweep's
// timeout.
func (r *Runner) RunSweep(ctx context.Context, s Sweep) (ran bool, err error) {
	log := r.log.With("sweep", s.Name)
	holder := fmt.Sprintf("%s/%d", r.owner, r.runs.Add(1))
	sent := time.Now() // before the take is sent: the lease runs from no earlier than this
	tctx, tcancel := context.WithTimeout(ctx, sweepTakeTimeout)
	_, err = dbq.New(r.pool).TakeSweepLease(tctx, dbq.TakeSweepLeaseParams{Name: s.Name, Holder: holder, LeaseSeconds: r.sweepLease.Seconds()})
	tcancel()
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("reconcile: take sweep lease %q: %w", s.Name, err)
	}
	// Release even when shutting down, so the next process does not wait out the lease. If this
	// fails the lease lapses by itself.
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sweepTakeTimeout)
		defer cancel()
		if rerr := dbq.New(r.pool).ReleaseSweepLease(rctx, dbq.ReleaseSweepLeaseParams{Name: s.Name, Holder: holder}); rerr != nil {
			err = errors.Join(err, fmt.Errorf("reconcile: release sweep lease %q: %w", s.Name, rerr))
		}
	}()

	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultSweepTimeout
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// The keepalive outlives the run's deadline and a shutdown: only stop() ends it, so the lease
	// lasts exactly as long as the body, even one that is slow to honour its cancellation.
	stop := r.keepalive(context.WithoutCancel(rctx), cancel, r.sweepLease, log, func(ctx context.Context) error {
		return r.extendSweep(ctx, s.Name, holder, r.sweepLease)
	}, sent)
	defer stop()
	return true, runSweepBody(rctx, s.Run)
}

// runSweepBody turns a panic into an error.
func runSweepBody(ctx context.Context, run func(context.Context) error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("sweep panic: %v\n%s", p, debug.Stack())
		}
	}()
	return run(ctx)
}
