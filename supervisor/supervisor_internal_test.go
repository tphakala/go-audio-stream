package supervisor

import (
	"context"
	"errors"
	"maps"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
)

// errFlaky is a retryable session-ending error the fakes return to trigger a
// reconnect. DefaultRetryable treats it as retryable (it is none of the
// terminal sentinels).
var errFlaky = errors.New("supervisor_test: flaky connection lost")

// errPermanent is a non-retryable error a custom Retryable rejects, to drive
// the StateFailed path.
var errPermanent = errors.New("supervisor_test: permanent failure")

// urlOne is the identity URL of the first fake session, reused across the
// live/gap Stats/Info assertions.
const urlOne = "rtsp://one/stream"

// fakeSource is an rtsp-free audiostream.Source for tests. Wait either returns
// its programmed error immediately or blocks until block is closed (or ctx
// cancels); entered is closed the first time Wait runs, so a test can rendezvous
// on the source becoming live.
type fakeSource struct {
	id         int
	waitErr    error
	block      chan struct{}
	entered    chan struct{}
	enterOnce  sync.Once
	closeCount atomic.Int32
	stats      audiostream.Stats
	info       audiostream.SourceInfo
}

var _ audiostream.Source = (*fakeSource)(nil)

func newFakeSource(id int) *fakeSource {
	return &fakeSource{id: id, entered: make(chan struct{})}
}

func (f *fakeSource) Wait(ctx context.Context) error {
	f.enterOnce.Do(func() { close(f.entered) })
	if f.block != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-f.block:
			return f.waitErr
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return f.waitErr
	}
}

func (f *fakeSource) Close() error {
	f.closeCount.Add(1)
	return nil
}

func (f *fakeSource) Stats() audiostream.Stats {
	// Honor the freshly-allocated contract so a caller cannot corrupt the fake.
	return audiostream.Stats{CapturedAt: f.stats.CapturedAt, Tracks: maps.Clone(f.stats.Tracks)}
}

func (f *fakeSource) Info() audiostream.SourceInfo { return f.info }

// factoryStep scripts one Factory invocation: a source to return, or an error,
// optionally blocking until block is closed (honoring ctx), with entered closed
// on the first invocation of this step so a test can rendezvous on the gap.
type factoryStep struct {
	src     *fakeSource
	err     error
	block   chan struct{}
	entered chan struct{}
	once    sync.Once
}

// fakeFactory serves scripted steps in order; the final step repeats once the
// script is exhausted. It counts invocations and records every source it
// actually returned, so a test can assert each was Closed exactly once.
type fakeFactory struct {
	mu    sync.Mutex
	steps []*factoryStep
	idx   int
	calls atomic.Int32
	made  []*fakeSource
}

func (ff *fakeFactory) make(ctx context.Context) (audiostream.Source, error) {
	ff.calls.Add(1)
	ff.mu.Lock()
	step := ff.steps[ff.idx]
	if ff.idx < len(ff.steps)-1 {
		ff.idx++
	}
	ff.mu.Unlock()

	step.once.Do(func() {
		if step.entered != nil {
			close(step.entered)
		}
	})
	if step.block != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-step.block:
		}
	}
	if step.err != nil {
		return nil, step.err
	}
	if step.src == nil {
		// Return an untyped nil interface, as a real Factory bug would, rather
		// than a typed (*fakeSource)(nil) that would read as non-nil.
		//nolint:nilnil // deliberately simulates a contract-violating Factory to exercise the nil-source guard.
		return nil, nil
	}
	ff.mu.Lock()
	ff.made = append(ff.made, step.src)
	ff.mu.Unlock()
	return step.src, nil
}

func (ff *fakeFactory) callCount() int { return int(ff.calls.Load()) }

func (ff *fakeFactory) sources() []*fakeSource {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	return append([]*fakeSource(nil), ff.made...)
}

