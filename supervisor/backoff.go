package supervisor

import "time"

// Backoff defaults, applied field by field to a zero or partially filled
// BackoffConfig by withDefaults. They yield a capped exponential schedule that
// starts at half a second, doubles each attempt, tops out at thirty seconds,
// and spreads each delay by plus or minus twenty percent so a fleet of clients
// that dropped together does not reconnect in lockstep.
const (
	// DefaultBase is the delay before the first reconnect attempt.
	DefaultBase = 500 * time.Millisecond
	// DefaultMax is the ceiling the exponential growth is clamped to.
	DefaultMax = 30 * time.Second
	// DefaultFactor is the per-attempt multiplier of the exponential growth.
	DefaultFactor = 2.0
	// DefaultJitter is the fractional half-width of the random spread applied
	// to each delay: 0.2 means the actual delay lands within plus or minus 20%
	// of the capped exponential value.
	DefaultJitter = 0.2
)

// BackoffConfig parameterizes the capped exponential backoff with jitter the
// supervisor waits between reconnect attempts. Every field is optional: a zero
// or negative value falls back to the matching Default, so the zero
// BackoffConfig is the default schedule.
type BackoffConfig struct {
	// Base is the delay before the first reconnect attempt and the unit the
	// exponential growth multiplies. Zero or negative uses DefaultBase.
	Base time.Duration
	// Max is the ceiling on the delay: the exponential growth is clamped here
	// and jitter never pushes a delay above it. Zero or negative uses
	// DefaultMax.
	Max time.Duration
	// Factor is the multiplier applied per consecutive failed attempt. A value
	// of 2.0 doubles the delay each time. Values at or below 1.0 fall back to
	// DefaultFactor, since a non-growing factor would defeat the backoff.
	Factor float64
	// Jitter is the fractional half-width of the random spread applied to each
	// delay, in [0.0, 1.0): 0.2 spreads a delay by plus or minus 20%. A zero or
	// negative value uses DefaultJitter, and a value at or above 1.0 is clamped
	// to just under 1.0 so a delay can never be driven negative.
	Jitter float64
}

// withDefaults returns a copy of b with each unset field replaced by its
// Default. It is applied once in newWithClock, so the run loop reads a fully
// resolved schedule and never re-checks for zero fields.
func (b BackoffConfig) withDefaults() BackoffConfig {
	out := b
	if out.Base <= 0 {
		out.Base = DefaultBase
	}
	if out.Max <= 0 {
		out.Max = DefaultMax
	}
	if out.Factor <= 1.0 {
		out.Factor = DefaultFactor
	}
	// Zero or negative Jitter is unset and defaults. Clamp anything at or above
	// 1.0 to just under it so the symmetric spread can never make a delay
	// negative.
	switch {
	case out.Jitter <= 0:
		out.Jitter = DefaultJitter
	case out.Jitter >= 1.0:
		out.Jitter = 0.999
	}
	return out
}

// delay returns the backoff duration for the given attempt number (1 for the
// first reconnect, 2 for the second consecutive failure, and so on), spread by
// jitter drawn from r, a value in [0.0, 1.0). b is assumed already resolved by
// withDefaults.
//
// The exponential term is computed by repeated multiplication rather than
// math.Pow, breaking as soon as it reaches Max, so a large attempt count can
// never overflow the float to +Inf (which jitter would then turn into NaN).
// The jitter is symmetric: r == 0.5 yields exactly the capped value, r == 0
// the lower edge (capped * (1 - Jitter)), and r approaching 1 the upper edge.
// The result is clamped to [0, Max], so Max stays a hard ceiling even after
// jitter and a delay is never negative.
func (b BackoffConfig) delay(attempt int, r float64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	maxF := float64(b.Max)
	d := float64(b.Base)
	for i := 1; i < attempt; i++ {
		d *= b.Factor
		if d >= maxF {
			d = maxF
			break
		}
	}
	if d > maxF {
		d = maxF
	}
	if b.Jitter > 0 {
		d += d * b.Jitter * (2*r - 1)
	}
	if d > maxF {
		d = maxF
	}
	if d < 0 {
		d = 0
	}
	return time.Duration(d)
}
