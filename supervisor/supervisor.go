package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
)

// errNilSource is the cause recorded when a Factory returns no source and no
// error, violating its contract. It is treated as a normal session-ending cause
// (retryable under DefaultRetryable) rather than dereferenced into a panic, so a
// buggy Factory degrades to a reconnect attempt instead of a nil-pointer crash.
var errNilSource = errors.New("supervisor: Factory returned a nil source and nil error")

// Factory constructs one fully negotiated, already delivering audiostream.Source.
// The supervisor calls it once for the first connect and again for every
// reconnect, so it must perform the whole handshake (dial, describe, set up,
// play, or the HTTP open) and return a source that is delivering frames, or an
// error. It closes over the consumer's OnFrame and OnCodecUpdate callbacks, so
// delivery is wired the same on every session and starts before the returned
// source is handed back. It MUST honor ctx for the entire handshake: the
// supervisor cancels ctx to abort an in-flight connect on Close, and a Factory
// that ignores ctx would wedge the shutdown.
type Factory func(ctx context.Context) (audiostream.Source, error)

// State is the supervisor's lifecycle phase, reported through Config.OnState
// and readable at any time via Supervisor.State.
type State int

const (
	// StateConnecting is the phase while a Factory call is in flight for the
	// first connect or a reconnect. It is the initial state and recurs before
	// every attempt.
	StateConnecting State = iota
	// StateConnected is the phase while a source is live and delivering. The
	// attempt counter resets to zero on entry, so a subsequent failure starts
	// its backoff from Base again.
	StateConnected
	// StateReconnecting is the phase between a failed session and the next
	// attempt, while the backoff delay elapses. The accompanying StateChange
	// carries the Attempt number, the retryable Err that ended the session, and
	// the Backoff duration about to be waited.
	StateReconnecting
	// StateClosed is the terminal phase after Close (or a Wait-context
	// cancellation) ended the supervisor. It is reported exactly once.
	StateClosed
	// StateFailed is the terminal phase after a non-retryable error ended the
	// supervisor without a local Close, or after a recovered panic in the run
	// loop. It is reported exactly once and the accompanying Err is the cause.
	StateFailed
)

// String returns the state's name for logs and diagnostics.
func (s State) String() string {
	switch s {
	case StateConnecting:
		return "connecting"
	case StateConnected:
		return "connected"
	case StateReconnecting:
		return "reconnecting"
	case StateClosed:
		return "closed"
	case StateFailed:
		return "failed"
	default:
		return fmt.Sprintf("State(%d)", int(s))
	}
}

// StateChange is one transition delivered to Config.OnState. It is a value
// snapshot: the callback may retain it freely.
type StateChange struct {
	// State is the phase just entered.
	State State
	// Attempt is the consecutive-failure counter. It is 0 for the first
	// connect and for every StateConnected, and 1, 2, 3, ... for successive
	// StateReconnecting transitions until a connect resets it.
	Attempt int
	// Err is the cause. It is the error that ended the session on a
	// StateReconnecting and StateFailed transition, and nil on StateConnecting
	// and StateConnected. On StateClosed it is the terminal cause recorded
	// (audiostream.ErrClosed after Close, or the Wait context's error).
	Err error
	// Backoff is the delay about to be waited before the next attempt, set only
	// on a StateReconnecting transition and zero otherwise.
	Backoff time.Duration
}

// Config configures a Supervisor. Only Factory is required.
type Config struct {
	// Factory constructs each session's source. It is required; New panics if
	// it is nil.
	Factory Factory
	// Backoff parameterizes the reconnect schedule. The zero BackoffConfig is
	// the default capped-exponential schedule (see BackoffConfig).
	Backoff BackoffConfig
	// OnState, when set, is called synchronously on the supervising goroutine
	// for every state transition, in order. It must not block for long and must
	// not call back into the supervisor's Wait (which would deadlock the loop
	// it runs on); Close, Stats, Info, and State are safe to call from it. A
	// panic in OnState is recovered and does not disturb the state machine.
	OnState func(StateChange)
	// Retryable decides whether a session-ending error should trigger a
	// reconnect (true) or end the supervisor with StateFailed (false). When
	// nil, DefaultRetryable is used.
	Retryable func(error) bool
	// Logger, when set, receives debug and warning records for reconnects and
	// recovered panics. Delivery is never gated on it.
	Logger *slog.Logger
}

