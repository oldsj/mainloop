package reconcile

import (
	"math/rand/v2"
	"time"
)

// Backoff is capped exponential backoff with jitter.
type Backoff struct {
	// Base is the delay after the first failed attempt.
	Base time.Duration
	// Max caps the delay, jitter included.
	Max time.Duration
	// Jitter is the fraction (0 to 1) by which a delay may move either way. At the cap it can
	// only move down.
	Jitter float64
}

// DefaultBackoff is used for the zero Backoff.
var DefaultBackoff = Backoff{Base: time.Second, Max: 5 * time.Minute, Jitter: 0.2}

// Delay is the wait after the given failed attempt (1 for the first).
func (b Backoff) Delay(attempt int) time.Duration {
	return b.delay(attempt, rand.Float64()) //nolint:gosec // jitter, not a secret
}

// delay is [Backoff.Delay] with the random draw (in [0, 1)) supplied, for tests.
func (b Backoff) delay(attempt int, draw float64) time.Duration {
	if b.Base <= 0 && b.Max <= 0 {
		b = DefaultBackoff
	}
	if b.Base <= 0 {
		b.Base = time.Second
	}
	if b.Max < b.Base {
		b.Max = b.Base
	}
	d := b.Base
	for i := 1; i < attempt && d < b.Max; i++ {
		d *= 2
	}
	d = min(d, b.Max)
	j := min(max(b.Jitter, 0), 1)
	d = time.Duration(float64(d) * (1 + j*(2*draw-1)))
	return min(max(d, 0), b.Max)
}
