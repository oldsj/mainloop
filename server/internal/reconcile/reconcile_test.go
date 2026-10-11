package reconcile_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

const (
	statePending = "pending"
	stateDone    = "done"
)

var intents = reconcile.NewTable("test_intents", statePending)

// newIntents creates the example domain table: the intent-row shape from the package comment.
func newIntents(t *testing.T, n int) *testdb.DB {
	t.Helper()
	d := testdb.New(t)
	ctx := context.Background()
	_, err := d.Pool.Exec(ctx, `
CREATE TABLE test_intents (
    id text PRIMARY KEY,
    idempotency_key text NOT NULL UNIQUE,
    state text NOT NULL CHECK (state = ANY (ARRAY['pending','done'])),
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_owner text,
    lease_until timestamptz);
-- The claim index (see the package comment): only rows that can still be claimed.
CREATE INDEX test_intents_claim ON test_intents (next_attempt_at, id) WHERE state IN ('pending')`)
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if _, err := d.Pool.Exec(ctx, `INSERT INTO test_intents (id, idempotency_key, state) VALUES ($1, $1 || '-key', 'pending')`,
			fmt.Sprintf("i%04d", i)); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func TestBackoffIsCappedAndJittered(t *testing.T) {
	b := reconcile.Backoff{Base: time.Second, Max: time.Minute, Jitter: 0.25}
	for attempt := 1; attempt <= 40; attempt++ {
		lo, hi := math.MaxInt64, 0
		for range 200 {
			d := int(b.Delay(attempt))
			lo, hi = min(lo, d), max(hi, d)
		}
		if time.Duration(hi) > time.Minute {
			t.Fatalf("attempt %d: delay %v exceeds the cap", attempt, time.Duration(hi))
		}
		if attempt == 1 && (time.Duration(lo) < 750*time.Millisecond || time.Duration(hi) > 1250*time.Millisecond) {
			t.Errorf("attempt 1 range [%v, %v], want about 1s +-25%%", time.Duration(lo), time.Duration(hi))
		}
		if attempt == 1 && lo == hi {
			t.Error("no jitter")
		}
	}
	if d := (reconcile.Backoff{Base: time.Second, Max: time.Minute}).Delay(10); d != time.Minute {
		t.Errorf("no-jitter delay at attempt 10 = %v, want the cap", d)
	}
}

func TestClaimTakesDueUnleasedRowsOnce(t *testing.T) {
	d := newIntents(t, 3)
	ctx := context.Background()
	got, err := intents.Claim(ctx, d.Pool, "a", time.Minute, 2)
	if err != nil || len(got) != 2 {
		t.Fatalf("first claim = %d rows, %v", len(got), err)
	}
	for _, c := range got {
		if c.Attempts != 1 || c.Owner != "a" || c.State != statePending {
			t.Errorf("claim = %+v", c)
		}
	}
	rest, err := intents.Claim(ctx, d.Pool, "b", time.Minute, 10)
	if err != nil || len(rest) != 1 {
		t.Fatalf("second claim = %d rows, %v; want the one unclaimed row", len(rest), err)
	}
	if more, _ := intents.Claim(ctx, d.Pool, "b", time.Minute, 10); len(more) != 0 {
		t.Fatalf("leased rows were claimed again: %+v", more)
	}
	// Not due yet: a retry delay hides the row.
	if err := got[0].RetryAfter(ctx, d.Pool, time.Hour); err != nil {
		t.Fatal(err)
	}
	if more, _ := intents.Claim(ctx, d.Pool, "b", time.Minute, 10); len(more) != 0 {
		t.Fatalf("a row that is not due was claimed: %+v", more)
	}
	// Settled rows are not claimable.
	if err := got[1].Finish(ctx, d.Pool, stateDone); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `UPDATE test_intents SET next_attempt_at = now()`); err != nil {
		t.Fatal(err)
	}
	again, err := intents.Claim(ctx, d.Pool, "c", time.Minute, 10)
	if err != nil || len(again) != 1 || again[0].ID != got[0].ID || again[0].Attempts != 2 {
		t.Fatalf("claim after retry = %+v, %v", again, err)
	}
}

