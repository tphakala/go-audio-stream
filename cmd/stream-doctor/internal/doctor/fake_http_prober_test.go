package doctor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/httpsource"
	"github.com/tphakala/go-audio-stream/rtsp"
)

// fakeHTTPProber is a scripted HTTPProber for the engine-level HTTP tests. Open
// returns its scripted error (nil by default) and, on success, exposes the
// scripted track and info; Collect returns the scripted CaptureResult. It
// records the call order so a test can assert the dispatch.
type fakeHTTPProber struct {
	openErr error
	track   rtsp.Track
	info    audiostream.SourceInfo
	result  CaptureResult
	calls   []string
}

// compile-time: fakeHTTPProber implements HTTPProber (and thus Prober), so
// Run's type switch drives it through the single OPEN step.
var _ HTTPProber = (*fakeHTTPProber)(nil)

// testHTTPURL is the plaintext http target shared by the engine-level HTTP
// tests (no credentials).
const testHTTPURL = "http://mic.local/stream.wav"

func (f *fakeHTTPProber) Open(_ context.Context) error {
	f.calls = append(f.calls, "Open")
	return f.openErr
}

func (f *fakeHTTPProber) Track() rtsp.Track { return f.track }

func (f *fakeHTTPProber) Info() audiostream.SourceInfo { return f.info }

func (f *fakeHTTPProber) Collect(_ context.Context, _ rtsp.Track, _ time.Duration) (CaptureResult, error) {
	f.calls = append(f.calls, "Collect")
	return f.result, nil
}

func (f *fakeHTTPProber) Close() error {
	f.calls = append(f.calls, "Close")
	return nil
}

// bareProber implements only the narrow Prober (Collect/Close), satisfying
// neither RTSPProber nor HTTPProber, so Run's type switch falls to its default
// branch. It exists to pin that an unsupported prober kind fails as a usage
// error rather than rendering a misleading clean result.
type bareProber struct{}

// compile-time: bareProber implements Prober but neither negotiation surface.
var _ Prober = bareProber{}

func (bareProber) Collect(context.Context, rtsp.Track, time.Duration) (CaptureResult, error) {
	return CaptureResult{}, nil
}

func (bareProber) Close() error { return nil }

// TestRunUnsupportedProberKind asserts a prober that is neither an RTSPProber
// nor an HTTPProber terminates with a wrapped usage error and ExitUsage, never
// a nil-error clean result. proberFor never yields such a prober in production;
// this guards the unreachable default branch against silently reporting success.
func TestRunUnsupportedProberKind(t *testing.T) {
	t.Parallel()
	opts := Options{URL: "rtsp://cam/stream", Duration: time.Second}

	var out strings.Builder
	res, err := Run(context.Background(), opts, bareProber{}, &out, io.Discard, testEnv(), fixedClock(time.Millisecond))
	if err == nil {
		t.Fatal("Run() error = nil, want a usage error for a bare prober")
	}
	if !errors.Is(err, ErrUsage) {
		t.Errorf("Run() error = %v, want it to wrap ErrUsage", err)
	}
	if code := mapExit(err, res); code != ExitUsage {
		t.Errorf("mapExit = %d, want ExitUsage", code)
	}
}

// httpL16Track is the synthesized single L16 track the doctor builds for an
// HTTP source: ID 0, no RTP payload type, s16le at 44100 Hz stereo.
func httpL16Track() rtsp.Track {
	return rtsp.Track{
		ID:          0,
		Media:       audiostream.MediaAudio,
		Codec:       audiostream.CodecL16{ClockRate: 44100, Channels: 2},
		ClockRate:   44100,
		Channels:    2,
		PayloadType: -1,
	}
}

// httpPCMFrames builds n regular L16 stereo frames of little-endian PCM so the
// capture is non-empty and the listen check has bytes to write.
func httpPCMFrames(n int) []CapturedFrame {
	fs := make([]CapturedFrame, n)
	base := time.Unix(200, 0)
	for i := range fs {
		d := time.Duration(i) * 10 * time.Millisecond
		fs[i] = CapturedFrame{
			Data:       []byte{0x01, 0x02, 0x03, 0x04},
			PTS:        d,
			ReceivedAt: base.Add(d),
		}
	}
	return fs
}

