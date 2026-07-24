package doctor

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/rtsp"
)

// TestRunPreCaptureFailure covers a failure at each pre-capture step (dial,
// setup, play): the walkthrough shows the step FAIL, the Result Phase is set,
// and the exit code is ExitConnection.
func TestRunPreCaptureFailure(t *testing.T) {
	t.Parallel()
	tracks := []rtsp.Track{aacTrack(), videoTrack()}
	tests := []struct {
		name   string
		prober *fakeProber
		step   string
		phase  Phase
	}{
		{"dial", &fakeProber{dialErr: &rtsp.ResponseError{Code: 503}}, "DIAL", PhaseDial},
		{"setup", &fakeProber{tracks: tracks, session: happySession(), setupErr: &rtsp.ResponseError{Code: 461}}, "SETUP", PhaseSetup},
		{"play", &fakeProber{tracks: tracks, session: happySession(), playErr: &rtsp.ResponseError{Code: 461}}, "PLAY", PhasePlay},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := Options{URL: testTargetURL, Duration: 10 * time.Second}
			var out strings.Builder
			res, err := Run(context.Background(), opts, tt.prober, &out, io.Discard, testEnv(), fixedClock(5*time.Millisecond))
			if err == nil {
				t.Fatalf("Run() error = nil, want a %s error", tt.step)
			}
			got := out.String()
			if !strings.Contains(got, tt.step) || !strings.Contains(got, "FAIL") {
				t.Errorf("walkthrough missing %s FAIL:\n%s", tt.step, got)
			}
			if res.Phase != tt.phase {
				t.Errorf("Phase = %d, want %d", res.Phase, tt.phase)
			}
			if code := mapExit(err, res); code != ExitConnection {
				t.Errorf("mapExit = %d, want ExitConnection", code)
			}
		})
	}
}

// TestRunRedactsFailureDetail is the end-to-end guard for the PII rule: a step
// failure whose error embeds the target host or credentials must not leak them
// into the walkthrough, which is posted publicly.
func TestRunRedactsFailureDetail(t *testing.T) {
	t.Parallel()
	f := &fakeProber{dialErr: errors.New("dial tcp cam.example:554: connect: connection refused")}
	opts := Options{URL: "rtsp://admin:hunter2@cam.example:554/stream", Duration: 10 * time.Second}

	var out strings.Builder
	_, _ = Run(context.Background(), opts, f, &out, io.Discard, testEnv(), fixedClock(time.Millisecond))
	got := out.String()
	for _, pii := range []string{"admin", "hunter2", "cam.example"} {
		if strings.Contains(got, pii) {
			t.Errorf("walkthrough leaked %q:\n%s", pii, got)
		}
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("walkthrough dropped the diagnostic text:\n%s", got)
	}
}

// TestRunListenSkipLeavesNoFile asserts the atomic write leaves nothing at the
// --wav path when the listen check skips (unsupported codec): no empty or
// clobbered file.
func TestRunListenSkipLeavesNoFile(t *testing.T) {
	t.Parallel()
	unsupported := rtsp.Track{ID: 0, Media: audiostream.MediaAudio, Codec: audiostream.CodecUnknown{RTPMap: testL16RTPMap}, ClockRate: 8000, Channels: 1}
	f := &fakeProber{
		tracks:  []rtsp.Track{unsupported},
		session: happySession(),
		result:  CaptureResult{Frames: []CapturedFrame{{Data: []byte{1, 2}}}, Reason: EndCompleted},
	}
	wavPath := filepath.Join(t.TempDir(), testWAVName)
	opts := Options{URL: testTargetURL, Duration: time.Second, WAVPath: wavPath}

	var out strings.Builder
	_, _ = Run(context.Background(), opts, f, &out, io.Discard, testEnv(), fixedClock(time.Millisecond))

	if _, err := os.Stat(wavPath); !os.IsNotExist(err) {
		t.Errorf("--wav file exists after a skipped listen check (stat err = %v); the atomic write must not create it", err)
	}
}

