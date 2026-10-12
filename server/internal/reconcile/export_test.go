package reconcile

import (
	"context"
	"sync"
	"time"
)

// SetExtend replaces how the runner extends a claim's lease, so a test can stall or fail it.
func (r *Runner) SetExtend(f func(ctx context.Context, c Claim, d time.Duration) (time.Time, error)) {
	r.extend = f
}

// ClaimThenCancel runs f right after the runner's first claim returns, before it looks at the result.
func ClaimThenCancel(r *Runner, f func()) {
	var once sync.Once
	r.afterClaim = func() { once.Do(f) }
}

// SetSweepLease shortens how long a sweep lease holds.
func (r *Runner) SetSweepLease(d time.Duration) { r.sweepLease = d }

// SetSweepExtend replaces how the runner extends a sweep lease, so a test can stall or fail it.
func (r *Runner) SetSweepExtend(f func(ctx context.Context, name, holder string, d time.Duration) error) {
	r.extendSweep = f
}
