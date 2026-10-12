package reconcile_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oldsj/mainloop/server/internal/reconcile"
	"github.com/oldsj/mainloop/server/internal/testdb"
)

func leaseUntil(t *testing.T, d *testdb.DB, name string) (holder string, until time.Time, ok bool) {
	t.Helper()
	rows, err := d.Pool.Query(context.Background(), `SELECT holder, until FROM sweep_leases WHERE name = $1`, name)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		if err := rows.Scan(&holder, &until); err != nil {
			t.Fatal(err)
		}
		return holder, until, true
	}
	return "", time.Time{}, false
}

// More slow sweeps than the pool has connections must not starve the API: a sweep that is waiting
// on a dependency holds no connection.
func TestSlowSweepsDoNotExhaustThePool(t *testing.T) {
	d := testdb.New(t)
	r := reconcile.NewRunner(d.Pool, quiet(), nil)
	n := int(d.Pool.Config().MaxConns) + 5

	inside := make(chan struct{}, n)
	hang := make(chan struct{}) // the fake kagent that never answers, until the test ends
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := reconcile.Sweep{Name: fmt.Sprintf("slow-%d", i), Every: time.Hour, Timeout: 30 * time.Second, Run: func(ctx context.Context) error {
				// A query first, as a real sweep reads its work, then the call that hangs.
				if _, err := d.Pool.Exec(ctx, "SELECT 1"); err != nil {
					return err
				}
				inside <- struct{}{}
				<-hang
				// Then the write that used to wait forever for a connection.
				_, err := d.Pool.Exec(ctx, "SELECT 1")
				return err
			}}
			ran, err := r.RunSweep(context.Background(), s)
			if err == nil && !ran {
				err = errors.New("did not run")
			}
			errs <- err
		}()
	}
	for range n {
		select {
		case <-inside:
		case <-time.After(20 * time.Second):
			t.Fatal("sweeps did not all start")
		}
	}

	// All n sweeps are in flight. The API's database ping and a query still answer.
	pctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := d.Pool.Ping(pctx); err != nil {
		t.Fatalf("ping with %d sweeps in flight: %v", n, err)
	}
	var one int
	if err := d.Pool.QueryRow(pctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("query with %d sweeps in flight: %d, %v", n, one, err)
	}
	if stat := d.Pool.Stat(); stat.AcquiredConns() != 0 {
		t.Errorf("%d connections held by sweeps in flight, want none", stat.AcquiredConns())
	}

	close(hang)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("sweep: %v", err)
		}
	}
}

// A sweep run has a deadline: a run stuck past it is cancelled and its lease is released.
func TestSweepRunHasADeadline(t *testing.T) {
	d := testdb.New(t)
	r := reconcile.NewRunner(d.Pool, quiet(), nil)
	s := reconcile.Sweep{Name: "stuck", Every: time.Hour, Timeout: 200 * time.Millisecond, Run: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	start := time.Now()
	ran, err := r.RunSweep(context.Background(), s)
	if !ran || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunSweep = %v, %v; want ran with a deadline error", ran, err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("took %v for a 200ms deadline", took)
	}
	if _, _, ok := leaseUntil(t, d, "stuck"); ok {
		t.Error("lease row left behind after the run ended")
	}
}

// Runners that race for one sweep: exactly one runs it.
func TestRacingRunnersRunASweepOnce(t *testing.T) {
	d := testdb.New(t)
	const racers = 8
	var inside sync.WaitGroup
	inside.Add(1)
	release := make(chan struct{})
	var mu sync.Mutex
	bodies := 0
	sweep := reconcile.Sweep{Name: "race", Every: time.Hour, Run: func(context.Context) error {
		mu.Lock()
		bodies++
		mu.Unlock()
		inside.Done()
		<-release
		return nil
	}}
	start := make(chan struct{})
	results := make(chan bool, racers)
	for range racers {
		r := reconcile.NewRunner(d.Pool, quiet(), nil)
		go func() {
			<-start
			ran, err := r.RunSweep(context.Background(), sweep)
			if err != nil {
				t.Error(err)
			}
			results <- ran
		}()
	}
	close(start)
	// The winner is blocked in the body, so the first racers-1 results are all the losers.
	for range racers - 1 {
		select {
		case ran := <-results:
			if ran {
				t.Fatal("a second runner ran the sweep while the first held its lease")
			}
		case <-time.After(20 * time.Second):
			t.Fatal("losing runners did not return")
		}
	}
	inside.Wait()
	close(release)
	if ran := <-results; !ran {
		t.Error("the winner reported that it did not run")
	}
	mu.Lock()
	defer mu.Unlock()
	if bodies != 1 {
		t.Errorf("sweep body ran %d times, want 1", bodies)
	}
}

