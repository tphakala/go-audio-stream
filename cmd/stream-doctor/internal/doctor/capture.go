package doctor

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/rtsp"
)

// Prober is the source-agnostic capture surface every source shares: it
// collects a window of audio and releases its resources. The per-protocol
// negotiation the runner drives before capture lives on the RTSPProber and
// HTTPProber extensions; the runner picks one by type assertion. The
// production adapters wrap *rtsp.Client and *httpsource.Client; tests supply a
// fake so the engine, stats, walkthrough, report, and exit-code paths run
// without a network or a camera.
type Prober interface {
	// Collect blocks for at most window (or until the stream ends or ctx
	// is cancelled), returning every captured audio frame for track, the
	// final Stats counters, and the end reason. It never returns the
	// internal capture timer's deadline as an error.
	Collect(ctx context.Context, track rtsp.Track, window time.Duration) (CaptureResult, error)
	// Close ends the session; idempotent.
	Close() error
}

// captureProgressReporter is the optional capability a Prober may implement to
// surface live capture counters while its Collect is blocked on the window. The
// production rtspProber and httpProber implement it; the runner type-asserts
// for it and only draws a live progress meter when the prober supports it and
// the meter's writer is an interactive terminal. Test fakes need not implement
// it, so the engine's golden output stays byte-stable. Progress must be safe to
// call concurrently with an in-flight Collect: both production implementations
// read only atomics and mutex-guarded state.
type captureProgressReporter interface {
	Progress() CaptureProgress
}

// RTSPProber is the RTSP negotiation surface: the timed DIAL, DESCRIBE, SETUP,
// PLAY handshake plus its session snapshot. rtspProber and the test fakeProber
// implement it.
type RTSPProber interface {
	Prober
	// Dial connects and runs OPTIONS. After it returns, SessionInfo
	// reports the negotiated KeepaliveMethod.
	Dial(ctx context.Context) error
	// Describe returns the SDP tracks.
	Describe(ctx context.Context) ([]rtsp.Track, error)
	// Setup sets up one track. opts.Discard is false for the target audio
	// track and true for the extra tracks set up under --full-stream. The
	// adapter records the track set up with Discard == false as the audio
	// target that its OnFrame sink filters on.
	Setup(ctx context.Context, track rtsp.Track, opts rtsp.SetupOptions) error
	// Play starts the stream.
	Play(ctx context.Context) error
	// SessionInfo is the negotiated session snapshot known so far.
	SessionInfo() rtsp.SessionInfo
}

// HTTPProber is the HTTP progressive-source negotiation surface: a single OPEN
// that resolves the audio format, exposes the synthesized single-track model,
// and reports the source identity. httpProber implements it.
type HTTPProber interface {
	Prober
	// Open performs the whole HTTP handshake and starts delivery. After it
	// returns nil, Track and Info are populated.
	Open(ctx context.Context) error
	// Track is the single synthesized L16 audio track (ID 0) the source
	// delivers, shaped so the shared track, decodable, stats, and listen paths
	// consume it unchanged.
	Track() rtsp.Track
	// Info is the source-neutral identity snapshot (URL and Server header).
	Info() audiostream.SourceInfo
}

// frameSink is the mutex-guarded, cap-bounded frame accumulator both adapters
// share. Its onFrame is the OnFrame callback each source's Config registers;
// the RTSP adapter wraps it behind an audio-track filter, while the HTTP
// adapter (single track, always ID 0) registers it directly.
type frameSink struct {
	// mu guards the frames accumulated by onFrame.
	mu        sync.Mutex
	frames    []CapturedFrame
	bytes     int
	truncated bool

	// maxFrames and maxBytes are the capture caps; the constructors set them to
	// the package defaults, and tests may override them directly.
	maxFrames int
	maxBytes  int
}

// onFrame appends an owned copy of f under the caps. It is non-blocking: a
// slice append under a short lock, no I/O.
//
//nolint:gocritic // f's signature is fixed by the OnFrame callback contract of both sources.
func (s *frameSink) onFrame(f audiostream.Frame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Once the caps are hit, truncated latches: a later smaller frame must not
	// slip in past the cap and leave a discontiguous gap in the capture.
	if !s.truncated && len(s.frames) < s.maxFrames && s.bytes+len(f.Data) <= s.maxBytes {
		s.frames = append(s.frames, CapturedFrame{
			Data:       append([]byte(nil), f.Data...),
			RTPTime:    f.RTPTime,
			PTS:        f.PTS,
			ReceivedAt: f.ReceivedAt,
			SeqGap:     f.SeqGap,
		})
		s.bytes += len(f.Data)
		return
	}
	s.truncated = true
}

