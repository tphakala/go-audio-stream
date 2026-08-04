package doctor

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/httpsource"
	"github.com/tphakala/go-audio-stream/rtsp"
)

func TestMapExit(t *testing.T) {
	t.Parallel()
	dialErr := errors.New("dial: connection refused")

	cases := []struct {
		name string
		err  error
		res  Result
		want int
	}{
		{"usage error", ErrUsage, Result{}, ExitUsage},
		{"dial error", dialErr, Result{Phase: PhaseDial}, ExitConnection},
		{"auth failed", rtsp.ErrAuthFailed, Result{Phase: PhaseDescribe}, ExitAuth},
		{"unauthorized", &rtsp.UnauthorizedError{}, Result{Phase: PhaseDescribe}, ExitAuth},
		{"response error 500", &rtsp.ResponseError{Code: 500}, Result{Phase: PhaseDescribe}, ExitConnection},
		{"no audio track", nil, Result{Phase: PhaseDescribe, AudioTrackFound: false}, ExitNoAudioTrack},
		{
			"unsupported codec", nil,
			Result{Phase: PhaseCapture, AudioTrackFound: true, CodecSupported: false, FramesCaptured: 500},
			ExitUnsupported,
		},
		{
			"zero frames", nil,
			Result{Phase: PhaseCapture, AudioTrackFound: true, CodecSupported: true, FramesCaptured: 0},
			ExitCapture,
		},
		{
			"clean capture", nil,
			Result{Phase: PhaseCapture, AudioTrackFound: true, CodecSupported: true, FramesCaptured: 500},
			ExitOK,
		},
		{
			"read timeout during capture", audiostream.ErrReadTimeout,
			Result{Phase: PhaseCapture, AudioTrackFound: true, CodecSupported: true, FramesCaptured: 0},
			ExitCapture,
		},
		{"connection closed", rtsp.ErrConnectionClosed, Result{Phase: PhaseDial}, ExitConnection},
		// HTTP source error classification (all pre-capture, PhaseDial).
		{"http insecure auth", httpsource.ErrInsecureAuth, Result{Phase: PhaseDial}, ExitUsage},
		{"http 401", &httpsource.StatusError{Code: http.StatusUnauthorized}, Result{Phase: PhaseDial}, ExitAuth},
		{"http 403 stays connection", &httpsource.StatusError{Code: http.StatusForbidden}, Result{Phase: PhaseDial}, ExitConnection},
		{"http unsupported format", httpsource.ErrUnsupportedFormat, Result{Phase: PhaseDial}, ExitUnsupported},
		{"http format unknown", httpsource.ErrFormatUnknown, Result{Phase: PhaseDial}, ExitUnsupported},
		{"http malformed wav", httpsource.ErrMalformedWAV, Result{Phase: PhaseDial}, ExitUnsupported},
		{"http redirect stays connection", &audiostream.RedirectError{Location: "http://elsewhere/x"}, Result{Phase: PhaseDial}, ExitConnection},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := mapExit(tc.err, tc.res); got != tc.want {
				t.Errorf("mapExit(%v, %+v) = %d, want %d", tc.err, tc.res, got, tc.want)
			}
		})
	}
}

func TestProberForDispatch(t *testing.T) {
	t.Parallel()
	for _, u := range []string{"rtsp://cam/stream", "rtsps://cam/stream", "RTSP://cam/stream"} {
		p, err := proberFor(Options{URL: u})
		if err != nil {
			t.Fatalf("proberFor(%q) error = %v, want nil", u, err)
		}
		if _, ok := p.(RTSPProber); !ok {
			t.Errorf("proberFor(%q) = %T, want an RTSPProber", u, p)
		}
	}
	for _, u := range []string{"http://mic/stream.wav", "https://mic/stream.wav", "HTTPS://mic/stream.wav"} {
		p, err := proberFor(Options{URL: u})
		if err != nil {
			t.Fatalf("proberFor(%q) error = %v, want nil", u, err)
		}
		if _, ok := p.(HTTPProber); !ok {
			t.Errorf("proberFor(%q) = %T, want an HTTPProber", u, p)
		}
	}
}

func TestProberForUnsupportedScheme(t *testing.T) {
	t.Parallel()
	// The URL carries credentials and a host; the usage error must name only the
	// scheme, never echo any of the rest.
	_, err := proberFor(Options{URL: "ftp://user:s3cr3t@files.example/audio"})
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("proberFor(ftp) error = %v, want ErrUsage", err)
	}
	if !strings.Contains(err.Error(), "ftp") {
		t.Errorf("usage error should name the scheme: %q", err)
	}
	for _, leak := range []string{"user", "s3cr3t", "files.example", "audio"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("usage error leaks %q from the raw URL: %q", leak, err)
		}
	}
}

func TestExecuteVersion(t *testing.T) {
	t.Parallel()
	var stdout, stderr strings.Builder
	code := Execute([]string{"--version"}, &stdout, &stderr)
	if code != ExitOK {
		t.Errorf("Execute(--version) code = %d, want %d", code, ExitOK)
	}
	out := stdout.String()
	if !strings.Contains(out, "stream-doctor") || !strings.Contains(out, Version) {
		t.Errorf("Execute(--version) stdout = %q, want it to contain %q and %q", out, "stream-doctor", Version)
	}
	if stderr.String() != "" {
		t.Errorf("Execute(--version) wrote to stderr: %q", stderr.String())
	}
}

func TestExecuteUsageError(t *testing.T) {
	t.Parallel()
	var stdout, stderr strings.Builder
	code := Execute(nil, &stdout, &stderr)
	if code != ExitUsage {
		t.Errorf("Execute(nil) code = %d, want %d", code, ExitUsage)
	}
	if stderr.String() == "" {
		t.Error("Execute(nil) wrote nothing to stderr")
	}
}
