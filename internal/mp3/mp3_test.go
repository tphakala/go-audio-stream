package mp3

import (
	"errors"
	"testing"
)

// The header literals below are hand-decoded from the bit layout so the test
// validates Parse against independently derived values, not against a shared
// helper. Each comment names the fields the 32-bit value encodes.
func TestParseValid(t *testing.T) {
	cases := []struct {
		name                                     string
		h                                        uint32
		wantRate, wantChannels, wantSPF, wantLen int
	}{
		// MPEG-1 Layer III, 128 kbps, 44100 Hz, stereo, no padding.
		{"mpeg1-l3-128k-44100-stereo", 0xFFFB9000, 44100, 2, 1152, 417},
		// Same, padding bit set: one extra byte.
		{"mpeg1-l3-128k-44100-padded", 0xFFFB9200, 44100, 2, 1152, 418},
		// MPEG-2 Layer III, 64 kbps, 22050 Hz, mono.
		{"mpeg2-l3-64k-22050-mono", 0xFFF380C0, 22050, 1, 576, 208},
		// MPEG-2.5 Layer III, 8 kbps, 11025 Hz, mono.
		{"mpeg2.5-l3-8k-11025-mono", 0xFFE310C0, 11025, 1, 576, 52},
		// MPEG-1 Layer I, 256 kbps, 44100 Hz, stereo.
		{"mpeg1-l1-256k-44100-stereo", 0xFFFF8000, 44100, 2, 384, 276},
		// MPEG-1 Layer II, 128 kbps, 44100 Hz, stereo.
		{"mpeg1-l2-128k-44100-stereo", 0xFFFD8000, 44100, 2, 1152, 417},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, err := Parse(tc.h)
			if err != nil {
				t.Fatalf("Parse(%#08x) error: %v", tc.h, err)
			}
			if h.SampleRate != tc.wantRate {
				t.Errorf("SampleRate = %d, want %d", h.SampleRate, tc.wantRate)
			}
			if h.Channels != tc.wantChannels {
				t.Errorf("Channels = %d, want %d", h.Channels, tc.wantChannels)
			}
			if h.SamplesPerFrame != tc.wantSPF {
				t.Errorf("SamplesPerFrame = %d, want %d", h.SamplesPerFrame, tc.wantSPF)
			}
			if h.FrameLen != tc.wantLen {
				t.Errorf("FrameLen = %d, want %d", h.FrameLen, tc.wantLen)
			}
		})
	}
}

func TestParseInvalid(t *testing.T) {
	cases := []struct {
		name string
		h    uint32
	}{
		{"bad-sync-zero", 0x00000000},
		{"bad-sync-partial", 0xFFD00000},
		{"reserved-version", 0xFFEB9000},
		{"reserved-layer", 0xFFF99000},
		{"free-format-bitrate", 0xFFFB0000},
		{"bad-bitrate-index", 0xFFFBF000},
		{"reserved-samplerate", 0xFFFB9C00},
		{"reserved-emphasis", 0xFFFB9002},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(tc.h); !errors.Is(err, ErrInvalidHeader) {
				t.Fatalf("Parse(%#08x) = %v, want ErrInvalidHeader", tc.h, err)
			}
		})
	}
}

// FuzzParse asserts every accepted header carries self-consistent geometry, so
// no malformed value slips through with a nonsensical frame length or rate that
// would break the framer that trusts Parse.
func FuzzParse(f *testing.F) {
	for _, s := range []uint32{0xFFFB9000, 0xFFF380C0, 0xFFE310C0, 0xFFFF8000, 0, 0xFFFFFFFF} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, h uint32) {
		hdr, err := Parse(h)
		if err != nil {
			return
		}
		if hdr.FrameLen < HeaderLen {
			t.Fatalf("accepted %#08x with FrameLen %d < %d", h, hdr.FrameLen, HeaderLen)
		}
		if hdr.SampleRate <= 0 {
			t.Fatalf("accepted %#08x with SampleRate %d", h, hdr.SampleRate)
		}
		if hdr.Channels != 1 && hdr.Channels != 2 {
			t.Fatalf("accepted %#08x with Channels %d", h, hdr.Channels)
		}
		if hdr.SamplesPerFrame <= 0 {
			t.Fatalf("accepted %#08x with SamplesPerFrame %d", h, hdr.SamplesPerFrame)
		}
	})
}
