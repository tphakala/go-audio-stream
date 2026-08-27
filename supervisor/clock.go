package supervisor

import (
	"context"
	"math/rand/v2"
	"time"
)

// clock is the internal seam that isolates the supervisor from real time, so
// the backoff sequence and the reconnect loop can be driven deterministically
// in tests without importing any new dependency. Production uses realClock;
// tests substitute a fake through newWithClock. It carries the two
// time-dependent operations the supervisor needs: an interruptible sleep and
// the jitter source.
type clock interface {
	// sleep blocks for d or until ctx is cancelled, whichever comes first. It
	// returns nil when the full duration elapsed and ctx.Err() when ctx
	// cancelled first, so the caller can tell a completed backoff from an
	// interrupted one (a Close during a backoff gap) without a second check.
	sleep(ctx context.Context, d time.Duration) error
	// rand returns a value in [0.0, 1.0) for backoff jitter. It is a method on
	// the clock so a test can stub the jitter to a fixed point and assert the
	// exact backoff durations.
	rand() float64
}

// realClock is the production clock: a real timer for sleep and math/rand/v2
// for jitter.
type realClock struct{}

// sleep waits for d using a single timer, returning early with ctx.Err() when
// ctx cancels first. A non-positive d returns immediately (still reporting a
// prior ctx cancellation), so a zero backoff never parks the loop.
func (realClock) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// rand returns a uniformly distributed float in [0.0, 1.0) from math/rand/v2,
// whose top-level generator is safe for concurrent use and needs no seeding.
func (realClock) rand() float64 {
	return rand.Float64()
}