// A long sweep keeps its lease alive, so a second runner stays out well past the first expiry.
func TestLeaseIsExtendedDuringALongSweep(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	const lease = 600 * time.Millisecond
	a := reconcile.NewRunner(d.Pool, quiet(), nil)
	a.SetSweepLease(lease)
	b := reconcile.NewRunner(d.Pool, quiet(), nil)
	b.SetSweepLease(lease)

	started := make(chan struct{})
	finish := make(chan struct{})
	sweep := reconcile.Sweep{Name: "long", Every: time.Hour, Run: func(ctx context.Context) error {
		close(started)
		select {
		case <-finish:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	var runErr error
	done := make(chan struct{})
	go func() { defer close(done); _, runErr = a.RunSweep(ctx, sweep) }()
	<-started

	_, first, ok := leaseUntil(t, d, "long")
	if !ok {
		t.Fatal("no lease row while the sweep runs")
	}
	// Three lease lengths of work: the original expiry passes twice over.
	began := time.Now()
	for time.Since(began) < 3*lease {
		if ran, err := b.RunSweep(ctx, sweep); err != nil || ran {
			t.Fatalf("second runner ran a live sweep: ran=%v err=%v", ran, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	_, later, ok := leaseUntil(t, d, "long")
	if !ok || !later.After(first) {
		t.Fatalf("lease not extended: %v then %v (present %v)", first, later, ok)
	}
	if leaseRemaining(t, d, "long") <= 0 {
		t.Errorf("lease already lapsed at %v", later)
	}
	close(finish)
	<-done
	if runErr != nil {
		t.Errorf("long sweep: %v", runErr)
	}
	if _, _, ok := leaseUntil(t, d, "long"); ok {
		t.Error("lease row left behind after completion")
	}
	if ran, err := b.RunSweep(ctx, reconcile.Sweep{Name: "long", Every: time.Hour, Run: func(context.Context) error { return nil }}); err != nil || !ran {
		t.Errorf("sweep after release: ran=%v err=%v", ran, err)
	}
}

// If another process takes the sweep over anyway (the lease lapsed unseen), the old run is
// cancelled and does not delete the new holder's row.
func TestLostSweepLeaseCancelsTheRun(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	r := reconcile.NewRunner(d.Pool, quiet(), nil)
	r.SetSweepLease(300 * time.Millisecond)
	started := make(chan struct{})
	sweep := reconcile.Sweep{Name: "usurped", Every: time.Hour, Run: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}
	done := make(chan error, 1)
	go func() { _, err := r.RunSweep(ctx, sweep); done <- err }()
	<-started
	if _, err := d.Pool.Exec(ctx, `UPDATE sweep_leases SET holder = 'new-holder/1', until = now() + interval '1 hour' WHERE name = 'usurped'`); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("run error = %v, want cancellation", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run was not cancelled after its lease was taken over")
	}
	if holder, _, ok := leaseUntil(t, d, "usurped"); !ok || holder != "new-holder/1" {
		t.Errorf("the old run disturbed the new holder's row: %q, %v", holder, ok)
	}
}

// leaseRemaining is how long a lease has left, measured on the database clock alone, so clock
// skew between the test and the database cannot matter. It is negative once the lease lapsed.
func leaseRemaining(t *testing.T, d *testdb.DB, name string) time.Duration {
	t.Helper()
	var secs float64
	if err := d.Pool.QueryRow(context.Background(),
		`SELECT extract(epoch FROM until - now())::float8 FROM sweep_leases WHERE name = $1`, name).Scan(&secs); err != nil {
		t.Fatal(err)
	}
	return time.Duration(secs * float64(time.Second))
}

// A holder that dies keeps its row until the lease lapses; another process takes the sweep over
// after that and not before.
func TestDeadHolderLeaseExpiresBeforeTakeover(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	if _, err := d.Pool.Exec(ctx,
		`INSERT INTO sweep_leases (name, holder, until) VALUES ('orphaned', 'dead-process/1', now() + interval '2 seconds')`); err != nil {
		t.Fatal(err)
	}
	r := reconcile.NewRunner(d.Pool, quiet(), nil)
	var runs int
	sweep := reconcile.Sweep{Name: "orphaned", Every: time.Hour, Run: func(context.Context) error { runs++; return nil }}

	// Only attempt while the database says at least half a second is left, so a late scheduler
	// cannot turn a correct refusal into a failure.
	attempts := 0
	for leaseRemaining(t, d, "orphaned") > 500*time.Millisecond {
		attempts++
		if ran, err := r.RunSweep(ctx, sweep); err != nil || ran {
			t.Fatalf("took over before the lease expired: ran=%v err=%v", ran, err)
		}
		time.Sleep(150 * time.Millisecond)
	}
	if attempts == 0 || runs != 0 {
		t.Fatalf("attempts=%d runs=%d before expiry", attempts, runs)
	}
	waitFor(t, 10*time.Second, "the dead holder's lease to lapse", func() bool { return leaseRemaining(t, d, "orphaned") < 0 })
	if ran, err := r.RunSweep(ctx, sweep); err != nil || !ran {
		t.Fatalf("no takeover after expiry: ran=%v err=%v", ran, err)
	}
	if runs != 1 {
		t.Errorf("sweep ran %d times after takeover, want 1", runs)
	}
	if _, _, ok := leaseUntil(t, d, "orphaned"); ok {
		t.Error("lease row left behind after the takeover finished")
	}
}

// A holder whose extension stalls (the database is unreachable from its side) cancels its own sweep
// before the lease can lapse for anyone else.
func TestStalledExtendCancelsTheSweepBeforeExpiry(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	a := reconcile.NewRunner(d.Pool, quiet(), nil)
	a.SetSweepLease(2 * time.Second) // the fence is 200ms before expiry
	a.SetSweepExtend(func(ctx context.Context, _, _ string, _ time.Duration) error {
		<-ctx.Done() // never confirms
		return ctx.Err()
	})
	var remaining time.Duration
	sweep := reconcile.Sweep{Name: "fenced", Every: time.Hour, Run: func(ctx context.Context) error {
		<-ctx.Done()
		// Asked on the database clock, at the moment the sweep learns it was cancelled.
		remaining = leaseRemaining(t, d, "fenced")
		return ctx.Err()
	}}
	ran, err := a.RunSweep(ctx, sweep)
	if !ran || !errors.Is(err, context.Canceled) {
		t.Fatalf("RunSweep = %v, %v; want ran and cancelled by its own fence", ran, err)
	}
	if remaining <= 0 {
		t.Errorf("sweep was cancelled %v after its lease expired", -remaining)
	}
}

// The lease lasts as long as the body, not as long as its context: a body that is slow to honour
// the deadline keeps the sweep, so a second runner stays out until the body returns.
func TestLeaseOutlivesTheDeadlineWhileTheBodyRuns(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	const lease = 900 * time.Millisecond
	a := reconcile.NewRunner(d.Pool, quiet(), nil)
	a.SetSweepLease(lease)
	b := reconcile.NewRunner(d.Pool, quiet(), nil)
	b.SetSweepLease(lease)

	deadlinePassed := make(chan struct{})
	var bodyReturned atomic.Bool
	sweep := reconcile.Sweep{Name: "stubborn", Every: time.Hour, Timeout: 200 * time.Millisecond, Run: func(ctx context.Context) error {
		<-ctx.Done()
		close(deadlinePassed)
		time.Sleep(4 * lease) // a section that ignores cancellation
		bodyReturned.Store(true)
		return ctx.Err()
	}}
	done := make(chan error, 1)
	go func() { _, err := a.RunSweep(ctx, sweep); done <- err }()
	<-deadlinePassed

	// Probe through more than three original lease lengths past the deadline.
	attempts := 0
	for !bodyReturned.Load() {
		if remaining := leaseRemaining(t, d, "stubborn"); remaining > 250*time.Millisecond {
			attempts++
			if ran, err := b.RunSweep(ctx, reconcile.Sweep{Name: "stubborn", Every: time.Hour, Run: func(context.Context) error { return nil }}); err != nil || ran {
				t.Fatalf("a second runner took the sweep while the body still ran: ran=%v err=%v", ran, err)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if attempts < 10 {
		t.Errorf("only %d probes while the body ran past its deadline", attempts)
	}
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("RunSweep error = %v, want the deadline", err)
	}
	if ran, err := b.RunSweep(ctx, reconcile.Sweep{Name: "stubborn", Every: time.Hour, Run: func(context.Context) error { return nil }}); err != nil || !ran {
		t.Errorf("sweep after the body returned: ran=%v err=%v", ran, err)
	}
}
