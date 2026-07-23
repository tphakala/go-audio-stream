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
	// Window is the requested capture window.
	Window time.Duration
	// Elapsed is the wall-clock time actually spent capturing.
	Elapsed time.Duration
	// Reason records why capture stopped.
	Reason EndReason
}

// HandshakeStep is one timed handshake stage for the walkthrough and report.
type HandshakeStep struct {
	Name    string // "DIAL", "DESCRIBE", "SETUP", "PLAY", "CAPTURE"
	OK      bool
	Elapsed time.Duration
	Detail  string // one-line negotiated detail or failure reason
}

// Env is the injected machine context, so report and walkthrough golden tests
// are deterministic.
type Env struct {
	OS      string // runtime.GOOS in production
	Arch    string // runtime.GOARCH in production
	Version string // Version in production
}

// ListenResult describes a written WAV (Task 4). Zero value means no listen
// check ran.
type ListenResult struct {
	Written    bool
	SampleRate int
	Channels   int
	Frames     int // samples per channel
	Skipped    bool
	SkipReason string
}

// Report is the fully-populated result of a run, consumed by both renderers.
type Report struct {
	RedactedURL  string
	Result       string // human phrase: "capture OK", "no audio track", "connection failed", ...
	Steps        []HandshakeStep
	Session      rtsp.SessionInfo
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