// fakeClock drives time deterministically. In auto mode (the default) sleep
// records the duration and returns immediately; in manual mode it blocks on
// wake so a test can step the backoff one delay at a time. Every sleep reports
// its duration on entered (buffered) so a test can observe each backoff.
type fakeClock struct {
	mu      sync.Mutex
	slept   []time.Duration
	randVal float64
	auto    bool
	entered chan time.Duration
	wake    chan struct{}
}

func newFakeClock() *fakeClock {
	return &fakeClock{
		randVal: 0.5,
		auto:    true,
		entered: make(chan time.Duration, 64),
		wake:    make(chan struct{}),
	}
}

func (c *fakeClock) sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.slept = append(c.slept, d)
	auto := c.auto
	c.mu.Unlock()
	select {
	case c.entered <- d:
	default:
	}
	if auto {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.wake:
		return nil
	}
}

func (c *fakeClock) rand() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.randVal
}

func (c *fakeClock) durations() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.slept...)
}

// stateRecorder collects every StateChange delivered to OnState under a lock,
// since OnState runs on the supervising goroutine while the test reads from its
// own.
type stateRecorder struct {
	mu      sync.Mutex
	changes []StateChange
}

func (r *stateRecorder) onState(sc StateChange) {
	r.mu.Lock()
	r.changes = append(r.changes, sc)
	r.mu.Unlock()
}

func (r *stateRecorder) states() []State {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]State, len(r.changes))
	for i, c := range r.changes {
		out[i] = c.State
	}
	return out
}

func (r *stateRecorder) snapshot() []StateChange {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]StateChange(nil), r.changes...)
}

// --- test helpers ---

// mustWait waits for the supervisor to end, failing on a hang, and checks the
// terminal cause matches want (when want is non-nil).
func mustWait(t *testing.T, s *Supervisor, want error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- s.Wait(context.Background()) }()
	select {
	case err := <-done:
		if want != nil && !errors.Is(err, want) {
			t.Fatalf("Wait = %v, want error matching %v", err, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return within 5s")
	}
}

func awaitClose(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func awaitSleep(t *testing.T, c *fakeClock) time.Duration {
	t.Helper()
	select {
	case d := <-c.entered:
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a backoff sleep")
		return 0
	}
}

// --- tests ---

// TestSupervisorImplementsSource confirms the compile-time assertion is backed
// by a usable value through the interface.
func TestSupervisorImplementsSource(t *testing.T) {
	t.Parallel()
	src := newFakeSource(1)
	src.block = make(chan struct{})
	ff := &fakeFactory{steps: []*factoryStep{{src: src}}}
	var s audiostream.Source = New(Config{Factory: ff.make})
	awaitClose(t, src.entered, "first Wait entry")
	if err := s.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
	mustWait(t, s.(*Supervisor), audiostream.ErrClosed)
}

// TestNewNilFactoryPanics confirms a nil Factory is rejected loudly at
// construction rather than deferred to a later nil call.
func TestNewNilFactoryPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("New with a nil Factory did not panic")
		}
	}()
	_ = New(Config{})
}

// TestDefaultRetryable pins the terminal set and the retryable default,
// including a nil (clean end) treated as retryable.
func TestDefaultRetryable(t *testing.T) {
	t.Parallel()
	terminal := []error{
		context.Canceled,
		context.DeadlineExceeded,
		audiostream.ErrClosed,
		audiostream.ErrRedirect,
		&audiostream.RedirectError{Location: "rtsp://elsewhere/stream"},
	}
	for _, err := range terminal {
		if DefaultRetryable(err) {
			t.Errorf("DefaultRetryable(%v) = true, want false (terminal)", err)
		}
	}
	retryable := []error{nil, errFlaky, audiostream.ErrReadTimeout, errors.New("boom")}
	for _, err := range retryable {
		if !DefaultRetryable(err) {
			t.Errorf("DefaultRetryable(%v) = false, want true (retryable)", err)
		}
	}
}

