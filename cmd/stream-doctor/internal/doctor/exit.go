package doctor

import (
	"errors"
	"fmt"
	"io"

	"github.com/tphakala/go-audio-stream/rtsp"
)

// Process exit codes.
const (
	ExitOK           = 0
	ExitUsage        = 1
	ExitConnection   = 2
	ExitAuth         = 3
	ExitNoAudioTrack = 4
	ExitUnsupported  = 5
	ExitCapture      = 6
)

// Phase records how far a run progressed, so mapExit can classify an error.
type Phase int

const (
	// PhaseStart means nothing was attempted.
	PhaseStart Phase = iota
	// PhaseDial means Dial was attempted.
	PhaseDial
	// PhaseDescribe means Describe was attempted.
	PhaseDescribe
	// PhaseSetup means Setup was attempted.
	PhaseSetup
	// PhasePlay means Play was attempted.
	PhasePlay
	// PhaseCapture means capture was attempted.
	PhaseCapture
)

// Result summarizes a run for exit-code mapping and report rendering.
type Result struct {
	Phase           Phase
	AudioTrackFound bool
	CodecSupported  bool // the audio track's codec can be turned into a WAV
	FramesCaptured  int
}

// Run executes one full stream-doctor diagnostic pass for opts and returns
// the run Result and terminal error for mapExit. The body is a stub for now:
// a later change replaces it with the real dial/describe/setup/play/capture
// walkthrough.
//
//nolint:gocritic // Options is the documented public Run signature; hugeParam does not apply to a per-run entry point.
func Run(opts Options) (Result, error) {
	return Result{}, nil
}

// Execute is the process entry point: it parses args, prints the version or
// a usage error to stdout/stderr and returns the matching exit code, or runs
// the diagnostic engine and returns mapExit's result. main calls Execute and
// never changes as the engine grows in later tasks.
func Execute(args []string, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args)
	switch {
	case errors.Is(err, errVersionRequested):
		_, _ = fmt.Fprintln(stdout, "stream-doctor", Version)
		return ExitOK
	case errors.Is(err, ErrUsage):
		_, _ = fmt.Fprintln(stderr, err)
		_, _ = fmt.Fprint(stderr, usageText)
		return ExitUsage
	}

	res, runErr := Run(opts)
	return mapExit(runErr, res)
}

// mapExit returns the process exit code for a completed run. err is the
// terminal error from Run (nil on a clean capture); res is the run outcome.
// The mapping follows the documented precedence: usage and auth failures
// take priority; any other error during the capture phase is a
// capture-quality failure (ExitCapture), while an error before capture is a
// connectivity failure (ExitConnection); a nil error is classified by how
// far the run actually got.
func mapExit(err error, res Result) int {
	if err != nil {
		return mapExitErr(err, res)
	}
	return mapExitClean(res)
}

// mapExitErr classifies a non-nil terminal error.
func mapExitErr(err error, res Result) int {
	if errors.Is(err, ErrUsage) {
		return ExitUsage
	}
	if errors.Is(err, rtsp.ErrAuthFailed) {
		return ExitAuth
	}
	var unauthorized *rtsp.UnauthorizedError
	if errors.As(err, &unauthorized) {
		return ExitAuth
	}
	if res.Phase == PhaseCapture {
		return ExitCapture
	}
	return ExitConnection
}

// mapExitClean classifies a clean (nil-error) run by how far it got.
func mapExitClean(res Result) int {
	switch {
	case !res.AudioTrackFound:
		return ExitNoAudioTrack
	case !res.CodecSupported:
		return ExitUnsupported
	case res.FramesCaptured == 0:
		return ExitCapture
	default:
		return ExitOK
	}
}
