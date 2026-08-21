package doctor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	aac "github.com/tphakala/go-aac"
	aacpcm "github.com/tphakala/go-aac/pcm"
	wavpcm "github.com/tphakala/go-wav/pcm"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/rtsp"
)

// aacListenFrames encodes numFrames access units of a sine tone with
// go-aac/pcm's low-level encoder and returns them as CapturedFrame (the
// fixed 7-byte ADTS header stripped, matching what RTP AAC depacketization
// delivers) plus the encoder's AudioSpecificConfig, so the end-to-end test
// exercises writeWAVAAC's real decode path rather than a synthetic payload.
func aacListenFrames(t *testing.T, sampleRate, channels, numFrames int) (frames []CapturedFrame, asc []byte) {
	t.Helper()

	fw := &frameWriter{}
	enc, err := aacpcm.NewEncoder(fw, aacpcm.Config{SampleRate: sampleRate, BitDepth: 16, Channels: channels})
	if err != nil {
		t.Fatalf("aacpcm.NewEncoder: %v", err)
	}
	pcm := make([]int16, aac.FrameSize*channels*numFrames)
	fillSine(pcm, sampleRate, channels, 0)
	if _, wErr := enc.Write(int16sToLE(pcm)); wErr != nil {
		t.Fatalf("Write: %v", wErr)
	}
	if cErr := enc.Close(); cErr != nil {
		t.Fatalf("Close: %v", cErr)
	}
	asc = enc.AudioSpecificConfig()
	if len(asc) == 0 {
		t.Fatal("AudioSpecificConfig is empty")
	}

	frameDur := time.Duration(aac.FrameSize) * time.Second / time.Duration(sampleRate)
	base := time.Unix(300, 0)
	frames = make([]CapturedFrame, 0, len(fw.frames))
	for i, adtsFrame := range fw.frames {
		if len(adtsFrame) < 7 {
			t.Fatalf("ADTS frame too short: %d bytes", len(adtsFrame))
		}
		d := time.Duration(i) * frameDur
		frames = append(frames, CapturedFrame{
			Data:       append([]byte(nil), adtsFrame[7:]...),
			PTS:        d,
			ReceivedAt: base.Add(d),
		})
	}
	if len(frames) == 0 {
		t.Fatal("no access units captured from the encoder")
	}
	return frames, asc
}

// TestRunReportAndWAV drives Run end to end over a scripted Prober with
// --report and --wav both set: a full handshake, a capture of real AAC
// access units, the WAV listen check writing to a temp file, and the
// markdown report to stdout. It asserts the WAV file exists and decodes,
// that stdout carries the report rather than the walkthrough, and that
// mapExit classifies the clean run as ExitOK.
func TestRunReportAndWAV(t *testing.T) {
	t.Parallel()
	const sampleRate = 48000
	const channels = 1
	const numFrames = 5

	frames, asc := aacListenFrames(t, sampleRate, channels, numFrames)
	track := rtsp.Track{
		ID: 0, Media: audiostream.MediaAudio,
		Codec:     audiostream.CodecAAC{AudioSpecificConfig: asc},
		ClockRate: sampleRate, Channels: channels,
	}

	var totalBytes int
	for i := range frames {
		totalBytes += len(frames[i].Data)
	}

	wavPath := filepath.Join(t.TempDir(), "capture.wav")
	f := &fakeProber{
		tracks:  []rtsp.Track{track},
		session: happySession(),
		result: CaptureResult{
			Frames:     frames,
			Stats:      audiostream.TrackStats{Packets: uint64(len(frames)), PayloadBytes: uint64(totalBytes), WireBytes: uint64(totalBytes) + 100}, //nolint:gosec // test data, bounded by numFrames above.
			CapturedAt: time.Unix(400, 0),
			Window:     10 * time.Second,
			Elapsed:    10 * time.Second,
			Reason:     EndCompleted,
		},
	}
	opts := Options{URL: testTargetURL, Duration: 10 * time.Second, Report: true, WAVPath: wavPath}

	var out, errOut strings.Builder
	res, err := Run(context.Background(), opts, f, &out, &errOut, testEnv(), fixedClock(5*time.Millisecond))
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if code := mapExit(err, res); code != ExitOK {
		t.Errorf("mapExit = %d, want ExitOK", code)
	}

	got := out.String()
	// The report is a single fenced block; the walkthrough is not fenced, so
	// the fence is what distinguishes them now that both carry a "handshake"
	// section header.
	if !strings.HasPrefix(got, reportFence) {
		t.Errorf("stdout is not the fenced report:\n%s", got)
	}
	// The report is pasted publicly by users: a successful run must not
	// surface the credentials or host from the target URL, nor the --wav
	// output path.
	for _, frag := range []string{wavPath, "user:pass", "cam.example"} {
		if strings.Contains(got, frag) {
			t.Errorf("report leaks %q:\n%s", frag, got)
		}
	}
	// The telemetry lines are part of the report: wire-bytes surfaces because
	// the fake reported WireBytes, and last-frame is always present.
	for _, frag := range []string{"wire-bytes:", "last-frame:"} {
		if !strings.Contains(got, frag) {
			t.Errorf("report missing the telemetry line %q:\n%s", frag, got)
		}
	}

	wavBytes, rerr := os.ReadFile(wavPath)
	if rerr != nil {
		t.Fatalf("reading WAV output: %v", rerr)
	}
	info, decoded, derr := wavpcm.DecodeInterleaved(wavBytes)
	if derr != nil {
		t.Fatalf("decoding WAV output: %v", derr)
	}
	if info.SampleRate != sampleRate || info.Channels != channels || info.BitDepth != 16 {
		t.Errorf("decoded WAV = %+v, want %d Hz, %d ch, 16-bit", info, sampleRate, channels)
	}
	if len(decoded) == 0 {
		t.Error("decoded WAV PCM is empty")
	}
}

