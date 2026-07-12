package cloudretry

import (
	"math"
	"math/rand"
	"time"
)

// Backoff computes the jittered exponential backoff delay between inline
// retry attempts (task spec step 2, verbatim: "backoff 1s->2s->4s ± 20%
// jitter"). Base/Factor/JitterFrac all have safe zero-value defaults (see
// Delay), and Rand is injectable so tests can assert exact bounds without
// relying on real randomness or real wall-clock sleeps.
type Backoff struct {
	// Base is the first retry's delay before jitter (default 1s).
	Base time.Duration
	// Factor is the exponential growth rate between successive attempts
	// (default 2.0 — 1s, 2s, 4s, 8s, ...).
	Factor float64
	// JitterFrac is the +/- fraction of the computed delay applied as
	// jitter (default 0.2 = ±20%).
	JitterFrac float64
	// Rand returns a float64 in [0,1); defaults to math/rand's
	// package-level source. Tests inject a deterministic function (e.g.
	// always 0.5 for zero jitter, or 0/1 for the exact ±20% extremes).
	Rand func() float64
}

// DefaultBackoff returns the task spec's exact defaults.
func DefaultBackoff() Backoff {
	return Backoff{Base: time.Second, Factor: 2, JitterFrac: 0.2, Rand: rand.Float64}
}

// Delay returns the backoff duration to wait BEFORE the (attempt+1)-th
// upstream try, given that attempt tries have already failed (1-based:
// attempt=1 -> ~1s, attempt=2 -> ~2s, attempt=3 -> ~4s, matching the task
// spec's own "1s->2s->4s" sequence). Never negative. attempt<1 is treated
// as 1.
func (b Backoff) Delay(attempt int) time.Duration {
	base := b.Base
	if base <= 0 {
		base = time.Second
	}
	factor := b.Factor
	if factor <= 0 {
		factor = 2
	}
	jitterFrac := b.JitterFrac
	if jitterFrac < 0 {
		jitterFrac = 0
	}
	if attempt < 1 {
		attempt = 1
	}

	d := float64(base) * math.Pow(factor, float64(attempt-1))

	r := b.Rand
	if r == nil {
		r = rand.Float64
	}
	// jitter in [-jitterFrac, +jitterFrac).
	jitter := (r()*2 - 1) * jitterFrac
	d += d * jitter
	if d < 0 {
		d = 0
	}
	return time.Duration(d)
}
