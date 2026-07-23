package doctor

import (
	"strings"
	"testing"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/rtsp"
)

func TestCodecName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		codec audiostream.Codec
		want  string
	}{
		{"aac", audiostream.CodecAAC{}, "AAC"},
		{"opus", audiostream.CodecOpus{}, "Opus"},
		{"g711 mu-law", audiostream.CodecG711{Law: audiostream.MuLaw}, "PCMU (G.711 mu-law)"},
		{"g711 a-law", audiostream.CodecG711{Law: audiostream.ALaw}, "PCMA (G.711 A-law)"},
		{"unknown with rtpmap", audiostream.CodecUnknown{RTPMap: testH264}, testH264},
		{"unknown empty", audiostream.CodecUnknown{}, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := codecName(tc.codec); got != tc.want {
				t.Errorf("codecName(%T) = %q, want %q", tc.codec, got, tc.want)
			}
		})
	}
}

func TestDecodable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		track rtsp.Track
		want  bool
	}{
		{"aac audio", rtsp.Track{Media: audiostream.MediaAudio, Codec: audiostream.CodecAAC{}}, true},
		{"opus audio", rtsp.Track{Media: audiostream.MediaAudio, Codec: audiostream.CodecOpus{}}, true},
		{"g711 audio", rtsp.Track{Media: audiostream.MediaAudio, Codec: audiostream.CodecG711{}}, true},
		{"unknown audio", rtsp.Track{Media: audiostream.MediaAudio, Codec: audiostream.CodecUnknown{RTPMap: testL16RTPMap}}, false},
		{"video track", rtsp.Track{Media: audiostream.MediaVideo, Codec: audiostream.CodecUnknown{RTPMap: testH264}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := decodable(tc.track); got != tc.want {
				t.Errorf("decodable(%+v) = %v, want %v", tc.track, got, tc.want)
			}
		})
	}
}

// TestRenderWalkthroughColumns covers the two column-rendering edge cases: a
// track with zero channels renders "-", and a failed early step omits every
// later step.
func TestRenderWalkthroughColumns(t *testing.T) {
	t.Parallel()
	env := Env{OS: "linux", Arch: "amd64", Version: "0.1.0"}

	t.Run("zero channels render dash", func(t *testing.T) {
		t.Parallel()
		r := Report{
			RedactedURL: "rtsp://REDACTED@cam/stream",
			HaveAudio:   true,
			Tracks: []rtsp.Track{
				{ID: 2, Media: audiostream.MediaAudio, Codec: audiostream.CodecOpus{}, ClockRate: 48000, Channels: 0},
			},
		}
		var b strings.Builder
		renderWalkthrough(&b, r, env)
		got := b.String()
		if !strings.Contains(got, "48000    -  yes") {
			t.Errorf("zero-channel row missing dash cell:\n%s", got)
		}
	})

	t.Run("failed step omits later steps", func(t *testing.T) {
		t.Parallel()
		r := Report{
			RedactedURL: "rtsp://REDACTED@cam/stream",
			Steps: []HandshakeStep{
				{Name: "DIAL", OK: false, Detail: "connection refused"},
			},
		}
		var b strings.Builder
		renderWalkthrough(&b, r, env)
		got := b.String()
		if !strings.Contains(got, "DIAL") || !strings.Contains(got, "FAIL") {
			t.Errorf("expected DIAL FAIL line, got:\n%s", got)
		}
		for _, later := range []string{"DESCRIBE", "SETUP", "PLAY", "tracks", "capture"} {
			if strings.Contains(got, later) {
				t.Errorf("expected %q to be omitted after a failed DIAL, got:\n%s", later, got)
			}
		}
	})
}