// TestRealClockSleepCancel confirms the production clock returns ctx.Err() when
// the context cancels before the duration elapses.
func TestRealClockSleepCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (realClock{}).sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("sleep on cancelled ctx = %v, want context.Canceled", err)
	}
	// A completed short sleep returns nil.
	if err := (realClock{}).sleep(context.Background(), time.Millisecond); err != nil {
		t.Errorf("completed sleep = %v, want nil", err)
	}
}

// TestHappyReconnectStateOrder exercises one full reconnect and pins the OnState
// order [Connecting, Connected, Reconnecting, Connecting, Connected], terminated
// by Close with a final Closed.
func TestHappyReconnectStateOrder(t *testing.T) {
	t.Parallel()
	rec := &stateRecorder{}
	clk := newFakeClock()

	src1 := newFakeSource(1)
	src1.waitErr = errFlaky // ends immediately, retryable -> reconnect
	src2 := newFakeSource(2)
	src2.block = make(chan struct{}) // stays live until Close

	ff := &fakeFactory{steps: []*factoryStep{{src: src1}, {src: src2}}}
	s := newWithClock(Config{Factory: ff.make, OnState: rec.onState}, clk)

	awaitClose(t, src2.entered, "second session live")
	_ = s.Close()
	mustWait(t, s, audiostream.ErrClosed)

	got := rec.states()
	want := []State{StateConnecting, StateConnected, StateReconnecting, StateConnecting, StateConnected}
	if len(got) < len(want)+1 {
		t.Fatalf("states = %v, want the 5-state prefix then Closed", got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("state[%d] = %v, want %v (full: %v)", i, got[i], w, got)
		}
	}
	if last := got[len(got)-1]; last != StateClosed {
		t.Errorf("terminal state = %v, want Closed (full: %v)", last, got)
	}

	// The single reconnect carried Attempt 1 with a non-nil retryable cause and
	// a backoff duration.
	for _, sc := range rec.snapshot() {
		if sc.State == StateReconnecting {
			if sc.Attempt != 1 {
				t.Errorf("Reconnecting Attempt = %d, want 1", sc.Attempt)
			}
			if !errors.Is(sc.Err, errFlaky) {
				t.Errorf("Reconnecting Err = %v, want errFlaky", sc.Err)
			}
			if sc.Backoff <= 0 {
				t.Errorf("Reconnecting Backoff = %v, want > 0", sc.Backoff)
			}
		}
	}
}

// TestBackoffEscalation drives the manual clock and an always-failing factory,
// confirming the backoff durations escalate exactly as the schedule computes and
// that the attempt counter climbs (no Connected to reset it).
func TestBackoffEscalation(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	clk.auto = false // step one backoff at a time

	ff := &fakeFactory{steps: []*factoryStep{{err: errFlaky}}} // repeats forever
	s := newWithClock(Config{
		Factory: ff.make,
		Backoff: BackoffConfig{Base: 100 * time.Millisecond, Max: 2 * time.Second, Factor: 2.0},
	}, clk)

	for i := 1; i <= 6; i++ {
		got := awaitSleep(t, clk)
		want := s.backoff.delay(i, clk.randVal)
		if got != want {
			t.Errorf("backoff[attempt=%d] = %v, want %v", i, got, want)
		}
		clk.wake <- struct{}{} // release this sleep, advancing to the next attempt
	}

	_ = s.Close()
	mustWait(t, s, audiostream.ErrClosed)

	// The recorded durations are strictly non-decreasing and reach the cap.
	ds := clk.durations()
	for i := 1; i < len(ds) && i < 6; i++ {
		if ds[i] < ds[i-1] {
			t.Errorf("backoff went backwards: ds[%d]=%v < ds[%d]=%v", i, ds[i], i-1, ds[i-1])
		}
	}
}

