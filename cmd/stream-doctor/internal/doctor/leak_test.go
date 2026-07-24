package doctor

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/rtsp"
)

// TestRunReportScrubsServerAndFMTP proves the new diagnostic fields are scrubbed
// end to end: a hostile stream that puts the target host, a resolved IP, and a
// code fence into the Server header and the raw fmtp must not leak any of them
// into the paste-ready report, and must not break out of the report's fence.
func TestRunReportScrubsServerAndFMTP(t *testing.T) {
	t.Parallel()
	poison := testHost + " 10.1.2.3 ```pwn"
	aac := audiostream.CodecAAC{AudioSpecificConfig: []byte{0x14, 0x08}}
	f := &fakeProber{
		tracks: []rtsp.Track{
			{ID: 0, Media: audiostream.MediaAudio, Codec: aac, ClockRate: 16000, Channels: 1, FMTP: poison},
		},
		session: rtsp.SessionInfo{
			Server:          poison,
			AuthScheme:      rtsp.AuthDigest,
			KeepaliveMethod: "OPTIONS",
			SessionTimeout:  60 * time.Second,
			Channels:        []rtsp.ChannelPair{{TrackID: 0, RTP: 0, RTCP: 1}},
		},
		result: CaptureResult{
			Frames:  frames500(),
			Stats:   audiostream.TrackStats{Packets: 500, Bytes: 64000},
			Window:  time.Second,
			Elapsed: time.Second,
			Reason:  EndCompleted,
		},
	}
	opts := Options{URL: testTargetURL, Duration: time.Second, Report: true}

	var out strings.Builder
	if _, err := Run(context.Background(), opts, f, &out, io.Discard, testEnv(), fixedClock(time.Millisecond)); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	got := out.String()
	for _, bad := range []string{testHost, "10.1.2.3"} {
		if strings.Contains(got, bad) {
			t.Errorf("report leaks %q via a scrubbed field:\n%s", bad, got)
		}
	}
	// The report opens and closes with exactly one fence each; a third run of
	// backticks would mean the poisoned field escaped the fence.
	if n := strings.Count(got, reportFence); n != 2 {
		t.Errorf("report has %d code fences, want 2 (a field broke out of the fence):\n%s", n, got)
	}
}

// TestRunReportFailureLineScrubbed proves the failure line, the single most
// important diagnostic on a failed run, carries no PII: a dial error that
// embeds the host and a resolved IP must surface the cause without leaking
// either.
func TestRunReportFailureLineScrubbed(t *testing.T) {
	t.Parallel()
	f := &fakeProber{
		dialErr: errors.New("dial tcp cam.example 10.1.2.3:554: connect: connection refused"),
	}
	opts := Options{URL: testTargetURL, Report: true}

	var out strings.Builder
	_, err := Run(context.Background(), opts, f, &out, io.Discard, testEnv(), fixedClock(time.Millisecond))
	if err == nil {
		t.Fatal("Run() error = nil, want dial error")
	}
	got := out.String()
	if !strings.Contains(got, "failure: DIAL - ") {
		t.Errorf("report missing the failure line naming the failed step:\n%s", got)
	}
	for _, bad := range []string{testHost, "10.1.2.3"} {
		if strings.Contains(got, bad) {
			t.Errorf("failure line leaks %q:\n%s", bad, got)
		}
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("failure line dropped the diagnostic cause:\n%s", got)
	}
}
