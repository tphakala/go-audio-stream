package doctor

import (
	"errors"
	"strings"
	"testing"

	audiostream "github.com/tphakala/go-audio-stream"
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