func TestExpiredLeaseIsReclaimedAndFencesTheOldClaimer(t *testing.T) {
	d := newIntents(t, 1)
	ctx := context.Background()
	crashed, err := intents.Claim(ctx, d.Pool, "crashed", 200*time.Millisecond, 1)
	if err != nil || len(crashed) != 1 {
		t.Fatalf("claim = %v, %v", crashed, err)
	}
	if got, _ := intents.Claim(ctx, d.Pool, "other", time.Minute, 1); len(got) != 0 {
		t.Fatal("claimed a row under a live lease")
	}
	time.Sleep(300 * time.Millisecond)
	taken, err := intents.Claim(ctx, d.Pool, "other", time.Minute, 1)
	if err != nil || len(taken) != 1 || taken[0].Attempts != 2 || taken[0].ID != crashed[0].ID {
		t.Fatalf("reclaim = %+v, %v", taken, err)
	}
	// The crashed claimer wakes up late: nothing it records may land.
	if err := crashed[0].Finish(ctx, d.Pool, stateDone); !errors.Is(err, reconcile.ErrLeaseLost) {
		t.Errorf("late Finish = %v, want ErrLeaseLost", err)
	}
	if _, err := crashed[0].Extend(ctx, d.Pool, time.Minute); !errors.Is(err, reconcile.ErrLeaseLost) {
		t.Errorf("late Extend = %v, want ErrLeaseLost", err)
	}
	if err := crashed[0].RetryAfter(ctx, d.Pool, 0); !errors.Is(err, reconcile.ErrLeaseLost) {
		t.Errorf("late RetryAfter = %v, want ErrLeaseLost", err)
	}
	if err := taken[0].Finish(ctx, d.Pool, stateDone); err != nil {
		t.Errorf("current claimer Finish = %v", err)
	}
	var state string
	if err := d.Pool.QueryRow(ctx, `SELECT state FROM test_intents`).Scan(&state); err != nil || state != stateDone {
		t.Errorf("state = %q, %v", state, err)
	}
}

// N claimers racing over M rows: every attempt is handed to exactly one claimer, and with every
// handler finishing, every row is handled exactly once.
func TestContentionHandlesEachIntentOnce(t *testing.T) {
	const workers, rows = 8, 120
	d := newIntents(t, rows)
	ctx := context.Background()
	var (
		mu      sync.Mutex
		handled = map[string]int{}
		wg      sync.WaitGroup
	)
	for w := range workers {
		wg.Go(func() {
			owner := fmt.Sprintf("w%d", w)
			for {
				claims, err := intents.Claim(ctx, d.Pool, owner, time.Minute, 5)
				if err != nil {
					t.Errorf("%s: claim: %v", owner, err)
					return
				}
				if len(claims) == 0 {
					return
				}
				for _, c := range claims {
					mu.Lock()
					handled[c.ID]++
					mu.Unlock()
					if err := c.Finish(ctx, d.Pool, stateDone); err != nil {
						t.Errorf("%s: finish %s: %v", owner, c.ID, err)
					}
				}
			}
		})
	}
	wg.Wait()
	if len(handled) != rows {
		t.Fatalf("handled %d distinct rows, want %d", len(handled), rows)
	}
	for id, n := range handled {
		if n != 1 {
			t.Errorf("%s handled %d times", id, n)
		}
	}
	var pending int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM test_intents WHERE state <> 'done'`).Scan(&pending); err != nil || pending != 0 {
		t.Errorf("unsettled rows = %d, %v", pending, err)
	}
}

// The same with the runner: several runners (separate lease owners) over one table, each with
// concurrent handlers. Handlers sleep, so the loops genuinely overlap.
func TestRunnersShareATableWithoutDuplicates(t *testing.T) {
	const runners, rows = 4, 80
	d := newIntents(t, rows)
	var (
		mu      sync.Mutex
		handled = map[string]int{}
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var all sync.WaitGroup
	for range runners {
		r := reconcile.NewRunner(d.Pool, quiet(), nil)
		r.Add(reconcile.Loop{
			Name: "example", Table: intents, Concurrency: 4, Interval: 20 * time.Millisecond,
			Handle: func(_ context.Context, c reconcile.Claim) (reconcile.Outcome, error) {
				time.Sleep(5 * time.Millisecond)
				mu.Lock()
				handled[c.ID]++
				mu.Unlock()
				return reconcile.Done(stateDone), nil
			},
		})
		all.Go(func() { _ = r.Run(ctx) })
	}
	waitFor(t, 20*time.Second, "all rows settled", func() bool {
		var n int
		return d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM test_intents WHERE state = 'done'`).Scan(&n) == nil && n == rows
	})
	cancel()
	all.Wait()
	mu.Lock()
	defer mu.Unlock()
	for id, n := range handled {
		if n != 1 {
			t.Errorf("%s handled %d times", id, n)
		}
	}
	if len(handled) != rows {
		t.Errorf("handled %d rows, want %d", len(handled), rows)
	}
}