// TestRunListenWritesWAV drives the listen check end to end for a decodable
// (G.711) track under --report: the WAV is written atomically to the --wav
// path and the report surfaces the listen result.
func TestRunListenWritesWAV(t *testing.T) {
	t.Parallel()
	g711 := rtsp.Track{ID: 0, Media: audiostream.MediaAudio, Codec: audiostream.CodecG711{Law: audiostream.MuLaw}, ClockRate: 8000, Channels: 1}
	f := &fakeProber{
		tracks:  []rtsp.Track{g711},
		session: happySession(),
		result:  CaptureResult{Frames: []CapturedFrame{{Data: make([]byte, 320)}}, Reason: EndCompleted},
	}
	wavPath := filepath.Join(t.TempDir(), testWAVName)
	opts := Options{URL: testTargetURL, Duration: time.Second, WAVPath: wavPath, Report: true}

	var out, errOut strings.Builder
	_, _ = Run(context.Background(), opts, f, &out, &errOut, testEnv(), fixedClock(time.Millisecond))

	info, err := os.Stat(wavPath)
	if err != nil {
		t.Fatalf("--wav file not written: %v", err)
	}
	if info.Size() == 0 {
		t.Error("--wav file is empty")
	}
	if !strings.Contains(out.String(), "Listen:") {
		t.Errorf("report (out) missing the Listen line:\n%s", out.String())
	}
}

// TestRunListenRenameFailurePIIFree makes the final rename fail (the --wav path
// is an existing directory) and asserts the skip reason discloses no filesystem
// path: os.Rename returns an *os.LinkError carrying both the temp and
// destination paths.
func TestRunListenRenameFailurePIIFree(t *testing.T) {
	t.Parallel()
	g711 := rtsp.Track{ID: 0, Media: audiostream.MediaAudio, Codec: audiostream.CodecG711{Law: audiostream.MuLaw}, ClockRate: 8000, Channels: 1}
	f := &fakeProber{
		tracks:  []rtsp.Track{g711},
		session: happySession(),
		result:  CaptureResult{Frames: []CapturedFrame{{Data: make([]byte, 320)}}, Reason: EndCompleted},
	}
	wavPath := t.TempDir() // an existing directory makes os.Rename fail
	opts := Options{URL: testTargetURL, Duration: time.Second, WAVPath: wavPath, Report: true}

	var out, errOut strings.Builder
	_, _ = Run(context.Background(), opts, f, &out, &errOut, testEnv(), fixedClock(time.Millisecond))

	report := out.String()
	if strings.Contains(report, wavPath) || strings.Contains(report, ".stream-doctor-") {
		t.Errorf("report leaks a filesystem path after a rename failure:\n%s", report)
	}
	if !strings.Contains(report, "skipped") {
		t.Errorf("report should show the listen check as skipped:\n%s", report)
	}
}

func TestSanitizeWriteErr(t *testing.T) {
	t.Parallel()
	// A *os.PathError embeds the local --wav path; sanitize must strip it.
	// The crafted path is built from testWAVName so the leak assertion
	// below always checks a fragment that is actually in the input.
	pathErr := &os.PathError{Op: "write", Path: "/home/user/secret/" + testWAVName, Err: errors.New("no space left on device")}
	got := sanitizeWriteErr(pathErr)
	if strings.Contains(got, "/home/user") || strings.Contains(got, testWAVName) {
		t.Errorf("sanitizeWriteErr leaked the path: %q", got)
	}
	if !strings.Contains(got, "no space left on device") {
		t.Errorf("sanitizeWriteErr dropped the cause: %q", got)
	}
	// A plain error is passed through with the prefix.
	if got := sanitizeWriteErr(errors.New("boom")); got != "wav write failed: boom" {
		t.Errorf("sanitizeWriteErr(plain) = %q, want %q", got, "wav write failed: boom")
	}
}

func TestEndReasonString(t *testing.T) {
	t.Parallel()
	cases := map[EndReason]string{
		EndCompleted:  "completed",
		EndWatchdog:   "watchdog",
		EndTeardown:   "teardown",
		EndDisconnect: "disconnect",
		EndCancelled:  "cancelled",
		EndTruncated:  "truncated",
		EndReason(99): "unknown",
	}
	for r, want := range cases {
		if got := r.String(); got != want {
			t.Errorf("EndReason(%d).String() = %q, want %q", int(r), got, want)
		}
	}
}
