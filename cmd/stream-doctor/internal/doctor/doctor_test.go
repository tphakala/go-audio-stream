package doctor

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/rtsp"
)

// testEnv is the deterministic machine context for the golden tests.
func testEnv() Env {
	return Env{OS: "linux", Arch: "amd64", Version: "0.1.0"}
}

const testTargetURL = "rtsp://user:pass@cam.example:554/stream"

const testH264 = "H264/90000"
const testDigestAuth = rtsp.AuthDigest
const testGetParameter = "GET_PARAMETER"
const testL16RTPMap = "L16/8000"

func aacTrack() rtsp.Track {
	return rtsp.Track{ID: 0, Media: audiostream.MediaAudio, Codec: audiostream.CodecAAC{}, ClockRate: 16000, Channels: 1}
}

func videoTrack() rtsp.Track {
	return rtsp.Track{ID: 1, Media: audiostream.MediaVideo, Codec: audiostream.CodecUnknown{RTPMap: testH264}, ClockRate: 90000, Channels: 0}
}

func happySession() rtsp.SessionInfo {
	return rtsp.SessionInfo{
		SessionTimeout:  60 * time.Second,
		AuthScheme:      testDigestAuth,
		KeepaliveMethod: testGetParameter,
		Channels:        []rtsp.ChannelPair{{TrackID: 0, RTP: 0, RTCP: 1}},
	}
}

// frames500 builds 500 perfectly regular frames so computeJitter is exactly 0
// and the golden capture block is deterministic.
func frames500() []CapturedFrame {
	fs := make([]CapturedFrame, 500)
	base := time.Unix(100, 0)
	for i := range fs {
		d := time.Duration(i) * 20 * time.Millisecond
		fs[i] = CapturedFrame{
			RTPTime:    uint32(i) * 320,
			PTS:        d,
			ReceivedAt: base.Add(d),
		}
	}
	return fs
}

// seqClock returns a now func yielding base plus each successive offset,
// clamping to the last offset once exhausted, so per-step elapsed values are
// deterministic.
func seqClock(offsets ...time.Duration) func() time.Time {
	base := time.Unix(0, 0)
	i := 0
	return func() time.Time {
		off := offsets[len(offsets)-1]
		if i < len(offsets) {
			off = offsets[i]
		}
		i++
		return base.Add(off)
	}
}

// happyClock produces the 12/8/6/7 ms handshake timings of the golden block.
func happyClock() func() time.Time {
	ms := time.Millisecond
	return seqClock(0, 12*ms, 12*ms, 20*ms, 20*ms, 26*ms, 26*ms, 33*ms)
}

// fixedClock advances step per call; used where step elapsed is not asserted.
func fixedClock(step time.Duration) func() time.Time {
	base := time.Unix(0, 0)
	var n int64
	return func() time.Time {
		t := base.Add(time.Duration(n) * step)
		n++
		return t
	}
}

const happyGolden = `stream-doctor 0.1.0 (linux/amd64)
target: rtsp://REDACTED@cam.example:554/stream

handshake
  DIAL       ok    12ms   auth Digest, keepalive GET_PARAMETER
  DESCRIBE   ok     8ms   1 audio track, 1 video track
  SETUP      ok     6ms   track 0, channels 0-1
  PLAY       ok     7ms   session timeout 60s

tracks
  #  kind   codec                clock   ch  depacketize
  0  audio  AAC                  16000    1  yes
  1  video  H264/90000           90000    -  no

capture (10s, track 0, ended: completed)
  packets   500
  bytes     64000
  lost      0 (0.00%)
  max gap   0
  bitrate   51.2 kbit/s
  jitter    0.00 ms
`