// TestCloseDuringBackoff confirms a Close while a backoff sleep is in flight
// interrupts the sleep, ends with ErrClosed, and does not call the Factory again.
func TestCloseDuringBackoff(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	clk.auto = false // hold the sleep so we can Close mid-backoff

	src := newFakeSource(1)
	src.waitErr = errFlaky // one session, ends immediately -> backoff
	ff := &fakeFactory{steps: []*factoryStep{{src: src}}}
	s := newWithClock(Config{Factory: ff.make}, clk)

	awaitSleep(t, clk) // backoff has begun; the sleep is parked
	_ = s.Close()
	mustWait(t, s, audiostream.ErrClosed)

	if got := ff.callCount(); got != 1 {
		t.Errorf("Factory calls = %d, want 1 (no reconnect after Close during backoff)", got)
	}
	if s.State() != StateClosed {
		t.Errorf("State = %v, want Closed", s.State())
	}
}

// TestCloseDuringLiveSession confirms a Close while a source is live closes that
// source exactly once and ends with ErrClosed.
func TestCloseDuringLiveSession(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	src := newFakeSource(1)
	src.block = make(chan struct{})
	ff := &fakeFactory{steps: []*factoryStep{{src: src}}}
	s := newWithClock(Config{Factory: ff.make}, clk)

	awaitClose(t, src.entered, "session live")
	_ = s.Close()
	mustWait(t, s, audiostream.ErrClosed)

	if got := src.closeCount.Load(); got != 1 {
		t.Errorf("source Close count = %d, want 1", got)
	}
}

// TestWaitContextCancel confirms cancelling the context passed to Wait ends the
// supervisor and returns context.Canceled.
func TestWaitContextCancel(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	src := newFakeSource(1)
	src.block = make(chan struct{})
	ff := &fakeFactory{steps: []*factoryStep{{src: src}}}
	s := newWithClock(Config{Factory: ff.make}, clk)
	awaitClose(t, src.entered, "session live")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	if err := s.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Wait = %v, want context.Canceled", err)
	}
	if s.State() != StateClosed {
		t.Errorf("State = %v, want Closed", s.State())
	}
}

// TestPermanentErrorDefault confirms a non-retryable error under DefaultRetryable
// ends with StateFailed and no backoff.
func TestPermanentErrorDefault(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	// A redirect is terminal under DefaultRetryable.
	ff := &fakeFactory{steps: []*factoryStep{{err: &audiostream.RedirectError{Location: "rtsp://other/stream"}}}}
	s := newWithClock(Config{Factory: ff.make}, clk)

	mustWait(t, s, audiostream.ErrRedirect)
	if s.State() != StateFailed {
		t.Errorf("State = %v, want Failed", s.State())
	}
	if got := len(clk.durations()); got != 0 {
		t.Errorf("backoff sleeps = %d, want 0 (no retry on a terminal error)", got)
	}
	if got := ff.callCount(); got != 1 {
		t.Errorf("Factory calls = %d, want 1", got)
	}
}

// TestPermanentErrorCustomRetryable confirms a custom Retryable can classify an
// otherwise-retryable-looking error as terminal.
func TestPermanentErrorCustomRetryable(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	ff := &fakeFactory{steps: []*factoryStep{{err: errPermanent}}}
	retryable := func(err error) bool { return !errors.Is(err, errPermanent) }
	s := newWithClock(Config{Factory: ff.make, Retryable: retryable}, clk)

	mustWait(t, s, errPermanent)
	if s.State() != StateFailed {
		t.Errorf("State = %v, want Failed", s.State())
	}
	if got := len(clk.durations()); got != 0 {
		t.Errorf("backoff sleeps = %d, want 0", got)
	}
}

// TestFactoryRetryThenSuccess confirms a retryable Factory error backs off and
// the next call's success connects, resetting the attempt counter.
func TestFactoryRetryThenSuccess(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	src := newFakeSource(2)
	src.block = make(chan struct{})
	ff := &fakeFactory{steps: []*factoryStep{{err: errFlaky}, {src: src}}}
	s := newWithClock(Config{Factory: ff.make}, clk)

	awaitClose(t, src.entered, "success after one retry")
	if s.State() != StateConnected {
		t.Errorf("State = %v, want Connected", s.State())
	}
	if got := ff.callCount(); got != 2 {
		t.Errorf("Factory calls = %d, want 2", got)
	}
	if got := len(clk.durations()); got != 1 {
		t.Errorf("backoff sleeps = %d, want 1", got)
	}
	_ = s.Close()
	mustWait(t, s, audiostream.ErrClosed)
}