// count returns the number of frames captured so far under the sink lock. It
// is the cheap, allocation-free read the live progress meter polls each second,
// distinct from snapshot which copies the whole slice for the final report.
func (s *frameSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.frames)
}

// snapshot returns an owned copy of the frames captured so far and the
// truncation flag.
func (s *frameSink) snapshot() (frames []CapturedFrame, truncated bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	frames = append([]CapturedFrame(nil), s.frames...)
	truncated = s.truncated
	return frames, truncated
}

// rtspProber is the production RTSPProber, wrapping *rtsp.Client.
type rtspProber struct {
	opts   Options
	client *rtsp.Client

	// audioTrackID is the ID of the track set up with Discard == false,
	// read on the reader goroutine by onFrame and written on the caller
	// goroutine by Setup; an atomic avoids locking mu on every frame.
	audioTrackID atomic.Int32

	// sink accumulates the captured audio frames, shared with the HTTP adapter.
	sink frameSink

	// codecMu guards learnedCodec.
	codecMu sync.Mutex
	// learnedCodec is the most recent codec the library resolved DURING
	// delivery for the audio target track and reported through
	// Config.OnCodecUpdate, or nil until one fires. Today only an in-band
	// MP4A-LATM track (cpresent=1) fires it, carrying the AudioSpecificConfig
	// learned from the first packet; an out-of-band track is seeded from
	// Describe and never fires it. onCodecUpdate writes it on the delivery
	// goroutine and Collect reads it on the caller goroutine, so it is
	// mutex-guarded.
	learnedCodec audiostream.Codec
}

// compile-time: rtspProber implements RTSPProber.
var _ RTSPProber = (*rtspProber)(nil)

// newRTSPProber builds a production RTSPProber for opts. The *rtsp.Client is
// created in Dial, with an OnFrame sink that copies frames for the audio
// track into a mutex-guarded slice, bounded by the capture caps.
//
//nolint:gocritic // Options is the documented constructor signature; hugeParam does not apply to a per-run entry point.
func newRTSPProber(opts Options) *rtspProber {
	p := &rtspProber{
		opts: opts,
		sink: frameSink{maxFrames: maxCaptureFrames, maxBytes: maxCaptureBytes},
	}
	// audioTrackID defaults to 0, which is a valid track ID, so store an
	// impossible ID until Setup records the real audio track. Otherwise a
	// camera whose video is track 0 would have its video frames captured as
	// audio in the window before the audio track is set up.
	p.audioTrackID.Store(-1)
	return p
}

// transportPreference maps the -transport flag value to the library's
// TransportPreference. ok is false for an unrecognized value, which parseArgs
// turns into a usage error; an empty string maps to the default PreferTCP so a
// zero Options is valid.
func transportPreference(s string) (pref rtsp.TransportPreference, ok bool) {
	switch s {
	case "", transportTCP:
		return rtsp.PreferTCP, true
	case transportUDP:
		return rtsp.PreferUDP, true
	case transportUDPThenTCP:
		return rtsp.PreferUDPThenTCP, true
	default:
		return rtsp.PreferTCP, false
	}
}

// g726PackingOverride maps the -g726-packing flag value to the library's
// G726PackingOverride. ok is false for an unrecognized value, which parseArgs
// turns into a usage error; an empty string maps to the default (defer to the
// SDP) so a zero Options is valid.
func g726PackingOverride(s string) (rtsp.G726PackingOverride, bool) {
	switch s {
	case "", g726PackingSDP:
		return rtsp.G726PackingFromSDP, true
	case g726PackingRFC3551:
		return rtsp.G726PackingForceRFC3551, true
	case g726PackingAAL2:
		return rtsp.G726PackingForceAAL2, true
	default:
		return rtsp.G726PackingFromSDP, false
	}
}

// effectiveG726Packing resolves a validated override against the packing Describe
// reported from the rtpmap, returning the packing the decoder will actually use.
// It hand-mirrors rtsp.(G726PackingOverride).resolve, which is unexported and so
// cannot be called across this module boundary (a forced value wins; the SDP
// default and any unrecognized value defer to the SDP), so the report cannot name
// a different packing from the one the decoder used. The two switches must be kept
// in step: a G726PackingOverride constant added to the rtsp package must be added
// here too. That is safe in practice because G.726 has exactly two codeword bit
// orders (RFC 3551 LSB-first and AAL2 MSB-first), so no third forced value can
// arise. ok is false for an unrecognized override, matching g726PackingOverride's
// contract.
func effectiveG726Packing(override rtsp.G726PackingOverride, fromSDP audiostream.G726Packing) (audiostream.G726Packing, bool) {
	switch override {
	case rtsp.G726PackingFromSDP:
		return fromSDP, true
	case rtsp.G726PackingForceRFC3551:
		return audiostream.G726PackingRFC3551, true
	case rtsp.G726PackingForceAAL2:
		return audiostream.G726PackingAAL2, true
	default:
		return fromSDP, false
	}
}

