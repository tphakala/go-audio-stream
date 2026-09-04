package doctor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/httpsource"
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

// Execute is the process entry point: it parses args, prints the version or
// a usage error to stdout/stderr and returns the matching exit code, or runs
// the diagnostic engine and returns mapExit's result. It runs with a
// background context; main uses ExecuteContext to wire SIGINT.
func Execute(args []string, stdout, stderr io.Writer) int {
	return ExecuteContext(context.Background(), args, stdout, stderr)
}

// ExecuteContext is Execute with a caller-supplied context so main can cancel
// a run on SIGINT. It parses args, prints the version or a usage error, or
// drives Run against a live RTSP prober and returns mapExit's code.
func ExecuteContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
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

	env := Env{OS: runtime.GOOS, Arch: runtime.GOARCH, Version: Version}
	prober, perr := proberFor(opts)
	if perr != nil {
		_, _ = fmt.Fprintln(stderr, perr)
		_, _ = fmt.Fprint(stderr, usageText)
		return ExitUsage
	}
	defer func() { _ = prober.Close() }()

	res, runErr := Run(ctx, opts, prober, stdout, stderr, env, time.Now)
	return mapExit(runErr, res)
}

// proberFor selects the prober for opts.URL by scheme: rtsp/rtsps drive the
// RTSP handshake, http/https drive an HTTP progressive source. Any other scheme
// (or an unparseable URL) is a usage error. The error names only the scheme,
// never the raw URL, which can carry credentials in its userinfo.
func proberFor(opts Options) (Prober, error) {
	u, err := url.Parse(opts.URL)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid URL", ErrUsage)
	}
	switch strings.ToLower(u.Scheme) {
	case "rtsp", "rtsps":
		return newRTSPProber(opts), nil
	case "http", "https":
		return newHTTPProber(opts), nil
	default:
		return nil, fmt.Errorf("%w: unsupported scheme %q", ErrUsage, u.Scheme)
	}
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

// mapExitErr classifies a non-nil terminal error. Usage failures (including an
// HTTP source refusing plaintext credentials) and auth failures take priority;
// an unsupported HTTP media format is ExitUnsupported; any other error during
// the capture phase is a capture-quality failure and before capture a
// connectivity failure.
func mapExitErr(err error, res Result) int {
	switch {
	case errors.Is(err, ErrUsage):
		return ExitUsage
	case errors.Is(err, httpsource.ErrInsecureAuth):
		return ExitUsage
	case isAuthErr(err):
		return ExitAuth
	case isUnsupportedErr(err):
		return ExitUnsupported
	case res.Phase == PhaseCapture:
		return ExitCapture
	default:
		return ExitConnection
	}
}

// isAuthErr reports whether err is an authentication failure: the RTSP client's
// give-up sentinel, an RTSP 401 challenge that could not be answered, or an
// HTTP 401 status. Shared by mapExitErr and failStep so the exit code and the
// report's result phrase classify the same errors as auth failures. An HTTP 403
// is deliberately not auth: it is an authorization refusal, not a credential
// challenge, and stays a connection failure.
func isAuthErr(err error) bool {
	if errors.Is(err, rtsp.ErrAuthFailed) {
		return true
	}
	if _, ok := errors.AsType[*rtsp.UnauthorizedError](err); ok {
		return true
	}
	var status *httpsource.StatusError
	return errors.As(err, &status) && status.Code == http.StatusUnauthorized
}

// unsupportedFormatPhrase is the OPEN result phrase for an HTTP source that
// rejects the media format (see isUnsupportedErr).
const unsupportedFormatPhrase = "unsupported format"

// isUnsupportedErr reports whether err is an HTTP source rejecting a media
// format it will not decode: an unsupported Content-Type or WAV sample format,
// an unresolvable raw shape, or a malformed WAV. These map to ExitUnsupported
// rather than the default ExitConnection.
func isUnsupportedErr(err error) bool {
	return errors.Is(err, httpsource.ErrUnsupportedFormat) ||
		errors.Is(err, httpsource.ErrFormatUnknown) ||
		errors.Is(err, httpsource.ErrMalformedWAV)
}

// openPhrase is the report's result phrase for an HTTP OPEN failure, keyed by
// error class. A 401 is handled by failStep's isAuthErr override ("authentication
// failed"), so it is not listed here.
func openPhrase(err error) string {
	switch {
	case errors.Is(err, httpsource.ErrInsecureAuth):
		return "credentials refused over plaintext http (use -insecure-auth)"
	case isUnsupportedErr(err):
		return unsupportedFormatPhrase
	case errors.Is(err, audiostream.ErrRedirect):
		return "redirected (not followed)"
	default:
		return "connection failed"
	}
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
