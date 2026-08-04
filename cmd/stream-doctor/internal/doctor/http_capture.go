package doctor

import (
	"context"
	"errors"
	"net/url"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/httpsource"
	"github.com/tphakala/go-audio-stream/rtsp"
)

// httpProber is the production HTTPProber, wrapping *httpsource.Client. Open
// resolves the audio format and synthesizes a single L16 track so the shared
// track, decodable, stats, and listen paths consume an HTTP source exactly as
// they consume an RTSP one.
type httpProber struct {
	opts   Options
	client *httpsource.Client

	// sink accumulates the captured audio frames, shared with the RTSP adapter.
	// The HTTP source carries a single track (always ID 0), so no per-frame
	// track filter is needed and sink.onFrame is registered directly.
	sink frameSink

	// track is the synthesized single audio track, populated by Open.
	track rtsp.Track
}

// compile-time: httpProber implements HTTPProber.
var _ HTTPProber = (*httpProber)(nil)

// newHTTPProber builds a production HTTPProber for opts. The *httpsource.Client
// is created in Open, with the shared frame sink registered as its OnFrame.
//
//nolint:gocritic // Options is the documented constructor signature; hugeParam does not apply to a per-run entry point.
func newHTTPProber(opts Options) *httpProber {
	return &httpProber{
		opts: opts,
		sink: frameSink{maxFrames: maxCaptureFrames, maxBytes: maxCaptureBytes},
	}
}

// Open performs the HTTP handshake via httpsource.Open and, on success,
// synthesizes the single L16 track the source delivers. The synthesized track
// reuses rtsp.Track, a plain value struct with nothing session-bound, so the
// shared renderers and the L16 listen path need no HTTP-specific branch: its
// CodecL16 routes writeWAV to writeWAVPCM, which expects the little-endian
// s16le PCM httpsource delivers. PayloadType is -1 (there is no RTP payload
// type on the wire), matching the dash cell a formatless track renders.
func (p *httpProber) Open(ctx context.Context) error {
	cfg := httpsource.Config{
		URL:               p.opts.URL,
		Username:          p.opts.Username,
		Password:          p.opts.Password,
		Timeout:           p.opts.Timeout,
		ReadIdle:          p.opts.ReadIdle,
		InsecureTLS:       p.opts.InsecureTLS,
		AllowInsecureAuth: p.opts.InsecureAuth,
		UserAgent:         "stream-doctor/" + Version,
		OnFrame:           p.sink.onFrame,
	}
	client, err := httpsource.Open(ctx, cfg)
	if err != nil {
		return err
	}
	p.client = client

	codec := client.Codec()
	p.track = rtsp.Track{
		ID:          0,
		Media:       audiostream.MediaAudio,
		Codec:       codec,
		ClockRate:   codec.ClockRate,
		Channels:    codec.Channels,
		PayloadType: -1,
	}
	return nil
}

// Track returns the synthesized single audio track. It is the zero rtsp.Track
// until Open has succeeded.
func (p *httpProber) Track() rtsp.Track {
	return p.track
}

// Info returns the source identity snapshot, or the zero snapshot before Open
// has created the client.
func (p *httpProber) Info() audiostream.SourceInfo {
	if p.client == nil {
		return audiostream.SourceInfo{}
	}
	return p.client.Info()
}

// Collect mirrors rtspProber.Collect: it owns the capture timer so the internal
// deadline is never surfaced as a user error, snapshots the shared sink, reads
// the single-track stats, and classifies the end reason.
func (p *httpProber) Collect(ctx context.Context, track rtsp.Track, window time.Duration) (CaptureResult, error) {
	captureCtx, cancel := context.WithTimeout(ctx, window)
	defer cancel()

	start := time.Now()
	waitErr := p.client.Wait(captureCtx)
	elapsed := time.Since(start)

	frames, truncated := p.sink.snapshot()

	st := p.client.Stats()

	return CaptureResult{
		Track:      track,
		Frames:     frames,
		Stats:      st.Tracks[0],
		CapturedAt: st.CapturedAt,
		Window:     window,
		Elapsed:    elapsed,
		Reason:     classifyHTTPEndReason(ctx, waitErr, truncated, len(frames)),
	}, nil
}

// Close ends the stream; idempotent. Safe before Open has created the client,
// so a deferred Close after a failed Open does not panic.
func (p *httpProber) Close() error {
	if p.client == nil {
		return nil
	}
	return p.client.Close()
}

// classifyHTTPEndReason maps httpsource.Wait's terminal error (and the parent
// ctx and truncation flag) to an EndReason. ctx is the caller's original
// context, distinct from Collect's internal capture-window context, so a parent
// cancellation (EndCancelled) is distinguishable from the capture window simply
// elapsing (EndCompleted). httpsource.LastFrameAt tracks the last body read
// rather than an RTP media frame, but the end-reason taxonomy is shared with
// the RTSP path; only the connection-loss and orderly-end sentinels differ.
func classifyHTTPEndReason(ctx context.Context, err error, truncated bool, frameCount int) EndReason {
	switch {
	case ctx.Err() != nil:
		return EndCancelled
	case errors.Is(err, context.DeadlineExceeded) && !truncated:
		return EndCompleted
	case truncated:
		return EndTruncated
	case errors.Is(err, httpsource.ErrStreamEnded):
		return EndStreamEnded
	case errors.Is(err, audiostream.ErrReadTimeout):
		return EndWatchdog
	case errors.Is(err, httpsource.ErrConnectionClosed):
		// A mid-stream disconnect must not fall through to EndCompleted; an
		// HTTP source signals a lost body with ErrConnectionClosed.
		return EndDisconnect
	case err != nil && frameCount == 0:
		// An unrecognized terminal cause with nothing captured looks like a
		// lost connection, not a clean end.
		return EndDisconnect
	default:
		return EndCompleted
	}
}

// httpAuthScheme reports the auth scheme the report shows for an HTTP run:
// "basic" when credentials were configured (Config Username/Password or a
// non-empty URL userinfo), "none" otherwise. It mirrors httpsource.parseTarget:
// a wholly-empty userinfo is treated as absent.
func httpAuthScheme(opts Options) string {
	if credentialsConfigured(opts) {
		return "basic"
	}
	return "none"
}

// credentialsConfigured reports whether opts carries HTTP Basic credentials.
func credentialsConfigured(opts Options) bool {
	if opts.Username != "" || opts.Password != "" {
		return true
	}
	if u, err := url.Parse(opts.URL); err == nil && u.User != nil {
		if user := u.User.Username(); user != "" {
			return true
		}
		if pass, _ := u.User.Password(); pass != "" {
			return true
		}
	}
	return false
}