// TestRunHTTPDispatchAndOpen asserts the engine picks the single OPEN step for
// an HTTPProber (never the RTSP DIAL/DESCRIBE/SETUP/PLAY group), that a
// successful open populates the synthesized L16 track and renders it, and that
// a clean HTTP capture maps to ExitOK.
func TestRunHTTPDispatchAndOpen(t *testing.T) {
	t.Parallel()
	f := &fakeHTTPProber{
		track: httpL16Track(),
		info:  audiostream.SourceInfo{URL: testHTTPURL, Server: "ESP32-Audio/1.0"},
		result: CaptureResult{
			Frames:     httpPCMFrames(50),
			Stats:      audiostream.TrackStats{Packets: 50, PayloadBytes: 200, LastFrameAt: time.Unix(210, 0)},
			CapturedAt: time.Unix(210, 400_000_000),
			Window:     10 * time.Second,
			Elapsed:    10 * time.Second,
			Reason:     EndStreamEnded,
		},
	}
	opts := Options{URL: testHTTPURL, Duration: 10 * time.Second}

	var out strings.Builder
	res, err := Run(context.Background(), opts, f, &out, io.Discard, testEnv(), fixedClock(5*time.Millisecond))
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	// The pre-capture surface must be the single OPEN step, then Collect.
	if len(f.calls) < 2 || f.calls[0] != "Open" || f.calls[1] != "Collect" {
		t.Errorf("call order = %v, want [Open Collect ...]", f.calls)
	}
	got := out.String()
	for _, want := range []string{
		"OPEN",
		"s16le 44100 Hz stereo, server ESP32-Audio/1.0",
		"track 0: audio, L16",
		"ended: stream-end",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("walkthrough missing %q:\n%s", want, got)
		}
	}
	// The RTSP-only lines must never appear for an HTTP run.
	for _, absent := range []string{stepDial, stepDescribe, stepSetup, stepPlay} {
		if strings.Contains(got, absent) {
			t.Errorf("HTTP walkthrough leaked the RTSP step %q:\n%s", absent, got)
		}
	}
	// The sender-clock line is RTCP-only and must be gated off for HTTP.
	if strings.Contains(got, "sender clock") {
		t.Errorf("HTTP walkthrough rendered the RTCP sender-clock line:\n%s", got)
	}
	want := Result{Phase: PhaseCapture, AudioTrackFound: true, CodecSupported: true, FramesCaptured: 50}
	if res != want {
		t.Errorf("Result = %+v, want %+v", res, want)
	}
	if code := mapExit(err, res); code != ExitOK {
		t.Errorf("mapExit = %d, want ExitOK", code)
	}
}

// TestRunHTTPReportSession asserts the report's minimal HTTP session block: the
// scrubbed server, the auth label, and the fixed transport line, with none of
// the RTSP session lines.
func TestRunHTTPReportSession(t *testing.T) {
	t.Parallel()
	f := &fakeHTTPProber{
		track: httpL16Track(),
		info:  audiostream.SourceInfo{URL: testHTTPURL, Server: "ESP32-Audio/1.0"},
		result: CaptureResult{
			Frames:     httpPCMFrames(10),
			Stats:      audiostream.TrackStats{Packets: 10, PayloadBytes: 40, LastFrameAt: time.Unix(210, 0)},
			CapturedAt: time.Unix(210, 0),
			Window:     10 * time.Second,
			Elapsed:    10 * time.Second,
			Reason:     EndStreamEnded,
		},
	}
	// Credentials supplied, so the auth label must read "basic".
	opts := Options{URL: "http://user:pass@mic.local/stream.wav", Duration: 10 * time.Second, Report: true}

	var out, errOut strings.Builder
	if _, err := Run(context.Background(), opts, f, &out, &errOut, testEnv(), fixedClock(5*time.Millisecond)); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	got := out.String()
	for _, want := range []string{
		"  server: ESP32-Audio/1.0\n",
		"  auth: basic\n",
		"  transport: HTTP progressive\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing HTTP session line %q:\n%s", want, got)
		}
	}
	for _, absent := range []string{"session-timeout", "keepalive", "TCP interleaved", "sender-clock"} {
		if strings.Contains(got, absent) {
			t.Errorf("report leaked the RTSP-only line %q for an HTTP run:\n%s", absent, got)
		}
	}
	// The target credentials and host must never survive into the report.
	for _, leak := range []string{"user:pass", "mic.local"} {
		if strings.Contains(got, leak) {
			t.Errorf("report leaks %q:\n%s", leak, got)
		}
	}
}

// TestRunHTTPOpenFailures pins each open-failure class to its report result
// phrase and its exit code.
func TestRunHTTPOpenFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		err    error
		phrase string
		code   int
	}{
		{"401", &httpsource.StatusError{Code: http.StatusUnauthorized}, authFailedPhrase, ExitAuth},
		{"insecure auth", httpsource.ErrInsecureAuth, "credentials refused over plaintext http (use -insecure-auth)", ExitUsage},
		{"unsupported format", httpsource.ErrUnsupportedFormat, unsupportedFormatPhrase, ExitUnsupported},
		{"malformed wav", httpsource.ErrMalformedWAV, unsupportedFormatPhrase, ExitUnsupported},
		{"format unknown", httpsource.ErrFormatUnknown, unsupportedFormatPhrase, ExitUnsupported},
		{"redirect", &audiostream.RedirectError{Location: "http://elsewhere/x"}, "redirected (not followed)", ExitConnection},
		{"generic", errors.New("dial tcp: connection refused"), "connection failed", ExitConnection},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeHTTPProber{openErr: tc.err}
			opts := Options{URL: testHTTPURL, Duration: 10 * time.Second, Report: true}

			var out, errOut strings.Builder
			res, err := Run(context.Background(), opts, f, &out, &errOut, testEnv(), fixedClock(5*time.Millisecond))
			if err == nil {
				t.Fatal("Run() error = nil, want the open error")
			}
			got := out.String()
			if !strings.Contains(got, "result: "+tc.phrase) {
				t.Errorf("report result phrase mismatch, want %q:\n%s", tc.phrase, got)
			}
			if !strings.Contains(got, "OPEN") || !strings.Contains(got, "FAIL") {
				t.Errorf("report missing the OPEN FAIL step:\n%s", got)
			}
			// A failed open captured nothing and set up no track.
			if strings.Contains(got, "track 0:") {
				t.Errorf("report rendered a track after a failed open:\n%s", got)
			}
			if code := mapExit(err, res); code != tc.code {
				t.Errorf("mapExit = %d, want %d", code, tc.code)
			}
		})
	}
}