// TestRunListenCreateFailureRedactsPath forces the listen check's temp-file
// creation to fail deterministically on every platform, root included: the
// --wav path's parent is a regular file, so os.CreateTemp in that
// "directory" can never succeed. The failure must surface as a skipped
// listen with the generic create-failure reason and no filesystem path in
// the report.
func TestRunListenCreateFailureRedactsPath(t *testing.T) {
	t.Parallel()
	parent := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	wavPath := filepath.Join(parent, testWAVName)

	track := rtsp.Track{ID: 0, Media: audiostream.MediaAudio, Codec: audiostream.CodecG711{Law: audiostream.MuLaw}, ClockRate: 8000, Channels: 1}
	f := &fakeProber{
		tracks:  []rtsp.Track{track},
		session: happySession(),
		result: CaptureResult{
			Frames:  []CapturedFrame{{Data: []byte{0, 1, 2, 3}}},
			Stats:   audiostream.TrackStats{Packets: 1, PayloadBytes: 4},
			Window:  10 * time.Second,
			Elapsed: 10 * time.Second,
			Reason:  EndCompleted,
		},
	}
	opts := Options{URL: testTargetURL, Duration: 10 * time.Second, Report: true, WAVPath: wavPath}

	var out, errOut strings.Builder
	res, err := Run(context.Background(), opts, f, &out, &errOut, testEnv(), fixedClock(5*time.Millisecond))
	if err != nil {
		t.Fatalf("Run() error = %v, want nil (a listen failure must not fail the run)", err)
	}
	if code := mapExit(err, res); code != ExitOK {
		t.Errorf("mapExit = %d, want ExitOK (a listen failure must not change the exit code)", code)
	}

	got := out.String()
	if !strings.Contains(got, "listen: skipped: could not create the WAV output file") {
		t.Errorf("report is missing the create-failure Listen skip line:\n%s", got)
	}
	for _, frag := range []string{parent, "not-a-dir", testWAVName, ".stream-doctor-"} {
		if strings.Contains(got, frag) {
			t.Errorf("report leaks the path fragment %q:\n%s", frag, got)
		}
	}
}

