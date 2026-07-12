package cloudretry

import (
	"testing"
	"time"
)

// zeroJitter always returns 0.5 - Delay's jitter term becomes
// (0.5*2-1)*frac == 0, i.e. exactly the unjittered base*factor^n value.
func zeroJitter() float64 { return 0.5 }

func TestBackoffDelaySequenceNoJitter(t *testing.T) {
	b := Backoff{Base: time.Second, Factor: 2, JitterFrac: 0.2, Rand: zeroJitter}
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 1 * time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
	}
	for _, c := range cases {
		if got := b.Delay(c.attempt); got != c.want {
			t.Errorf("Delay(%d) = %v, want %v", c.attempt, got, c.want)
		}
	}
}

func TestBackoffDelayJitterBounds(t *testing.T) {
	// Rand returning 0 -> jitter = -1*frac -> exactly base*(1-0.2).
	low := Backoff{Base: time.Second, Factor: 2, JitterFrac: 0.2, Rand: func() float64 { return 0 }}
	if got, want := low.Delay(1), 800*time.Millisecond; got != want {
		t.Errorf("low-jitter Delay(1) = %v, want %v", got, want)
	}
	// Rand returning as close to 1 as float64 allows -> jitter -> +frac ->
	// exactly base*(1+0.2) in the limit; use 0.999999999 to stay < 1.
	high := Backoff{Base: time.Second, Factor: 2, JitterFrac: 0.2, Rand: func() float64 { return 1 }}
	got := high.Delay(1)
	want := 1200 * time.Millisecond
	if got != want {
		t.Errorf("high-jitter Delay(1) = %v, want %v", got, want)
	}
}

func TestBackoffDefaults(t *testing.T) {
	b := Backoff{Rand: zeroJitter} // Base/Factor/JitterFrac all zero-value
	if got, want := b.Delay(1), 1*time.Second; got != want {
		t.Errorf("Delay(1) with zero-value Backoff = %v, want %v (default base 1s)", got, want)
	}
	if got, want := b.Delay(2), 2*time.Second; got != want {
		t.Errorf("Delay(2) with zero-value Backoff = %v, want %v (default factor 2)", got, want)
	}
}

func TestBackoffDelayNeverNegativeAndAttemptBelowOneNormalized(t *testing.T) {
	b := Backoff{Base: time.Second, Factor: 2, JitterFrac: 0.2, Rand: func() float64 { return 0 }}
	if got := b.Delay(0); got != b.Delay(1) {
		t.Errorf("Delay(0) = %v, want same as Delay(1) = %v", got, b.Delay(1))
	}
	if got := b.Delay(-5); got < 0 {
		t.Errorf("Delay(-5) = %v, want >= 0", got)
	}
}

func TestDefaultBackoffShape(t *testing.T) {
	b := DefaultBackoff()
	if b.Base != time.Second {
		t.Errorf("Base = %v, want 1s", b.Base)
	}
	if b.Factor != 2 {
		t.Errorf("Factor = %v, want 2", b.Factor)
	}
	if b.JitterFrac != 0.2 {
		t.Errorf("JitterFrac = %v, want 0.2", b.JitterFrac)
	}
	if b.Rand == nil {
		t.Error("Rand = nil, want the default math/rand source")
	}
	// Sanity: a real call must never panic and must stay within +/-20% of
	// the unjittered value across many samples.
	for attempt := 1; attempt <= 3; attempt++ {
		base := time.Second * time.Duration(1<<uint(attempt-1))
		lo := time.Duration(float64(base) * 0.8)
		hi := time.Duration(float64(base) * 1.2)
		for i := 0; i < 50; i++ {
			d := b.Delay(attempt)
			if d < lo || d > hi {
				t.Fatalf("Delay(%d) = %v, want within [%v, %v]", attempt, d, lo, hi)
			}
		}
	}
}