// dialConfig builds the rtsp.Config Dial hands to rtsp.Dial: a pure mapping
// from p.opts plus the prober's OnFrame sink, kept as its own method so the
// -transport binding is assertable without a live dial.
func (p *rtspProber) dialConfig() rtsp.Config {
	// opts.Transport was validated by parseArgs, so ok is always true here; the
	// zero-value PreferTCP is a safe fallback for a caller that bypassed it.
	pref, _ := transportPreference(p.opts.Transport)
	return rtsp.Config{
		URL:           p.opts.URL,
		Username:      p.opts.Username,
		Password:      p.opts.Password,
		Timeout:       p.opts.Timeout,
		ReadIdle:      p.opts.ReadIdle,
		InsecureTLS:   p.opts.InsecureTLS,
		UserAgent:     "stream-doctor/" + Version,
		OnFrame:       p.onFrame,
		OnCodecUpdate: p.onCodecUpdate,
		Transport:     pref,
	}
}

// Dial connects to opts.URL and runs OPTIONS via rtsp.Dial.
func (p *rtspProber) Dial(ctx context.Context) error {
	client, err := rtsp.Dial(ctx, p.dialConfig())
	if err != nil {
		return err
	}
	p.client = client
	return nil
}

// Describe returns the SDP tracks.
func (p *rtspProber) Describe(ctx context.Context) ([]rtsp.Track, error) {
	return p.client.Describe(ctx)
}

// Setup sets up one track, recording it as the audio target when
// opts.Discard is false.
func (p *rtspProber) Setup(ctx context.Context, track rtsp.Track, opts rtsp.SetupOptions) error {
	if !opts.Discard {
		//nolint:gosec // track.ID is a small SDP media index, never large enough to overflow int32.
		p.audioTrackID.Store(int32(track.ID))
	}
	return p.client.Setup(ctx, track, opts)
}

// Play starts the stream.
func (p *rtspProber) Play(ctx context.Context) error {
	return p.client.Play(ctx)
}

// Progress reports the live capture counters for the audio target track,
// polled by the runner's progress meter each second during Collect. It reads
// the shared sink's frame count and the client's atomic Stats, both safe to
// touch while Wait is blocked. Before Dial has created the client, or before
// Setup has recorded the audio track, only the (zero) frame count is known.
func (p *rtspProber) Progress() CaptureProgress {
	prog := CaptureProgress{Frames: p.sink.count()}
	if p.client == nil {
		return prog
	}
	//nolint:gosec // audioTrackID was stored from an int track ID; the round trip through int32 is exact.
	ts := p.client.Stats().Tracks[int(p.audioTrackID.Load())]
	prog.Packets = ts.Packets
	prog.Lost = ts.SeqGaps
	prog.Malformed = ts.Malformed
	return prog
}

// SessionInfo returns the negotiated session snapshot known so far, or the zero
// snapshot before Dial has created the client.
func (p *rtspProber) SessionInfo() rtsp.SessionInfo {
	if p.client == nil {
		return rtsp.SessionInfo{}
	}
	return p.client.SessionInfo()
}

// Close ends the session; idempotent. Safe before Dial has created the
// client, so a deferred Close after a failed Dial does not panic.
func (p *rtspProber) Close() error {
	if p.client == nil {
		return nil
	}
	return p.client.Close()
}

// onFrame is the OnFrame sink registered on the *rtsp.Client. It filters on
// the audio target track and forwards to the shared frame sink. Frames on any
// track other than the audio target are dropped immediately; under
// --full-stream the discarded tracks never reach here (the library drops them
// allocation-free), so this check is a cheap backstop.
//
//nolint:gocritic // f's signature is fixed by rtsp.Config.OnFrame's func(audiostream.Frame) callback contract.
func (p *rtspProber) onFrame(f audiostream.Frame) {
	//nolint:gosec // audioTrackID was stored from an int track ID; the round trip through int32 is exact.
	if f.TrackID != int(p.audioTrackID.Load()) {
		return
	}
	p.sink.onFrame(f)
}

