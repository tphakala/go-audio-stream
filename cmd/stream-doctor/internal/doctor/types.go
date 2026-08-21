package doctor

import (
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/rtsp"
)

// CapturedFrame is an owned copy of one delivered audio frame. Data is
// copied out of the library's reusable buffer during OnFrame, so it is
// safe to retain.
type CapturedFrame struct {
	// Data is the frame payload, an owned copy.
	Data []byte
	// RTPTime is the raw 32-bit RTP timestamp of the packet that completed
	// this frame.
	RTPTime uint32
	// PTS is the presentation time relative to the first captured frame.
	PTS time.Duration
	// ReceivedAt is the local receive time of the completing packet.
	ReceivedAt time.Time
	// SeqGap is the number of RTP packets lost immediately before this
	// frame.
	SeqGap int
}

// CaptureProgress is a lightweight snapshot of the counters accumulated so far
// during a capture window, rendered by the live progress meter each second so
// the wait no longer looks like a hang. It mirrors the subset of the final
// capture block that changes visibly second to second. A prober exposes it
// through the optional captureProgressReporter interface.
type CaptureProgress struct {
	// Frames is the number of audio frames delivered so far (from the shared
	// capture sink). It is meaningful for every source, including HTTP
	// progressive sources that do not meter RTP packets.
	Frames int
	// Packets is the number of RTP packets accepted so far (from the library
	// Stats); zero for a source that does not meter packets.
	Packets uint64
	// Lost is the number of packets lost per sequence tracking so far (from
	// the library Stats.SeqGaps).
	Lost uint64
	// Malformed is the number of packets discarded without delivery so far
	// (from the library Stats.Malformed).
	Malformed uint64
}

// EndReason records why capture stopped.
type EndReason int

const (
	// EndCompleted means the capture window elapsed normally.
	EndCompleted EndReason = iota
	// EndWatchdog means ErrReadTimeout ended the capture: the stream went
	// silent.
	EndWatchdog
	// EndTeardown means the server sent a TEARDOWN.
	EndTeardown
	// EndDisconnect means the control connection was lost unexpectedly.
	EndDisconnect
	// EndCancelled means the parent context was cancelled (Ctrl-C).
	EndCancelled
	// EndTruncated means the capture cap was hit before the window
	// elapsed.
	EndTruncated
	// EndStreamEnded means the source reached an orderly end of stream: an
	// HTTP body EOF, or a WAV data chunk whose declared bytes were fully
	// consumed. It is distinct from EndCompleted, which is the capture window
	// elapsing while the stream was still live.
	EndStreamEnded
)

// SourceKind identifies the transport a run probed, so the renderers can pick
// an RTSP or HTTP session block. The zero value is SourceRTSP, so a Report the
// RTSP path builds needs no explicit Kind and every existing RTSP test is
// unaffected.
type SourceKind int

const (
	// SourceRTSP is an RTSP/RTSPS session driven through the DIAL, DESCRIBE,
	// SETUP, PLAY walkthrough.
	SourceRTSP SourceKind = iota
	// SourceHTTP is an HTTP(S) progressive source opened in one OPEN step.
	SourceHTTP
)

// CaptureResult is everything one capture produced.
type CaptureResult struct {
	// Session is the negotiated session snapshot at the end of capture.
	Session rtsp.SessionInfo
	// Track is the audio track that was set up.
	Track rtsp.Track
	// Frames are the audio frames captured, in arrival order, bounded by
	// the capture caps.
	Frames []CapturedFrame
	// Stats are the library's final receive counters for the audio track.
	Stats audiostream.TrackStats
	// CapturedAt is when the library's Stats snapshot read completed. It
	// carries a monotonic reading and is the reference the last-frame age is
	// measured against.
	CapturedAt time.Time
	// Window is the requested capture window.
	Window time.Duration
	// Elapsed is the wall-clock time actually spent capturing.
	Elapsed time.Duration
	// Reason records why capture stopped.
	Reason EndReason
	// LearnedCodec is a codec configuration the source resolved DURING capture
	// rather than at Describe, or nil when nothing new was learned. Today this
	// is the in-band MP4A-LATM AudioSpecificConfig (cpresent=1), which the
	// depacketizer learns from the first RTP packet and reports through
	// rtsp.Config.OnCodecUpdate after Describe has already returned an
	// ASC-less track. The runner overlays it onto the audio track so the
	// tracks block and the listen check see the resolved config. It stays nil
	// for out-of-band LATM (whose ASC is already in the Describe track) and for
	// every other source.
	LearnedCodec audiostream.Codec
}

// HandshakeStep is one timed handshake stage for the walkthrough and report.
type HandshakeStep struct {
	Name    string // "DIAL", "DESCRIBE", "SETUP", "PLAY", "CAPTURE"
	OK      bool
	Elapsed time.Duration
	Detail  string // one-line negotiated detail or failure reason
	// Hint is an optional actionable suggestion shown under a failed step's
	// detail (for example "try -transport tcp"), "" when there is nothing to
	// suggest. It is classifier-authored, host-free text, so it carries no PII.
	Hint string
}

// Env is the injected machine context, so report and walkthrough golden tests
// are deterministic.
type Env struct {
	OS      string // runtime.GOOS in production
	Arch    string // runtime.GOARCH in production
	Version string // Version in production
}

// ListenResult describes a written WAV. Zero value means no listen check ran.
type ListenResult struct {
	Written    bool
	SampleRate int
	Channels   int
	Frames     int // samples per channel
	Skipped    bool
	SkipReason string
	// SenderStart is the sender's wall-clock time of the first captured
	// frame, extrapolated from the RTCP sender clock; the zero Time when no
	// sender clock was available. It anchors the written WAV to absolute
	// time.
	SenderStart time.Time
}

// Report is the fully-populated result of a run, consumed by both renderers.
type Report struct {
	// Kind is the probed transport; SourceRTSP is the zero value, so the RTSP
	// path leaves it and reportSession renders the RTSP session block.
	Kind        SourceKind
	RedactedURL string
	Result      string // human phrase: "capture OK", "no audio track", "connection failed", ...
	Steps       []HandshakeStep
	Session     rtsp.SessionInfo
	// Source is the source-neutral identity snapshot for an HTTP run (URL and
	// Server), scrubbed at the orchestration boundary. It is the zero value for
	// an RTSP run, whose identity lives on Session.
	Source audiostream.SourceInfo
	// SourceAuth is the HTTP auth label the report's session block shows
	// ("basic" or "none"), "" for an RTSP run, which shows Session.AuthScheme.
	SourceAuth   string
	Tracks       []rtsp.Track
	AudioTrack   rtsp.Track
	HaveAudio    bool
	Capture      CaptureStats
	CaptureShown bool
	Window       time.Duration
	Reason       EndReason
	Listen       ListenResult
}

// Capture memory caps, independent of the duration flag.
const (
	// maxCaptureFrames bounds the number of frames retained; a flood stops
	// here (hours of audio at typical packet rates).
	maxCaptureFrames = 500_000
	// maxCaptureBytes bounds the total payload bytes retained.
	maxCaptureBytes = 256 << 20
)