// Supervisor wraps a single-session audiostream.Source factory into a
// transparently reconnecting Source. It satisfies audiostream.Source, so an
// existing consumer swaps a concrete client for a Supervisor without changing
// how it waits, closes, or reads stats. Reconnection is driven by exactly one
// supervising goroutine; Wait, Close, Stats, Info, and State are safe from any
// other goroutine.
type Supervisor struct {
	factory   Factory
	onState   func(StateChange)
	backoff   BackoffConfig
	retryable func(error) bool
	logger    *slog.Logger
	clk       clock

	// runCancel cancels the run context, unblocking the Factory, the live
	// source's Wait, and a backoff sleep. It is the single shutdown lever, fired
	// once through initiateShutdown.
	runCancel context.CancelFunc

	// closeOnce funnels every terminal trigger (Close, a Wait-context cancel)
	// through initiateShutdown exactly once, first cause wins.
	closeOnce sync.Once
	// done is closed by the run goroutine as its final act, after the terminal
	// state has been emitted and termErr recorded.
	done chan struct{}

	// mu guards the fields below. It is a leaf lock: a concrete source method
	// (Stats, Info, Close) is NEVER called while it is held, so it cannot
	// invert against a source's own lock.
	mu        sync.Mutex
	termErr   error
	current   audiostream.Source
	lastStats audiostream.Stats
	lastInfo  audiostream.SourceInfo
	state     State
}

// Supervisor satisfies the root package's source-agnostic capture contract, so
// it is a drop-in for any single-session source.
var _ audiostream.Source = (*Supervisor)(nil)

// New builds a Supervisor from cfg and immediately starts connecting on a
// background goroutine, so the consumer's OnFrame can begin firing before the
// first Wait. It panics if cfg.Factory is nil, since a supervisor with nothing
// to construct can never make progress.
func New(cfg Config) *Supervisor {
	return newWithClock(cfg, realClock{})
}