// The example reconciler: a flaky remote that fails twice per intent, then succeeds. The
// idempotency key is what the remote dedups on, so retries never apply an effect twice.
func TestExampleReconcilerRetriesWithBackoffUntilDone(t *testing.T) {
	d := newIntents(t, 3)
	remote := &fakeRemote{failFirst: 2, applied: map[string]int{}, calls: map[string]int{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := reconcile.NewRunner(d.Pool, quiet(), nil)
	r.Add(reconcile.Loop{
		Name: "example", Table: intents, Interval: 20 * time.Millisecond,
		Handle: func(ctx context.Context, c reconcile.Claim) (reconcile.Outcome, error) {
			var key string
			// No transaction is open here: this read is its own statement.
			if err := d.Pool.QueryRow(ctx, `SELECT idempotency_key FROM test_intents WHERE id = $1`, c.ID).Scan(&key); err != nil {
				return reconcile.Retry(), err
			}
			if err := remote.apply(key); err != nil {
				return reconcile.RetryAfter(10 * time.Millisecond), err
			}
			return reconcile.Done(stateDone), nil
		},
	})
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	waitFor(t, 20*time.Second, "all rows settled", func() bool {
		var n int
		return d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM test_intents WHERE state = 'done'`).Scan(&n) == nil && n == 3
	})
	cancel()
	<-done
	remote.mu.Lock()
	defer remote.mu.Unlock()
	for key, n := range remote.applied {
		if n != 1 {
			t.Errorf("%s applied %d times", key, n)
		}
		if remote.calls[key] != 3 {
			t.Errorf("%s called %d times, want 3 (two failures, one success)", key, remote.calls[key])
		}
	}
	var attempts int
	if err := d.Pool.QueryRow(context.Background(), `SELECT min(attempts) FROM test_intents`).Scan(&attempts); err != nil || attempts != 3 {
		t.Errorf("attempts = %d, %v", attempts, err)
	}
}

type fakeRemote struct {
	mu        sync.Mutex
	failFirst int
	applied   map[string]int
	calls     map[string]int
}

func (f *fakeRemote) apply(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[key]++
	if f.calls[key] <= f.failFirst {
		return errors.New("remote unavailable")
	}
	f.applied[key] = 1
	return nil
}

// A handler that panics is retried like one that fails; the loop keeps running.
func TestHandlerPanicIsARetry(t *testing.T) {
	d := newIntents(t, 1)
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := reconcile.NewRunner(d.Pool, quiet(), nil)
	r.Add(reconcile.Loop{
		Name: "example", Table: intents, Interval: 20 * time.Millisecond,
		Backoff: reconcile.Backoff{Base: 10 * time.Millisecond, Max: 20 * time.Millisecond},
		Handle: func(context.Context, reconcile.Claim) (reconcile.Outcome, error) {
			if calls.Add(1) == 1 {
				panic("boom")
			}
			return reconcile.Done(stateDone), nil
		},
	})
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	waitFor(t, 20*time.Second, "row settled after the panic", func() bool {
		var n int
		return d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM test_intents WHERE state = 'done'`).Scan(&n) == nil && n == 1
	})
	cancel()
	<-done
	if calls.Load() != 2 {
		t.Errorf("handler ran %d times, want 2", calls.Load())
	}
}

// A handler that outlives its lease keeps it through the keepalive, so nobody else is handed the
// row; one whose lease is taken away is cancelled.
func TestKeepaliveHoldsTheLeaseAndLossCancelsTheHandler(t *testing.T) {
	d := newIntents(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var rival atomic.Int32
	r := reconcile.NewRunner(d.Pool, quiet(), nil)
	r.Add(reconcile.Loop{
		Name: "slow", Table: intents, Lease: 300 * time.Millisecond, Interval: 20 * time.Millisecond,
		Handle: func(ctx context.Context, c reconcile.Claim) (reconcile.Outcome, error) {
			// Outlive the lease twice over; a rival claimer polls throughout.
			stop := time.After(700 * time.Millisecond)
			for {
				select {
				case <-stop:
					return reconcile.Done(stateDone), nil
				case <-time.After(30 * time.Millisecond):
					if got, _ := intents.Claim(context.Background(), d.Pool, "rival", time.Minute, 1); len(got) > 0 {
						rival.Add(1)
					}
				case <-ctx.Done():
					return reconcile.Retry(), ctx.Err()
				}
			}
		},
	})
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	waitFor(t, 20*time.Second, "row settled", func() bool {
		var n int
		return d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM test_intents WHERE state = 'done'`).Scan(&n) == nil && n == 1
	})
	cancel()
	<-done
	if rival.Load() != 0 {
		t.Errorf("a rival claimed the row %d times while the keepalive held the lease", rival.Load())
	}

	// Lease loss: take the row away from a running handler.
	d2 := newIntents(t, 1)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	cancelled := make(chan struct{})
	started := make(chan struct{})
	r2 := reconcile.NewRunner(d2.Pool, quiet(), nil)
	r2.Add(reconcile.Loop{
		Name: "lost", Table: intents, Lease: 300 * time.Millisecond, Interval: 20 * time.Millisecond,
		Handle: func(ctx context.Context, _ reconcile.Claim) (reconcile.Outcome, error) {
			close(started)
			select {
			case <-ctx.Done():
				close(cancelled)
				return reconcile.Retry(), ctx.Err()
			case <-time.After(10 * time.Second):
				return reconcile.Done(stateDone), nil
			}
		},
	})
	done2 := make(chan struct{})
	go func() { defer close(done2); _ = r2.Run(ctx2) }()
	<-started
	if _, err := d2.Pool.Exec(context.Background(), `UPDATE test_intents SET lease_owner = 'thief', attempts = attempts + 1`); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler was not cancelled after losing its lease")
	}
	cancel2()
	<-done2
}

// Cancelling the runner stops claiming but still records the outcome of a handler already
// running: the work is not repeated after a clean shutdown.
func TestShutdownRecordsInFlightOutcomes(t *testing.T) {
	d := newIntents(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	r := reconcile.NewRunner(d.Pool, quiet(), nil)
	r.Add(reconcile.Loop{
		Name: "example", Table: intents, Interval: 20 * time.Millisecond,
		Handle: func(context.Context, reconcile.Claim) (reconcile.Outcome, error) {
			close(started)
			time.Sleep(200 * time.Millisecond)
			return reconcile.Done(stateDone), nil
		},
	})
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	<-started
	cancel()
	<-done
	var state string
	if err := d.Pool.QueryRow(context.Background(), `SELECT state FROM test_intents`).Scan(&state); err != nil || state != stateDone {
		t.Errorf("state after shutdown = %q, %v", state, err)
	}
}

// Sweeps run under an advisory lock, so only one process runs a given sweep at a time; a panic is
// contained.
func TestSweepIsSingletonAndPanicSafe(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	r := reconcile.NewRunner(d.Pool, quiet(), nil)
	inside := make(chan struct{})
	release := make(chan struct{})
	sweep := reconcile.Sweep{Name: "once", Every: time.Hour, Run: func(context.Context) error {
		close(inside)
		<-release
		return nil
	}}
	first := make(chan bool, 1)
	go func() { ran, _ := r.RunSweep(ctx, sweep); first <- ran }()
	<-inside
	r2 := reconcile.NewRunner(d.Pool, quiet(), nil)
	if ran, err := r2.RunSweep(ctx, sweep); err != nil || ran {
		t.Errorf("second runner ran the sweep concurrently: ran=%v err=%v", ran, err)
	}
	close(release)
	if !<-first {
		t.Error("first runner did not run")
	}
	// After the first finishes the lock is free again.
	again := reconcile.Sweep{Name: "once", Every: time.Hour, Run: func(context.Context) error { return nil }}
	if ran, err := r2.RunSweep(ctx, again); err != nil || !ran {
		t.Errorf("sweep after release: ran=%v err=%v", ran, err)
	}
}

func TestRunnerRunsSweepsOnTheirPeriod(t *testing.T) {
	d := testdb.New(t)
	var runs atomic.Int32
	r := reconcile.NewRunner(d.Pool, quiet(), nil)
	r.AddSweep(reconcile.Sweep{Name: "tick", Every: 50 * time.Millisecond, Run: func(context.Context) error {
		if runs.Add(1) == 1 {
			panic("first run panics")
		}
		return nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	waitFor(t, 10*time.Second, "three sweep runs", func() bool { return runs.Load() >= 3 })
	cancel()
	<-done
}

// A notification wakes a loop well before its poll interval.
func TestNotifyWakesALoop(t *testing.T) {
	d := newIntents(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := reconcile.NewNotifier(d.Pool, quiet())
	r := reconcile.NewRunner(d.Pool, quiet(), n)
	var handled atomic.Int32
	// The probe's first wake is the notifier's catch-up after LISTEN, which wakes the loop too. A
	// second wake, from a real notification on the probe channel, shows the listener is subscribed
	// and the catch-up is behind us, so only the row's own notification can wake the loop (its
	// poll is 1 h).
	probe := n.Subscribe("test_probe")
	r.Add(reconcile.Loop{
		Name: "example", Table: intents, Interval: time.Hour, Wake: []string{"test_intents"},
		Handle: func(context.Context, reconcile.Claim) (reconcile.Outcome, error) {
			handled.Add(1)
			return reconcile.Done(stateDone), nil
		},
	})
	var wg sync.WaitGroup
	wg.Go(func() { _ = n.Run(ctx) })
	wg.Go(func() { _ = r.Run(ctx) })
	select {
	case <-probe:
	case <-time.After(5 * time.Second):
		t.Fatal("the notifier never subscribed")
	}
	if _, err := d.Pool.Exec(ctx, `SELECT pg_notify('test_probe', '')`); err != nil {
		t.Fatal(err)
	}
	select {
	case <-probe:
	case <-time.After(5 * time.Second):
		t.Fatal("the probe notification never arrived")
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO test_intents (id, idempotency_key, state) VALUES ('n1', 'n1-key', 'pending')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `SELECT pg_notify('test_intents', '')`); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, "the notified row handled long before the 1h poll", func() bool { return handled.Load() == 1 })
	cancel()
	wg.Wait()
}

// The listener's connection is killed: it reconnects, wakes its subscribers (they catch up from
// the table) and delivers later notifications.
func TestNotifierReconnectsAndWakes(t *testing.T) {
	d := testdb.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := reconcile.NewNotifier(d.Pool, quiet())
	n.Backoff = reconcile.Backoff{Base: 20 * time.Millisecond, Max: 100 * time.Millisecond}
	wake := n.Subscribe("test_chan")
	done := make(chan struct{})
	go func() { defer close(done); _ = n.Run(ctx) }()

	expect := func(what string) {
		t.Helper()
		select {
		case <-wake:
		case <-time.After(10 * time.Second):
			t.Fatalf("no wake: %s", what)
		}
	}
	expect("initial wake after LISTEN")
	// Retry the notify until delivered: the first may race the LISTEN.
	waitFor(t, 10*time.Second, "a notification wake", func() bool {
		_, _ = d.Pool.Exec(ctx, `SELECT pg_notify('test_chan', '')`)
		select {
		case <-wake:
			return true
		case <-time.After(50 * time.Millisecond):
			return false
		}
	})

	// Kill every other backend of this database that is listening.
	killed := 0
	err := d.Pool.QueryRow(ctx, `
SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity
WHERE datname = current_database() AND pid <> pg_backend_pid() AND query ILIKE 'LISTEN%'`).Scan(&killed)
	if err != nil || killed == 0 {
		t.Fatalf("could not kill the listener: killed=%d err=%v", killed, err)
	}
	expect("wake after reconnect")
	waitFor(t, 10*time.Second, "a notification after reconnect", func() bool {
		_, _ = d.Pool.Exec(ctx, `SELECT pg_notify('test_chan', '')`)
		select {
		case <-wake:
			return true
		case <-time.After(50 * time.Millisecond):
			return false
		}
	})
	cancel()
	<-done
}

func waitFor(t *testing.T, timeout time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// An Extend that stalls (the database is unreachable) must not leave the handler running past the
// lease: the runner cancels it first, because a rival may claim the row the moment the lease ends.
func TestKeepaliveSelfFencesWhenExtendStalls(t *testing.T) {
	for name, extend := range map[string]func(context.Context, reconcile.Claim, time.Duration) (time.Time, error){
		"stalled": func(ctx context.Context, _ reconcile.Claim, _ time.Duration) (time.Time, error) {
			<-ctx.Done()
			return time.Time{}, ctx.Err()
		},
		"failing": func(context.Context, reconcile.Claim, time.Duration) (time.Time, error) {
			return time.Time{}, errors.New("connection refused")
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := newIntents(t, 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			const lease = 600 * time.Millisecond
			started := time.Now()
			cancelledAfter := make(chan time.Duration, 1)
			r := reconcile.NewRunner(d.Pool, quiet(), nil)
			r.SetExtend(extend)
			r.Add(reconcile.Loop{
				Name: "stalled", Table: intents, Lease: lease, Interval: 20 * time.Millisecond,
				Handle: func(hctx context.Context, _ reconcile.Claim) (reconcile.Outcome, error) {
					select {
					case <-hctx.Done():
						cancelledAfter <- time.Since(started)
						return reconcile.Retry(), hctx.Err()
					case <-time.After(10 * time.Second):
						cancelledAfter <- -1
						return reconcile.Done(stateDone), nil
					}
				},
			})
			done := make(chan struct{})
			go func() { defer close(done); _ = r.Run(ctx) }()
			select {
			case after := <-cancelledAfter:
				if after < 0 || after >= lease {
					t.Errorf("handler cancelled after %v, want before the %v lease ended", after, lease)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the handler was never cancelled")
			}
			cancel()
			<-done
		})
	}
}

// Rows claimed just as the runner shuts down are handed back at once instead of waiting out their
// lease.
func TestShutdownReleasesClaimsItNeverStarted(t *testing.T) {
	d := newIntents(t, 3)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := reconcile.NewRunner(d.Pool, quiet(), nil)
	r.Add(reconcile.Loop{
		Name: "x", Table: intents, Lease: time.Hour, Interval: time.Hour,
		Handle: func(context.Context, reconcile.Claim) (reconcile.Outcome, error) {
			return reconcile.Done(stateDone), nil
		},
	})
	reconcile.ClaimThenCancel(r, cancel)
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Run(ctx) }()
	<-done
	var leased int
	if err := d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM test_intents WHERE lease_until > now()`).Scan(&leased); err != nil || leased != 0 {
		t.Errorf("%d rows still leased after shutdown (%v)", leased, err)
	}
}
