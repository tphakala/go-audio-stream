package doctor

import (
	"os"
	"strings"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/rtsp"
)

// goldenReport builds the Report matching the M5 Task 4 brief's exact
// golden markdown fixture (testdata/report_golden.md).
func goldenReport() Report {
	return Report{
		RedactedURL: "rtsp://REDACTED@cam.example:554/Preview_01_main",
		Result:      "capture OK",
		Steps: []HandshakeStep{
			{Name: stepDial, OK: true, Elapsed: 12 * time.Millisecond},
			{Name: stepDescribe, OK: true, Elapsed: 8 * time.Millisecond},
			{Name: stepSetup, OK: true, Elapsed: 6 * time.Millisecond},
			{Name: stepPlay, OK: true, Elapsed: 7 * time.Millisecond},
		},
		Session: rtsp.SessionInfo{
			AuthScheme:      testDigestAuth,
			SessionTimeout:  60 * time.Second,
			KeepaliveMethod: testGetParameter,
			Channels:        []rtsp.ChannelPair{{TrackID: 0, RTP: 0, RTCP: 1}},
		},
		Tracks: []rtsp.Track{
			{ID: 0, Media: audiostream.MediaAudio, Codec: audiostream.CodecAAC{}, ClockRate: 16000, Channels: 1},
			{ID: 1, Media: audiostream.MediaVideo, Codec: audiostream.CodecUnknown{RTPMap: testH264}, ClockRate: 90000, Channels: 0},
		},
		AudioTrack:   rtsp.Track{ID: 0, Media: audiostream.MediaAudio, Codec: audiostream.CodecAAC{}, ClockRate: 16000, Channels: 1},
		HaveAudio:    true,
		Capture:      CaptureStats{Packets: 500, Bytes: 64000, Lost: 0, LossRatio: 0, MaxGap: 0, Bitrate: 51200, JitterMS: 0.586},
		CaptureShown: true,
		Window:       10 * time.Second,
		Reason:       EndCompleted,
		Listen:       ListenResult{Written: true, SampleRate: 16000, Channels: 1, Frames: 160000},
	}
}

func TestRenderReportGolden(t *testing.T) {
	t.Parallel()
	want, err := os.ReadFile("testdata/report_golden.md")
	if err != nil {
		t.Fatalf("reading golden fixture: %v", err)
	}
	got := renderReport(goldenReport(), testEnv())
	if got != string(want) {
		t.Errorf("renderReport mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, string(want))
	}
}

func TestRenderReportRedaction(t *testing.T) {
	t.Parallel()
	r := goldenReport()
	r.RedactedURL = rtsp.RedactURL("rtsp://admin:hunter2@cam:554/s")
	r.Listen = ListenResult{Written: true, SampleRate: 16000, Channels: 1, Frames: 160000}

	got := renderReport(r, testEnv())
	if !strings.Contains(got, "REDACTED") {
		t.Error("report does not contain REDACTED")
	}
	if strings.Contains(got, "hunter2") {
		t.Error("report leaks the password")
	}
	if strings.Contains(got, "admin") {
		t.Error("report leaks the username")
	}
	// ListenResult carries no path field, so a written WAV can never surface
	// a local file path in the report; this asserts the intended shape of
	// the Listen line rather than a specific path string.
	if strings.Contains(got, "--wav") || strings.Contains(got, ".wav") {
		t.Error("report mentions the --wav output path")
	}
}

func TestRenderReportUnsupported(t *testing.T) {
	t.Parallel()
	r := Report{
		RedactedURL: "rtsp://REDACTED@cam.example:554/stream",
		Result:      "unsupported audio codec",
		Steps: []HandshakeStep{
			{Name: stepDial, OK: true, Elapsed: 5 * time.Millisecond},
			{Name: stepDescribe, OK: true, Elapsed: 5 * time.Millisecond},
			{Name: stepSetup, OK: true, Elapsed: 5 * time.Millisecond},
			{Name: stepPlay, OK: true, Elapsed: 5 * time.Millisecond},
		},
		Session: rtsp.SessionInfo{AuthScheme: "none", KeepaliveMethod: "OPTIONS"},
		Tracks: []rtsp.Track{
			{ID: 0, Media: audiostream.MediaAudio, Codec: audiostream.CodecUnknown{RTPMap: testL16RTPMap}, ClockRate: 8000, Channels: 1},
		},
		AudioTrack: rtsp.Track{ID: 0, Media: audiostream.MediaAudio, Codec: audiostream.CodecUnknown{RTPMap: testL16RTPMap}, ClockRate: 8000, Channels: 1},
		HaveAudio:  true,
		Listen:     ListenResult{Skipped: true, SkipReason: "codec not supported for the listen check"},
	}

	got := renderReport(r, testEnv())
	if !strings.Contains(got, "**Result:** unsupported audio codec") {
		t.Errorf("report does not contain the unsupported-codec result line:\n%s", got)
	}
	if !strings.Contains(got, r.Listen.SkipReason) {
		t.Errorf("report does not contain the skip reason:\n%s", got)
	}
}

func TestRenderReportEndReasons(t *testing.T) {
	t.Parallel()
	cases := []struct {
		reason EndReason
		phrase string
	}{
		{EndCompleted, endReasonCompletedLabel},
		{EndWatchdog, "silence (read timeout)"},
		{EndTeardown, "server teardown"},
		{EndDisconnect, endReasonDisconnectLabel},
		{EndCancelled, endReasonCancelledLabel},
		{EndTruncated, "truncated (capture cap)"},
	}
	for _, tc := range cases {
		t.Run(tc.phrase, func(t *testing.T) {
			t.Parallel()
			if got := endReasonPhrase(tc.reason); got != tc.phrase {
				t.Errorf("endReasonPhrase(%v) = %q, want %q", tc.reason, got, tc.phrase)
			}
		})
	}
}