// newWithClock is the constructor New delegates to, with the clock seam exposed
// so tests can drive time deterministically. It applies the backoff defaults
// and the default Retryable, starts the run goroutine, and returns the ready
// Supervisor.
func newWithClock(cfg Config, clk clock) *Supervisor {
	if cfg.Factory == nil {
		panic("supervisor: New called with a nil Factory")
	}
	s := &Supervisor{
		factory:   cfg.Factory,
		onState:   cfg.OnState,
		backoff:   cfg.Backoff.withDefaults(),
		retryable: cfg.Retryable,
		logger:    cfg.Logger,
		clk:       clk,
		done:      make(chan struct{}),
		state:     StateConnecting,
	}
	if s.retryable == nil {
		s.retryable = DefaultRetryable
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.runCancel = cancel
	go s.run(ctx)
	return s
}

// run is the single supervising goroutine. A deferred recover converts a panic
// from the user-supplied Factory or from a source's Wait into a terminal
// StateFailed rather than a process crash (see recoverRun), and the final
// deferred close(done) releases every Wait only after the terminal state has
// been emitted.
func (s *Supervisor) run(ctx context.Context) {
	defer close(s.done)
	// Cancel the run context on every exit path, including a return through
	// finish(StateFailed, ...) which does not call runCancel itself. A Factory
	// that derived a child context, timer, or goroutine from the context it was
	// handed would otherwise keep it alive until the caller also calls Close.
	// runCancel is idempotent, so this is safe alongside initiateShutdown.
	defer s.runCancel()
	defer s.recoverRun()
	s.loop(ctx)
}

// recoverRun turns a panic that escaped the run loop into a clean terminal
// failure. It closes any live source (outside mu, per the lock discipline) so a
// panic mid-session does not leak it, cancels the run context, and records the
// panic as the StateFailed cause. It runs before close(done), so a Wait unblocks
// only after the failure has been recorded.
func (s *Supervisor) recoverRun() {
	r := recover()
	if r == nil {
		return
	}
	err := fmt.Errorf("supervisor: recovered panic in run loop: %v", r)
	if s.logger != nil {
		s.logger.Warn("supervisor: recovered panic in run loop", "panic", r)
	}
	s.mu.Lock()
	cur := s.current
	s.current = nil
	s.mu.Unlock()
	if cur != nil {
		_ = cur.Close()
	}
	s.runCancel()
	s.finish(StateFailed, err)
}

// loop drives connect, deliver, and reconnect until a terminal condition. It
// returns after emitting exactly one terminal state (StateClosed on a cancel,
// StateFailed on a non-retryable error), unless a panic escapes it, in which
// case recoverRun emits the terminal StateFailed instead.
func (s *Supervisor) loop(ctx context.Context) {
	attempt := 0
	for {
		if ctx.Err() != nil {
			s.finishClosed()
			return
		}
		s.setState(StateChange{State: StateConnecting, Attempt: attempt})

		src, err := s.factory(ctx)
		if err == nil && src == nil {
			// A contract-violating Factory: surface a clear cause rather than
			// letting the loop dereference nil in src.Wait below.
			err = errNilSource
		}
		if err != nil {
			if ctx.Err() != nil {
				s.finishClosed()
				return
			}
			if !s.retryable(err) {
				s.finish(StateFailed, err)
				return
			}
			attempt++
			if !s.backoffSleep(ctx, attempt, err) {
				s.finishClosed()
				return
			}
			continue
		}

		s.setCurrent(src)
		attempt = 0
		s.setState(StateChange{State: StateConnected})

		werr := src.Wait(ctx)
		// Capture the final session stats and identity BEFORE clearing current,
		// so the last session's counters survive the reconnect gap (correction 5).
		s.captureFinal(src)
		_ = src.Close()
		s.clearCurrent()

		if ctx.Err() != nil {
			s.finishClosed()
			return
		}
		if !s.retryable(werr) {
			s.finish(StateFailed, werr)
			return
		}
		attempt++
		if !s.backoffSleep(ctx, attempt, werr) {
			s.finishClosed()
			return
		}
	}
}

// backoffSleep emits the StateReconnecting transition for attempt and cause,
// then waits out the computed backoff. It returns true when the delay elapsed
// and the loop should try again, and false when the run context cancelled
// during the wait (a Close during the gap), so the caller finishes as closed.
func (s *Supervisor) backoffSleep(ctx context.Context, attempt int, cause error) bool {
	d := s.backoff.delay(attempt, s.clk.rand())
	if s.logger != nil {
		s.logger.Debug("supervisor: scheduling reconnect", "attempt", attempt, "backoff", d, "cause", cause)
	}
	s.setState(StateChange{State: StateReconnecting, Attempt: attempt, Err: cause, Backoff: d})
	return s.clk.sleep(ctx, d) == nil
}

// setState records the new phase under mu and emits the transition. The record
// happens under the lock so a concurrent State() reads a consistent value; the
// emit happens outside the lock so a slow or reentrant OnState cannot stall a
// concurrent Stats or State.
func (s *Supervisor) setState(sc StateChange) {
	s.mu.Lock()
	s.state = sc.State
	s.mu.Unlock()
	s.emit(sc)
}

// finish records the terminal cause (first cause wins) and phase under mu and
// emits the terminal transition. It is the non-Close terminal path: StateFailed
// for a non-retryable error or a recovered panic.
//
// A concurrent Close (or a Wait-context cancel) can win the race for termErr in
// the narrow window between the loop's ctx.Err() check and this lock: only
// initiateShutdown ever pre-sets termErr, since finish and finishClosed run
// once, on the single run goroutine. So a termErr already set here means a
// deliberate shutdown won, and the terminal phase must be StateClosed to stay
// consistent with the ErrClosed (or ctx) cause Wait will return, rather than
// reporting StateFailed alongside an ErrClosed cause.
func (s *Supervisor) finish(state State, cause error) {
	s.mu.Lock()
	if s.termErr == nil {
		s.termErr = cause
	} else {
		state = StateClosed
	}
	s.state = state
	termErr := s.termErr
	s.mu.Unlock()
	s.emit(StateChange{State: state, Err: termErr})
}

// finishClosed is the terminal path taken when the run context cancelled: a
// Close or a Wait-context cancellation. The cause was already recorded by
// initiateShutdown, so this only backfills a defensive default, sets the phase,
// and emits StateClosed with the recorded cause.
func (s *Supervisor) finishClosed() {
	s.mu.Lock()
	if s.termErr == nil {
		// Unreachable while runCancel fires only from initiateShutdown, which
		// records a cause first; kept so a future caller of runCancel cannot
		// produce a nil terminal error.
		s.termErr = context.Canceled
	}
	s.state = StateClosed
	termErr := s.termErr
	s.mu.Unlock()
	s.emit(StateChange{State: StateClosed, Err: termErr})
}

// emit delivers one transition to OnState, recovering a panic from the callback
// so a misbehaving consumer cannot break the state machine or crash the run
// goroutine. A nil callback is a no-op.
func (s *Supervisor) emit(sc StateChange) {
	if s.onState == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil && s.logger != nil {
			s.logger.Warn("supervisor: recovered panic in OnState callback", "panic", r)
		}
	}()
	s.onState(sc)
}