// onCodecUpdate is the OnCodecUpdate sink registered on the *rtsp.Client. It
// records the codec the library resolved during delivery for the audio target
// track, so Collect can surface an in-band MP4A-LATM AudioSpecificConfig that
// Describe could not (the config is not in the SDP; the depacketizer learns it
// from the first packet). Updates on any other track are dropped: only the
// audio target is rendered and decoded. It runs on the delivery goroutine, so
// it stores under codecMu; latest wins, so an SSRC reset that re-announces a
// new config replaces the previous ASC rather than leaving a stale one.
func (p *rtspProber) onCodecUpdate(u audiostream.CodecUpdate) {
	//nolint:gosec // audioTrackID was stored from an int track ID; the round trip through int32 is exact.
	if u.TrackID != int(p.audioTrackID.Load()) {
		return
	}
	codec := u.Codec
	// Copy the codec's slices before retaining it. The OnCodecUpdate contract
	// (rtsp.Config: "the CodecUpdate's Codec and any slices it carries are owned
	// by the callee only for the duration of the call; copy AudioSpecificConfig
	// to retain it") disclaims ownership after this returns, and the library's
	// in-band ASC path is double-buffered scratch it may reuse. Collect reads
	// the retained value long after the call, and applyLearnedCodec then aliases
	// it into the rendered track, so an uncopied slice could be mutated under a
	// later reader. append([]byte(nil), nil...) stays nil, so this preserves an
	// absent StreamMuxConfig.
	if lat, ok := codec.(audiostream.CodecMP4ALATM); ok {
		lat.AudioSpecificConfig = append([]byte(nil), lat.AudioSpecificConfig...)
		lat.StreamMuxConfig = append([]byte(nil), lat.StreamMuxConfig...)
		codec = lat
	}
	p.codecMu.Lock()
	p.learnedCodec = codec
	p.codecMu.Unlock()
}

// learnedCodecSnapshot returns the codec most recently learned for the audio
// target track through OnCodecUpdate, or nil if none. Collect calls it after
// snapshotting frames: because OnCodecUpdate and OnFrame run in order on the
// single delivery goroutine, every captured frame's config update has already
// completed, so a captured in-band track never surfaces without its ASC.
func (p *rtspProber) learnedCodecSnapshot() audiostream.Codec {
	p.codecMu.Lock()
	defer p.codecMu.Unlock()
	return p.learnedCodec
}

// Collect owns the capture timer so the internal deadline is never
// surfaced as a user error: it always returns a nil error, letting the
// captured frame count and Reason drive the exit code.
func (p *rtspProber) Collect(ctx context.Context, track rtsp.Track, window time.Duration) (CaptureResult, error) {
	captureCtx, cancel := context.WithTimeout(ctx, window)
	defer cancel()

	start := time.Now()
	waitErr := p.client.Wait(captureCtx)
	elapsed := time.Since(start)

	frames, truncated := p.sink.snapshot()

	// Read the learned codec AFTER snapshotting frames: the two callbacks run in
	// order on the delivery goroutine, so any captured frame's OnCodecUpdate has
	// already stored its config, and an in-band track never surfaces without its
	// ASC.
	learned := p.learnedCodecSnapshot()

	st := p.client.Stats()

	return CaptureResult{
		Session:      p.client.SessionInfo(),
		Track:        track,
		Frames:       frames,
		Stats:        st.Tracks[track.ID],
		CapturedAt:   st.CapturedAt,
		Window:       window,
		Elapsed:      elapsed,
		Reason:       classifyEndReason(ctx, waitErr, truncated, len(frames)),
		LearnedCodec: learned,
	}, nil
}

// classifyEndReason maps Wait's terminal error (and the parent ctx and
// truncation flag) to an EndReason. ctx is the caller's original context,
// distinct from Collect's internal capture-window context, so a parent
// cancellation (EndCancelled) is distinguishable from the capture window
// simply elapsing (EndCompleted).
func classifyEndReason(ctx context.Context, err error, truncated bool, frameCount int) EndReason {
	switch {
	// Only a genuine cancellation (Ctrl-C) is EndCancelled. A parent-context
	// deadline is the caller's own time budget elapsing, not a cancel, so it
	// falls through to the completed/truncated handling below.
	case errors.Is(ctx.Err(), context.Canceled):
		return EndCancelled
	case errors.Is(err, context.DeadlineExceeded) && !truncated:
		return EndCompleted
	case truncated:
		return EndTruncated
	case errors.Is(err, audiostream.ErrReadTimeout):
		return EndWatchdog
	case errors.Is(err, rtsp.ErrServerTeardown):
		return EndTeardown
	case errors.Is(err, rtsp.ErrConnectionClosed):
		return EndDisconnect
	case err != nil && frameCount == 0:
		// An unrecognized terminal cause with nothing captured looks like
		// a lost connection, not a clean end.
		return EndDisconnect
	default:
		return EndCompleted
	}
}