// TestNilSourceIsRetried confirms a contract-violating Factory that returns no
// source and no error degrades to a reconnect attempt (a clear cause, a
// backoff) rather than a nil-pointer crash.
func TestNilSourceIsRetried(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	src := newFakeSource(1)
	src.block = make(chan struct{})
	nilStep := &factoryStep{} // src nil, err nil: contract violation
	ff := &fakeFactory{steps: []*factoryStep{nilStep, {src: src}}}
	s := newWithClock(Config{Factory: ff.make}, clk)

	awaitClose(t, src.entered, "recovery after a nil source")
	if s.State() != StateConnected {
		t.Errorf("State = %v, want Connected", s.State())
	}
	if got := len(clk.durations()); got != 1 {
		t.Errorf("backoff sleeps = %d, want 1 (the nil source was retried once)", got)
	}
	_ = s.Close()
	mustWait(t, s, audiostream.ErrClosed)
}

// TestStatsInfoZeroBeforeConnect confirms Stats and Info return the zero value
// before the first source connects.
func TestStatsInfoZeroBeforeConnect(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	entered := make(chan struct{})
	ff := &fakeFactory{steps: []*factoryStep{{block: make(chan struct{}), entered: entered}}}
	s := newWithClock(Config{Factory: ff.make}, clk)
	awaitClose(t, entered, "factory entered (still connecting)")

	if st := s.Stats(); len(st.Tracks) != 0 || !st.CapturedAt.IsZero() {
		t.Errorf("Stats before connect = %+v, want zero", st)
	}
	if info := s.Info(); info != (audiostream.SourceInfo{}) {
		t.Errorf("Info before connect = %+v, want zero", info)
	}

	_ = s.Close()
	mustWait(t, s, audiostream.ErrClosed)
}

// TestStatsInfoLiveAndGap confirms Stats/Info delegate to the live source while
// connected and return the last session's snapshot during a reconnect gap.
func TestStatsInfoLiveAndGap(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()

	src1 := newFakeSource(1)
	src1.block = make(chan struct{})
	src1.waitErr = errFlaky
	src1.stats = audiostream.Stats{Tracks: map[int]audiostream.TrackStats{0: {Packets: 7}}}
	src1.info = audiostream.SourceInfo{URL: urlOne, Server: "CamOne"}

	gapEntered := make(chan struct{})
	// gapBlock is never closed: the second Factory call parks here until Close
	// cancels its ctx, holding the supervisor in the reconnect gap meanwhile.
	gapBlock := make(chan struct{})
	ff := &fakeFactory{steps: []*factoryStep{
		{src: src1},
		{block: gapBlock, entered: gapEntered}, // holds the loop in the gap
	}}
	s := newWithClock(Config{Factory: ff.make}, clk)

	// Live: Stats/Info delegate to src1.
	awaitClose(t, src1.entered, "src1 live")
	if got := s.Stats().Tracks[0].Packets; got != 7 {
		t.Errorf("live Stats packets = %d, want 7", got)
	}
	if got := s.Info().URL; got != urlOne {
		t.Errorf("live Info URL = %q, want %q", got, urlOne)
	}

	// End session 1 and let the loop stall on the second (blocking) Factory call.
	close(src1.block)
	awaitClose(t, gapEntered, "reconnect gap")

	if got := s.Stats().Tracks[0].Packets; got != 7 {
		t.Errorf("gap Stats packets = %d, want 7 (last snapshot)", got)
	}
	if got := s.Info().URL; got != urlOne {
		t.Errorf("gap Info URL = %q, want the last snapshot %q", got, urlOne)
	}

	// The gap snapshot must be an independent copy: mutating it cannot corrupt
	// the retained one.
	snap := s.Stats()
	delete(snap.Tracks, 0)
	if got := s.Stats().Tracks[0].Packets; got != 7 {
		t.Errorf("gap Stats after mutating a prior snapshot = %d, want 7", got)
	}

	_ = s.Close()
	mustWait(t, s, audiostream.ErrClosed)
}