func TestRunHappyPathWalkthrough(t *testing.T) {
	t.Parallel()
	f := &fakeProber{
		tracks:  []rtsp.Track{aacTrack(), videoTrack()},
		session: happySession(),
		result: CaptureResult{
			Frames:  frames500(),
			Stats:   audiostream.TrackStats{Packets: 500, Bytes: 64000},
			Window:  10 * time.Second,
			Elapsed: 10 * time.Second,
			Reason:  EndCompleted,
		},
	}
	opts := Options{URL: testTargetURL, Duration: 10 * time.Second}

	var out, errOut strings.Builder
	res, err := Run(context.Background(), opts, f, &out, &errOut, testEnv(), happyClock())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if got := out.String(); got != happyGolden {
		t.Errorf("walkthrough mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, happyGolden)
	}
	want := Result{Phase: PhaseCapture, AudioTrackFound: true, CodecSupported: true, FramesCaptured: 500}
	if res != want {
		t.Errorf("Result = %+v, want %+v", res, want)
	}
	if code := mapExit(err, res); code != ExitOK {
		t.Errorf("mapExit = %d, want ExitOK", code)
	}
}

func TestRunNoAudioTrack(t *testing.T) {
	t.Parallel()
	f := &fakeProber{
		tracks:  []rtsp.Track{videoTrack()},
		session: happySession(),
	}
	opts := Options{URL: testTargetURL, Duration: 10 * time.Second}

	var out strings.Builder
	res, err := Run(context.Background(), opts, f, &out, io.Discard, testEnv(), fixedClock(5*time.Millisecond))
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if res.AudioTrackFound {
		t.Error("AudioTrackFound = true, want false")
	}
	if !strings.Contains(out.String(), "no audio track") {
		t.Errorf("walkthrough missing no-audio message:\n%s", out.String())
	}
	if code := mapExit(err, res); code != ExitNoAudioTrack {
		t.Errorf("mapExit = %d, want ExitNoAudioTrack", code)
	}
}

func TestRunDialFails(t *testing.T) {
	t.Parallel()
	dialErr := &rtsp.ResponseError{Code: 503}
	f := &fakeProber{dialErr: dialErr}
	opts := Options{URL: testTargetURL, Duration: 10 * time.Second}

	var out strings.Builder
	res, err := Run(context.Background(), opts, f, &out, io.Discard, testEnv(), fixedClock(5*time.Millisecond))
	if err == nil {
		t.Fatal("Run() error = nil, want dial error")
	}
	got := out.String()
	if !strings.Contains(got, "DIAL") || !strings.Contains(got, "FAIL") {
		t.Errorf("walkthrough missing DIAL FAIL:\n%s", got)
	}
	if res.Phase != PhaseDial {
		t.Errorf("Phase = %d, want PhaseDial", res.Phase)
	}
	if code := mapExit(err, res); code != ExitConnection {
		t.Errorf("mapExit = %d, want ExitConnection", code)
	}
}

func TestRunDescribeAuthFails(t *testing.T) {
	t.Parallel()
	f := &fakeProber{
		session:     happySession(),
		describeErr: rtsp.ErrAuthFailed,
	}
	opts := Options{URL: testTargetURL, Duration: 10 * time.Second}

	var out strings.Builder
	res, err := Run(context.Background(), opts, f, &out, io.Discard, testEnv(), fixedClock(5*time.Millisecond))
	if err == nil {
		t.Fatal("Run() error = nil, want auth error")
	}
	got := out.String()
	if !strings.Contains(got, "DESCRIBE") || !strings.Contains(got, "FAIL") {
		t.Errorf("walkthrough missing DESCRIBE FAIL:\n%s", got)
	}
	if code := mapExit(err, res); code != ExitAuth {
		t.Errorf("mapExit = %d, want ExitAuth", code)
	}
}

func TestRunSetupFails(t *testing.T) {
	t.Parallel()
	f := &fakeProber{
		tracks:   []rtsp.Track{aacTrack(), videoTrack()},
		session:  happySession(),
		setupErr: &rtsp.ResponseError{Code: 461},
	}
	opts := Options{URL: testTargetURL, Duration: 10 * time.Second}

	var out strings.Builder
	res, err := Run(context.Background(), opts, f, &out, io.Discard, testEnv(), fixedClock(5*time.Millisecond))
	if err == nil {
		t.Fatal("Run() error = nil, want setup error")
	}
	got := out.String()
	if !strings.Contains(got, "SETUP") || !strings.Contains(got, "FAIL") {
		t.Errorf("walkthrough missing SETUP FAIL:\n%s", got)
	}
	if res.Phase != PhaseSetup {
		t.Errorf("Phase = %d, want PhaseSetup", res.Phase)
	}
	if code := mapExit(err, res); code != ExitConnection {
		t.Errorf("mapExit = %d, want ExitConnection", code)
	}
}

func TestRunCaptureSilent(t *testing.T) {
	t.Parallel()
	f := &fakeProber{
		tracks:  []rtsp.Track{aacTrack(), videoTrack()},
		session: happySession(),
		result: CaptureResult{
			Frames:  nil,
			Stats:   audiostream.TrackStats{},
			Window:  10 * time.Second,
			Elapsed: 15 * time.Second,
			Reason:  EndWatchdog,
		},
	}
	opts := Options{URL: testTargetURL, Duration: 10 * time.Second}

	var out strings.Builder
	res, err := Run(context.Background(), opts, f, &out, io.Discard, testEnv(), fixedClock(5*time.Millisecond))
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if res.FramesCaptured != 0 {
		t.Errorf("FramesCaptured = %d, want 0", res.FramesCaptured)
	}
	got := out.String()
	if !strings.Contains(got, "ended: watchdog") {
		t.Errorf("walkthrough missing watchdog reason:\n%s", got)
	}
	if !strings.Contains(got, "CAPTURE") || !strings.Contains(got, "FAIL") {
		t.Errorf("walkthrough missing CAPTURE FAIL:\n%s", got)
	}
	if code := mapExit(err, res); code != ExitCapture {
		t.Errorf("mapExit = %d, want ExitCapture", code)
	}
}

func TestRunUnsupportedCodec(t *testing.T) {
	t.Parallel()
	unknownAudio := rtsp.Track{ID: 0, Media: audiostream.MediaAudio, Codec: audiostream.CodecUnknown{RTPMap: testL16RTPMap}, ClockRate: 8000, Channels: 1}
	f := &fakeProber{
		tracks:  []rtsp.Track{unknownAudio},
		session: happySession(),
		result: CaptureResult{
			Frames:  frames500(),
			Stats:   audiostream.TrackStats{Packets: 500, Bytes: 64000},
			Window:  10 * time.Second,
			Elapsed: 10 * time.Second,
			Reason:  EndCompleted,
		},
	}
	opts := Options{URL: testTargetURL, Duration: 10 * time.Second}

	var out strings.Builder
	res, err := Run(context.Background(), opts, f, &out, io.Discard, testEnv(), fixedClock(5*time.Millisecond))
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if res.CodecSupported {
		t.Error("CodecSupported = true, want false")
	}
	if !strings.Contains(out.String(), "  no") {
		t.Errorf("walkthrough should mark codec depacketize no:\n%s", out.String())
	}
	if code := mapExit(err, res); code != ExitUnsupported {
		t.Errorf("mapExit = %d, want ExitUnsupported", code)
	}
}

func TestRunSetupAudioOnly(t *testing.T) {
	t.Parallel()
	f := &fakeProber{
		tracks:  []rtsp.Track{aacTrack(), videoTrack()},
		session: happySession(),
		result: CaptureResult{
			Frames:  frames500(),
			Stats:   audiostream.TrackStats{Packets: 500, Bytes: 64000},
			Elapsed: 10 * time.Second,
			Reason:  EndCompleted,
		},
	}
	opts := Options{URL: testTargetURL, Duration: 10 * time.Second, FullStream: false}

	res, err := Run(context.Background(), opts, f, io.Discard, io.Discard, testEnv(), fixedClock(5*time.Millisecond))
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	want := []setupCall{{TrackID: 0, Discard: false}}
	if !equalSetups(f.setups, want) {
		t.Errorf("setups = %+v, want %+v", f.setups, want)
	}
	if code := mapExit(err, res); code != ExitOK {
		t.Errorf("mapExit = %d, want ExitOK", code)
	}
}

func TestRunSetupFullStream(t *testing.T) {
	t.Parallel()
	f := &fakeProber{
		tracks: []rtsp.Track{aacTrack(), videoTrack()},
		session: rtsp.SessionInfo{
			SessionTimeout:  60 * time.Second,
			AuthScheme:      testDigestAuth,
			KeepaliveMethod: testGetParameter,
			Channels:        []rtsp.ChannelPair{{TrackID: 0, RTP: 0, RTCP: 1}, {TrackID: 1, RTP: 2, RTCP: 3}},
		},
		result: CaptureResult{
			Frames:  frames500(),
			Stats:   audiostream.TrackStats{Packets: 500, Bytes: 64000},
			Elapsed: 10 * time.Second,
			Reason:  EndCompleted,
		},
	}
	opts := Options{URL: testTargetURL, Duration: 10 * time.Second, FullStream: true}

	var out strings.Builder
	res, err := Run(context.Background(), opts, f, &out, io.Discard, testEnv(), fixedClock(5*time.Millisecond))
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	want := []setupCall{{TrackID: 0, Discard: false}, {TrackID: 1, Discard: true}}
	if !equalSetups(f.setups, want) {
		t.Errorf("setups = %+v, want %+v", f.setups, want)
	}
	if !strings.Contains(out.String(), "1 track discarded") {
		t.Errorf("walkthrough SETUP line missing discard count:\n%s", out.String())
	}
	if code := mapExit(err, res); code != ExitOK {
		t.Errorf("mapExit = %d, want ExitOK", code)
	}
}

func TestRunSetupFullStreamTwoNonAudio(t *testing.T) {
	t.Parallel()
	otherTrack := rtsp.Track{ID: 2, Media: audiostream.MediaOther, Codec: audiostream.CodecUnknown{RTPMap: "application/x"}, ClockRate: 90000}
	f := &fakeProber{
		tracks:  []rtsp.Track{aacTrack(), videoTrack(), otherTrack},
		session: happySession(),
		result: CaptureResult{
			Frames:  frames500(),
			Stats:   audiostream.TrackStats{Packets: 500, Bytes: 64000},
			Elapsed: 10 * time.Second,
			Reason:  EndCompleted,
		},
	}
	opts := Options{URL: testTargetURL, Duration: 10 * time.Second, FullStream: true}

	var out strings.Builder
	_, err := Run(context.Background(), opts, f, &out, io.Discard, testEnv(), fixedClock(5*time.Millisecond))
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	want := []setupCall{{TrackID: 0, Discard: false}, {TrackID: 1, Discard: true}, {TrackID: 2, Discard: true}}
	if !equalSetups(f.setups, want) {
		t.Errorf("setups = %+v, want %+v", f.setups, want)
	}
	if !strings.Contains(out.String(), "2 tracks discarded") {
		t.Errorf("walkthrough SETUP line missing discard count:\n%s", out.String())
	}
}

func equalSetups(got, want []setupCall) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
