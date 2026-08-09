package doctor

import (
	"context"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/rtsp"
)

// TestRunInBandLATMLearnedASC drives Run end to end over a scripted Prober for
// an in-band MP4A-LATM track (cpresent=1): Describe resolves the track with no
// AudioSpecificConfig, and the config is learned during capture and surfaced
// through CaptureResult.LearnedCodec. With the learned codec present, the
// tracks block must render the asc hex and the listen check must decode the
// real AAC access units to a WAV; with it absent (nil), or with a learned codec
// of the wrong type that the overlay guard rejects, both must degrade exactly as
// before: no asc line and a listen skip naming the in-band case. The cases share
// every input except LearnedCodec, so the test pins that field as the sole cause
// of the difference.
func TestRunInBandLATMLearnedASC(t *testing.T) {
	t.Parallel()
	const sampleRate = 48000
	const channels = 1
	const numFrames = 5

	frames, asc := aacListenFrames(t, sampleRate, channels, numFrames)
	ascLine := "asc: " + hex.EncodeToString(asc)

	// The Describe-time in-band track carries no ASC yet; MuxConfigPresent true
	// is how the doctor distinguishes an in-band track still awaiting its config
	// from an out-of-band track whose StreamMuxConfig was missing (see
	// writeWAVLATM).
	describeTrack := rtsp.Track{
		ID: 0, Media: audiostream.MediaAudio,
		Codec:     audiostream.CodecMP4ALATM{MuxConfigPresent: true},
		ClockRate: sampleRate, Channels: channels,
	}

	var totalBytes int
	for i := range frames {
		totalBytes += len(frames[i].Data)
	}
	stats := audiostream.TrackStats{
		Packets:      uint64(len(frames)), //nolint:gosec // test data, bounded by numFrames.
		PayloadBytes: uint64(totalBytes),  //nolint:gosec // test data, bounded above.
		WireBytes:    uint64(totalBytes),  //nolint:gosec // test data, bounded above.
		LastFrameAt:  time.Unix(110, 0),
	}

	for _, tc := range []struct {
		name    string
		learned audiostream.Codec
		// wantASC is whether applyLearnedCodec should merge the learned codec and
		// unlock the asc line plus the decode. It is false both when nothing was
		// learned and when the learned codec is the wrong type for the track, so
		// the guard rejects it.
		wantASC bool
	}{
		{
			name:    "learned in-band asc decodes and renders",
			learned: audiostream.CodecMP4ALATM{AudioSpecificConfig: asc, MuxConfigPresent: true},
			wantASC: true,
		},
		{
			name:    "no config learned degrades to skip",
			learned: nil,
			wantASC: false,
		},
		{
			// A learned codec of a type other than the track's MP4A-LATM (which
			// the library never emits today, but the overlay guards against) must
			// be ignored, not merged onto the LATM track: same degradation as
			// nothing learned. This drives applyLearnedCodec's type-mismatch guard.
			name:    "learned codec of a mismatched type is ignored",
			learned: audiostream.CodecAAC{AudioSpecificConfig: asc},
			wantASC: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wavPath := filepath.Join(t.TempDir(), "capture.wav")
			f := &fakeProber{
				tracks:  []rtsp.Track{describeTrack},
				session: happySession(),
				result: CaptureResult{
					Frames:       frames,
					Stats:        stats,
					CapturedAt:   time.Unix(110, 400_000_000),
					Window:       10 * time.Second,
					Elapsed:      10 * time.Second,
					Reason:       EndCompleted,
					LearnedCodec: tc.learned,
				},
			}
			opts := Options{URL: testTargetURL, Duration: 10 * time.Second, WAVPath: wavPath}

			var out strings.Builder
			res, err := Run(context.Background(), opts, f, &out, io.Discard, testEnv(), fixedClock(5*time.Millisecond))
			if err != nil {
				t.Fatalf("Run() error = %v, want nil", err)
			}
			// The codec type is unchanged either way, so an in-band LATM track is
			// reported as a supported codec with captured frames regardless of
			// whether the ASC was learned; learning the ASC only unlocks the asc
			// line and the decode.
			if !res.CodecSupported {
				t.Error("CodecSupported = false, want true for an MP4A-LATM track")
			}
			if res.FramesCaptured != len(frames) {
				t.Errorf("FramesCaptured = %d, want %d", res.FramesCaptured, len(frames))
			}
			if code := mapExit(err, res); code != ExitOK {
				t.Errorf("mapExit = %d, want ExitOK", code)
			}

			got := out.String()
			_, statErr := os.Stat(wavPath)
			if tc.wantASC {
				if !strings.Contains(got, ascLine) {
					t.Errorf("walkthrough missing learned ASC %q:\n%s", ascLine, got)
				}
				if !strings.Contains(got, "listen: wrote") {
					t.Errorf("walkthrough missing successful listen line:\n%s", got)
				}
				if statErr != nil {
					t.Errorf("WAV file was not written on a learned in-band decode: %v", statErr)
				}
				return
			}
			// Not applied (nothing learned, or a learned codec the guard rejects):
			// identical to the pre-#87 behavior.
			if strings.Contains(got, "asc:") {
				t.Errorf("walkthrough rendered an asc line with no config applied:\n%s", got)
			}
			if !strings.Contains(got, "listen: skipped: "+latmInBandUnlearnedReason) {
				t.Errorf("walkthrough missing the in-band skip reason:\n%s", got)
			}
			if statErr == nil {
				t.Error("a skipped listen wrote a WAV file, want none")
			}
		})
	}
}