// TestRunListenWriteFailureRedactsPath drives the listen check into
// writeWAV's own failure return: the AAC decoder constructs against the
// valid AudioSpecificConfig, then rejects the garbage access units
// mid-stream, and listen() reports that error through sanitizeWriteErr.
// The skip reason must carry the sanitized "wav write failed:" prefix and
// no filesystem path, and the failed check must leave no file at the
// --wav path.
func TestRunListenWriteFailureRedactsPath(t *testing.T) {
	t.Parallel()
	_, asc := aacListenFrames(t, 48000, 1, 2)
	track := rtsp.Track{
		ID: 0, Media: audiostream.MediaAudio,
		Codec:     audiostream.CodecAAC{AudioSpecificConfig: asc},
		ClockRate: 48000, Channels: 1,
	}
	garbage := []CapturedFrame{
		{Data: bytes.Repeat([]byte{0xde, 0xad}, 200)},
		{Data: bytes.Repeat([]byte{0x55}, 300)},
	}
	f := &fakeProber{
		tracks:  []rtsp.Track{track},
		session: happySession(),
		result: CaptureResult{
			Frames:  garbage,
			Stats:   audiostream.TrackStats{Packets: 2, PayloadBytes: 700},
			Window:  10 * time.Second,
			Elapsed: 10 * time.Second,
			Reason:  EndCompleted,
		},
	}
	tmpDir := t.TempDir()
	wavPath := filepath.Join(tmpDir, testWAVName)
	opts := Options{URL: testTargetURL, Duration: 10 * time.Second, Report: true, WAVPath: wavPath}

	var out, errOut strings.Builder
	res, err := Run(context.Background(), opts, f, &out, &errOut, testEnv(), fixedClock(5*time.Millisecond))
	if err != nil {
		t.Fatalf("Run() error = %v, want nil (a listen failure must not fail the run)", err)
	}
	if code := mapExit(err, res); code != ExitOK {
		t.Errorf("mapExit = %d, want ExitOK (a listen failure must not change the exit code)", code)
	}

	got := out.String()
	if !strings.Contains(got, "listen: skipped: wav write failed:") {
		t.Errorf("report is missing the sanitized write-failure Listen skip line:\n%s", got)
	}
	for _, frag := range []string{tmpDir, testWAVName, ".stream-doctor-"} {
		if strings.Contains(got, frag) {
			t.Errorf("report leaks the path fragment %q:\n%s", frag, got)
		}
	}
	if _, statErr := os.Stat(wavPath); !os.IsNotExist(statErr) {
		t.Errorf("--wav file exists after a failed listen check (stat err = %v); the atomic write must not create it", statErr)
	}
	// The failed check must also clean up its temp file, not just skip the
	// rename: an orphaned .stream-doctor-*.wav in the directory is a leak.
	entries, dirErr := os.ReadDir(tmpDir)
	if dirErr != nil {
		t.Fatalf("ReadDir(%s): %v", tmpDir, dirErr)
	}
	if len(entries) != 0 {
		t.Errorf("temp directory is not empty after a failed listen check: %v", entries)
	}
}

// TestRunReportModeSeparation confirms the --report stream contract: the
// markdown report goes to out, and errOut carries no duplicate plain-text
// walkthrough (that duplication, a fenced report on stdout plus a plain
// walkthrough on stderr, was the reported double-output). errOut is reserved
// for the ephemeral banner and capture meter, neither of which a non-terminal
// writer (this test's buffer) receives.
func TestRunReportModeSeparation(t *testing.T) {
	t.Parallel()
	f := &fakeProber{
		tracks:  []rtsp.Track{aacTrack(), videoTrack()},
		session: happySession(),
		result: CaptureResult{
			Frames:  frames500(),
			Stats:   audiostream.TrackStats{Packets: 500, PayloadBytes: 64000},
			Window:  10 * time.Second,
			Elapsed: 10 * time.Second,
			Reason:  EndCompleted,
		},
	}
	opts := Options{URL: testTargetURL, Duration: 10 * time.Second, Report: true}

	var out, errOut strings.Builder
	res, err := Run(context.Background(), opts, f, &out, &errOut, testEnv(), fixedClock(5*time.Millisecond))
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if code := mapExit(err, res); code != ExitOK {
		t.Errorf("mapExit = %d, want ExitOK", code)
	}

	if !strings.Contains(out.String(), reportFence) {
		t.Errorf("stdout does not contain the fenced report:\n%s", out.String())
	}
	// No duplicate walkthrough on stderr: a non-terminal errOut (this buffer)
	// receives neither the banner nor the meter, so it stays empty. In
	// particular it must not carry a second, plain-text copy of the run.
	if got := errOut.String(); got != "" {
		t.Errorf("stderr is not empty in report mode, want no duplicate output:\n%s", got)
	}
}