// TestDoubleClose confirms Close is idempotent and Wait still reports ErrClosed.
func TestDoubleClose(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	src := newFakeSource(1)
	src.block = make(chan struct{})
	ff := &fakeFactory{steps: []*factoryStep{{src: src}}}
	s := newWithClock(Config{Factory: ff.make}, clk)
	awaitClose(t, src.entered, "session live")

	if err := s.Close(); err != nil {
		t.Errorf("first Close = %v, want nil", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
	mustWait(t, s, audiostream.ErrClosed)
	// A third Close after termination is still safe.
	if err := s.Close(); err != nil {
		t.Errorf("post-termination Close = %v, want nil", err)
	}
}

// TestFactoryPanicBecomesFailed confirms a panic in the Factory is recovered
// into a terminal StateFailed rather than crashing the process.
func TestFactoryPanicBecomesFailed(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	factory := func(context.Context) (audiostream.Source, error) {
		panic("factory boom")
	}
	s := newWithClock(Config{Factory: factory}, clk)

	done := make(chan error, 1)
	go func() { done <- s.Wait(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Wait after Factory panic = nil, want a non-nil failure")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after Factory panic")
	}
	if s.State() != StateFailed {
		t.Errorf("State = %v, want Failed", s.State())
	}
}

// TestNoGoroutineLeak confirms the supervising goroutine settles after Close and
// that every source the Factory produced was Closed exactly once.
func TestNoGoroutineLeak(t *testing.T) {
	// Not parallel: it reads runtime.NumGoroutine, which other parallel tests
	// would perturb.
	src1 := newFakeSource(1)
	src1.waitErr = errFlaky
	src2 := newFakeSource(2)
	src2.waitErr = errFlaky
	src3 := newFakeSource(3)
	src3.block = make(chan struct{})
	ff := &fakeFactory{steps: []*factoryStep{{src: src1}, {src: src2}, {src: src3}}}

	clk := newFakeClock()
	base := runtime.NumGoroutine()

	s := newWithClock(Config{Factory: ff.make}, clk)
	awaitClose(t, src3.entered, "third session live")
	_ = s.Close()
	mustWait(t, s, audiostream.ErrClosed)

	for _, src := range ff.sources() {
		if got := src.closeCount.Load(); got != 1 {
			t.Errorf("source %d Close count = %d, want 1", src.id, got)
		}
	}

	// Allow the supervising goroutine to unwind; poll rather than sleep a fixed
	// amount so a fast machine does not wait needlessly.
	settled := false
	for i := 0; i < 100; i++ {
		if runtime.NumGoroutine() <= base+1 {
			settled = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !settled {
		t.Errorf("goroutines did not settle: base=%d now=%d", base, runtime.NumGoroutine())
	}
}

// TestConcurrentAccessorsDuringReconnect hammers Stats, Info, and State from
// many goroutines while the supervisor flaps through reconnects, then Closes.
// Run under -race, it guards the mu discipline and the current/last handoff.
func TestConcurrentAccessorsDuringReconnect(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()

	// A factory that always returns a fresh source ending immediately, so the
	// supervisor reconnects continuously under the readers.
	var idCounter atomic.Int32
	factory := func(context.Context) (audiostream.Source, error) {
		src := newFakeSource(int(idCounter.Add(1)))
		src.waitErr = errFlaky
		src.stats = audiostream.Stats{Tracks: map[int]audiostream.TrackStats{0: {Packets: 1}}}
		src.info = audiostream.SourceInfo{URL: "rtsp://flap/stream"}
		return src, nil
	}
	s := newWithClock(Config{Factory: factory}, clk)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = s.Stats()
				_ = s.Info()
				_ = s.State()
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
	_ = s.Close()
	mustWait(t, s, audiostream.ErrClosed)
}
