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

// Prober is the RTSP session surface the doctor drives. The production
// adapter wraps *rtsp.Client; tests supply a fake so the engine, stats,
// walkthrough, report, and exit-code paths run without a network or a
// camera.
type Prober interface {
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
	// Collect blocks for at most window (or until the stream ends or ctx
	// is cancelled), returning every captured audio frame for track, the
	// final Stats counters, the negotiated SessionInfo, and the end
	// reason. It never returns the internal capture timer's deadline as
	// an error.
	Collect(ctx context.Context, track rtsp.Track, window time.Duration) (CaptureResult, error)
	// SessionInfo is the negotiated session snapshot known so far.
	SessionInfo() rtsp.SessionInfo
	// Close ends the session; idempotent.
	Close() error
}

// rtspProber is the production Prober, wrapping *rtsp.Client.
type rtspProber struct {
	opts   Options
	client *rtsp.Client

	// audioTrackID is the ID of the track set up with Discard == false,
	// read on the reader goroutine by onFrame and written on the caller
	// goroutine by Setup; an atomic avoids locking mu on every frame.
	audioTrackID atomic.Int32

	// mu guards the frames accumulated by onFrame.
	mu        sync.Mutex
	frames    []CapturedFrame
	bytes     int
	truncated bool

	// maxFrames and maxBytes are the capture caps; newRTSPProber sets them
	// to the package defaults, and tests may override them directly.
	maxFrames int
	maxBytes  int
}

// newRTSPProber builds a production Prober for opts. The *rtsp.Client is
// created in Dial, with an OnFrame sink that copies frames for the audio
// track into a mutex-guarded slice, bounded by the capture caps.
//
//nolint:gocritic // Options is the documented constructor signature; hugeParam does not apply to a per-run entry point.
func newRTSPProber(opts Options) *rtspProber {
	p := &rtspProber{
		opts:      opts,
		maxFrames: maxCaptureFrames,
		maxBytes:  maxCaptureBytes,
	}
	// audioTrackID defaults to 0, which is a valid track ID, so store an
	// impossible ID until Setup records the real audio track. Otherwise a
	// camera whose video is track 0 would have its video frames captured as
	// audio in the window before the audio track is set up.
	p.audioTrackID.Store(-1)
	return p
}

// Dial connects to opts.URL and runs OPTIONS via rtsp.Dial.
func (p *rtspProber) Dial(ctx context.Context) error {
	cfg := rtsp.Config{
		URL:         p.opts.URL,
		Username:    p.opts.Username,
		Password:    p.opts.Password,
		Timeout:     p.opts.Timeout,
		ReadIdle:    p.opts.ReadIdle,
		InsecureTLS: p.opts.InsecureTLS,
		UserAgent:   "stream-doctor/" + Version,
		OnFrame:     p.onFrame,
	}
	client, err := rtsp.Dial(ctx, cfg)
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

// SessionInfo returns the negotiated session snapshot known so far, or the zero
// snapshot before Dial has created the client.
func (p *rtspProber) SessionInfo() rtsp.SessionInfo {
	if p.client == nil {
		return rtsp.SessionInfo{}
	}
	return p.client.SessionInfo()
}

// Close ends the session; idempotent, and safe before Dial has created the
// client (so a deferred Close after construction does not panic).
func (p *rtspProber) Close() error {
	if p.client == nil {
		return nil
	}
	return p.client.Close()
}

// onFrame is the OnFrame sink registered on the *rtsp.Client. It is
// non-blocking: a slice append under a short lock, no I/O. Frames on any
// track other than the audio target are dropped immediately; under
// --full-stream the discarded tracks never reach here (the library drops
// them allocation-free), so this check is a cheap backstop.
//
//nolint:gocritic // f's signature is fixed by rtsp.Config.OnFrame's func(audiostream.Frame) callback contract.
func (p *rtspProber) onFrame(f audiostream.Frame) {
	//nolint:gosec // audioTrackID was stored from an int track ID; the round trip through int32 is exact.
	if f.TrackID != int(p.audioTrackID.Load()) {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	// Once the caps are hit, truncated latches: a later smaller frame must not
	// slip in past the cap and leave a discontiguous gap in the capture.
	if !p.truncated && len(p.frames) < p.maxFrames && p.bytes+len(f.Data) <= p.maxBytes {
		p.frames = append(p.frames, CapturedFrame{
			Data:       append([]byte(nil), f.Data...),
			RTPTime:    f.RTPTime,
			PTS:        f.PTS,
			ReceivedAt: f.ReceivedAt,
			SeqGap:     f.SeqGap,
		})
		p.bytes += len(f.Data)
		return
	}
	p.truncated = true
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

	p.mu.Lock()
	frames := append([]CapturedFrame(nil), p.frames...)
	truncated := p.truncated
	p.mu.Unlock()

	stats := p.client.Stats().Tracks[track.ID]

	return CaptureResult{
		Session: p.client.SessionInfo(),
		Track:   track,
		Frames:  frames,
		Stats:   stats,
		Window:  window,
		Elapsed: elapsed,
		Reason:  classifyEndReason(ctx, waitErr, truncated, len(frames)),
	}, nil
}

// classifyEndReason maps Wait's terminal error (and the parent ctx and
// truncation flag) to an EndReason. ctx is the caller's original context,
// distinct from Collect's internal capture-window context, so a parent
// cancellation (EndCancelled) is distinguishable from the capture window
// simply elapsing (EndCompleted).
func classifyEndReason(ctx context.Context, err error, truncated bool, frameCount int) EndReason {
	switch {
	case ctx.Err() != nil:
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
