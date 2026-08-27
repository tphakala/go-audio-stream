package supervisor

import (
	"testing"
	"time"
)

// TestBackoffWithDefaults confirms the zero and partial BackoffConfig fill in
// from the exported Default constants, and that a deliberate zero Jitter is
// preserved while a negative one defaults.
func TestBackoffWithDefaults(t *testing.T) {
	t.Parallel()

	got := BackoffConfig{}.withDefaults()
	want := BackoffConfig{Base: DefaultBase, Max: DefaultMax, Factor: DefaultFactor, Jitter: DefaultJitter}
	if got != want {
		t.Fatalf("zero config withDefaults = %+v, want %+v", got, want)
	}

	// An explicit field survives; the rest fill in from the defaults.
	partial := BackoffConfig{Base: time.Second}.withDefaults()
	if partial.Base != time.Second {
		t.Errorf("Base = %v, want 1s (an explicit value must survive)", partial.Base)
	}
	if partial.Max != DefaultMax || partial.Factor != DefaultFactor || partial.Jitter != DefaultJitter {
		t.Errorf("partial withDefaults = %+v, want the remaining fields defaulted", partial)
	}

	// Zero or negative Jitter defaults; a Factor at or below 1 defaults; a
	// Jitter at or above 1 is clamped just under 1 so a delay stays non-negative.
	neg := BackoffConfig{Jitter: -1, Factor: 1.0}.withDefaults()
	if neg.Jitter != DefaultJitter {
		t.Errorf("negative Jitter = %v, want DefaultJitter", neg.Jitter)
	}
	if neg.Factor != DefaultFactor {
		t.Errorf("Factor 1.0 = %v, want DefaultFactor (a non-growing factor defeats backoff)", neg.Factor)
	}
	if hi := (BackoffConfig{Jitter: 5}).withDefaults(); hi.Jitter >= 1.0 {
		t.Errorf("Jitter 5 clamped to %v, want < 1.0", hi.Jitter)
	}
}

// TestBackoffDelayExponentialAndCap checks the pure exponential growth and the
// Max ceiling with jitter stubbed to its midpoint (r == 0.5), where the spread
// is exactly zero so the delay equals the bare capped exponential value.
func TestBackoffDelayExponentialAndCap(t *testing.T) {
	t.Parallel()

	b := BackoffConfig{Base: 500 * time.Millisecond, Max: 30 * time.Second, Factor: 2.0, Jitter: 0.2}

	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 500 * time.Millisecond},
		{2, 1 * time.Second},
		{3, 2 * time.Second},
		{4, 4 * time.Second},
		{5, 8 * time.Second},
		{6, 16 * time.Second},
		{7, 30 * time.Second}, // 32s exceeds Max, capped.
		{8, 30 * time.Second}, // stays capped.
		{100, 30 * time.Second},
	}
	for _, c := range cases {
		if got := b.delay(c.attempt, 0.5); got != c.want {
			t.Errorf("delay(attempt=%d, r=0.5) = %v, want %v", c.attempt, got, c.want)
		}
	}

	// attempt < 1 is clamped to the first attempt.
	if got := b.delay(0, 0.5); got != 500*time.Millisecond {
		t.Errorf("delay(0) = %v, want Base", got)
	}
}

// TestBackoffDelayJitterBounds checks the symmetric jitter edges: r == 0 is the
// lower edge (capped * (1 - Jitter)) and r just under 1 is the upper edge,
// clamped so it never exceeds Max.
func TestBackoffDelayJitterBounds(t *testing.T) {
	t.Parallel()

	b := BackoffConfig{Base: time.Second, Max: 30 * time.Second, Factor: 2.0, Jitter: 0.2}

	// Attempt 1 (base 1s) is well below the cap, so both edges are unclamped.
	if got := b.delay(1, 0); got != 800*time.Millisecond {
		t.Errorf("delay(1, r=0) = %v, want 800ms (1s * (1 - 0.2))", got)
	}
	if got := b.delay(1, 1); got != 1200*time.Millisecond {
		t.Errorf("delay(1, r=1) = %v, want 1200ms (1s * (1 + 0.2))", got)
	}

	// A large attempt sits at the cap, so the upper jitter edge is clamped to
	// Max while the lower edge still spreads down.
	if got := b.delay(20, 1); got != 30*time.Second {
		t.Errorf("delay(20, r=1) = %v, want Max (jitter cannot exceed the cap)", got)
	}
	if got := b.delay(20, 0); got != 24*time.Second {
		t.Errorf("delay(20, r=0) = %v, want 24s (30s * (1 - 0.2))", got)
	}

	// With jitter disabled the delay is the exact capped exponential value at
	// every r.
	noJitter := BackoffConfig{Base: time.Second, Max: 30 * time.Second, Factor: 2.0, Jitter: 0}
	for _, r := range []float64{0, 0.25, 0.5, 0.75, 0.999} {
		if got := noJitter.delay(2, r); got != 2*time.Second {
			t.Errorf("no-jitter delay(2, r=%v) = %v, want 2s", r, got)
		}
	}
}