// setCurrent publishes the live source under mu.
func (s *Supervisor) setCurrent(src audiostream.Source) {
	s.mu.Lock()
	s.current = src
	s.mu.Unlock()
}

// clearCurrent retracts the live source under mu, so Stats and Info fall back
// to the last snapshot during the reconnect gap.
func (s *Supervisor) clearCurrent() {
	s.mu.Lock()
	s.current = nil
	s.mu.Unlock()
}

// captureFinal snapshots the ending session's stats and identity for the gap.
// The source methods are called WITHOUT holding mu (the lock discipline of
// correction 4), and only the returned values are stored under mu.
func (s *Supervisor) captureFinal(src audiostream.Source) {
	st := src.Stats()
	info := src.Info()
	s.mu.Lock()
	s.lastStats = st
	s.lastInfo = info
	s.mu.Unlock()
}

// Wait blocks until the supervisor ends and returns the terminal cause:
// audiostream.ErrClosed after Close, ctx.Err() if the passed ctx cancels first,
// or the non-retryable error that ended the last session (StateFailed). The
// first cause wins. Do not call Wait from inside OnFrame or OnState. It may be
// called from more than one goroutine; all callers observe the same cause.
func (s *Supervisor) Wait(ctx context.Context) error {
	select {
	case <-s.done:
		return s.termError()
	case <-ctx.Done():
		s.initiateShutdown(ctx.Err())
		<-s.done
		return s.termError()
	}
}

// Close ends the supervisor and any live session. It is idempotent and safe
// from any goroutine, including from inside OnFrame. It cancels the run context,
// which unblocks an in-flight Factory, the live source's Wait, and any backoff
// sleep; Wait afterwards returns audiostream.ErrClosed unless an earlier cause
// had already ended the supervisor. Close returns nil.
func (s *Supervisor) Close() error {
	s.initiateShutdown(audiostream.ErrClosed)
	return nil
}

// initiateShutdown funnels every terminal trigger through one place, exactly
// once; the first cause wins. It records the cause under mu, then cancels the
// run context so the loop observes the shutdown and unwinds to a terminal state.
func (s *Supervisor) initiateShutdown(cause error) {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		if s.termErr == nil {
			s.termErr = cause
		}
		s.mu.Unlock()
		s.runCancel()
	})
}

// termError returns the recorded terminal cause, or nil if none yet.
func (s *Supervisor) termError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.termErr
}

// Stats returns the current session's counters while a source is live, and the
// final counters of the last session during a reconnect gap. Before the first
// connect it returns the zero Stats. The live source's Stats is called only
// AFTER mu is released (correction 4), so the supervisor's lock never nests
// inside a source's lock. Counters reset per reconnect (see the package doc);
// they are current-session, not cumulative.
func (s *Supervisor) Stats() audiostream.Stats {
	s.mu.Lock()
	cur := s.current
	last := s.lastStats
	s.mu.Unlock()
	if cur != nil {
		return cur.Stats()
	}
	// Clone so a caller mutating the returned map cannot corrupt the retained
	// last snapshot, honoring the Source.Stats freshly-allocated contract. A nil
	// Tracks map (before any session) clones to nil, which is the right zero.
	out := last
	out.Tracks = maps.Clone(last.Tracks)
	return out
}

// Info returns the current session's identity while a source is live, the last
// session's identity during a reconnect gap, and the zero SourceInfo before the
// first connect. Like Stats, it calls the live source's Info only after mu is
// released.
func (s *Supervisor) Info() audiostream.SourceInfo {
	s.mu.Lock()
	cur := s.current
	last := s.lastInfo
	s.mu.Unlock()
	if cur != nil {
		return cur.Info()
	}
	return last
}

// State returns the supervisor's current lifecycle phase. It is safe from any
// goroutine and is the same value most recently delivered to OnState.
func (s *Supervisor) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// DefaultRetryable is the reconnect policy used when Config.Retryable is nil. It
// treats context.Canceled, context.DeadlineExceeded, audiostream.ErrClosed, and
// audiostream.ErrRedirect as terminal (returns false), and every other error,
// including a nil error (a clean session end), audiostream.ErrReadTimeout, a
// dropped connection, and a server teardown, as retryable (returns true). The
// terminal set is the errors that will not resolve by reconnecting: a local
// cancel or close, a deadline the caller imposed, or a redirect the caller must
// act on rather than retry blindly against the same target.
func DefaultRetryable(err error) bool {
	switch {
	case errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, audiostream.ErrClosed),
		errors.Is(err, audiostream.ErrRedirect):
		return false
	default:
		return true
	}
}
