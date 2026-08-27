package sdp_test

import (
	"bytes"
	"strconv"
	"strings"
	"testing"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/rtsp/sdp"
)

func codecsOf(t *testing.T, name string) []sdp.DescribedTrack {
	t.Helper()
	s, err := sdp.Parse(fixtureBytes(t, name))
	if err != nil {
		t.Fatalf("Parse(%s): %v", name, err)
	}
	return s.Codecs()
}

func TestCodecsAAC(t *testing.T) {
	t.Parallel()
	tracks := codecsOf(t, "reolink-aac.sdp")
	if len(tracks) != 2 {
		t.Fatalf("track count = %d, want 2", len(tracks))
	}

	// Video track: unknown codec, still listed, not fatal.
	if _, ok := tracks[0].Codec.(audiostream.CodecUnknown); !ok {
		t.Errorf("track[0].Codec = %T, want CodecUnknown", tracks[0].Codec)
	}
	if tracks[0].Media != audiostream.MediaVideo {
		t.Errorf("track[0].Media = %v, want video", tracks[0].Media)
	}

	// Audio track: AAC with the ASC decoded from config=1408.
	aac, ok := tracks[1].Codec.(audiostream.CodecAAC)
	if !ok {
		t.Fatalf("track[1].Codec = %T, want CodecAAC", tracks[1].Codec)
	}
	if !bytes.Equal(aac.AudioSpecificConfig, []byte{0x14, 0x08}) {
		t.Errorf("ASC = % x, want 14 08", aac.AudioSpecificConfig)
	}
	if tracks[1].ClockRate != 16000 || tracks[1].Channels != 1 {
		t.Errorf("clock/channels = %d/%d, want 16000/1", tracks[1].ClockRate, tracks[1].Channels)
	}
	p := tracks[1].AAC
	if p == nil {
		t.Fatal("AAC params nil")
	}
	if p.SizeLength != 13 || p.IndexLength != 3 || p.IndexDeltaLength != 3 {
		t.Errorf("AU header lengths = %d/%d/%d, want 13/3/3", p.SizeLength, p.IndexLength, p.IndexDeltaLength)
	}
	if !strings.EqualFold(p.Mode, "AAC-hbr") {
		t.Errorf("mode = %q, want AAC-hbr", p.Mode)
	}
	if !bytes.Equal(p.Config, []byte{0x14, 0x08}) {
		t.Errorf("config = % x, want 14 08", p.Config)
	}
}

func TestCodecsOpus(t *testing.T) {
	t.Parallel()
	tracks := codecsOf(t, "mediamtx-opus.sdp")
	if len(tracks) != 1 {
		t.Fatalf("track count = %d", len(tracks))
	}
	if _, ok := tracks[0].Codec.(audiostream.CodecOpus); !ok {
		t.Errorf("Codec = %T, want CodecOpus", tracks[0].Codec)
	}
	if tracks[0].ClockRate != 48000 || tracks[0].Channels != 2 {
		t.Errorf("clock/channels = %d/%d, want 48000/2", tracks[0].ClockRate, tracks[0].Channels)
	}
	if tracks[0].AAC != nil {
		t.Error("AAC params must be nil for Opus")
	}
}

func TestCodecsG711Static(t *testing.T) {
	t.Parallel()
	// PT 0 with no rtpmap must still resolve to PCMU via RFC 3551.
	u := codecsOf(t, "pcmu-static.sdp")
	g, ok := u[0].Codec.(audiostream.CodecG711)
	if !ok || g.Law != audiostream.MuLaw {
		t.Errorf("pcmu-static Codec = %#v, want CodecG711{MuLaw}", u[0].Codec)
	}
	if u[0].ClockRate != 8000 || u[0].Channels != 1 {
		t.Errorf("pcmu clock/channels = %d/%d, want 8000/1", u[0].ClockRate, u[0].Channels)
	}
	// PT 8 with an explicit rtpmap must resolve to PCMA.
	a := codecsOf(t, "pcma-dynamic.sdp")
	if g, ok := a[0].Codec.(audiostream.CodecG711); !ok || g.Law != audiostream.ALaw {
		t.Errorf("pcma Codec = %#v, want CodecG711{ALaw}", a[0].Codec)
	}
}

func TestCodecsUnknown(t *testing.T) {
	t.Parallel()
	tracks := codecsOf(t, "unknown-codec.sdp")
	cu, ok := tracks[0].Codec.(audiostream.CodecUnknown)
	if !ok {
		t.Fatalf("track[0].Codec = %T, want CodecUnknown", tracks[0].Codec)
	}
	if cu.RTPMap != "SPEEX/16000" {
		t.Errorf("CodecUnknown.RTPMap = %q, want SPEEX/16000", cu.RTPMap)
	}
	if tracks[1].Media != audiostream.MediaOther {
		t.Errorf("track[1].Media = %v, want other", tracks[1].Media)
	}
}

func TestCodecsAACFmtpWhitespaceAndMissingEquals(t *testing.T) {
	t.Parallel()
	// A camera sends inconsistent spacing (config= 1408) and a bare flag
	// parameter with no '=' (cpresent). The config must still decode after
	// trimming, and the flag must be skipped without panicking.
	body := []byte("v=0\r\n" +
		"m=audio 0 RTP/AVP 97\r\n" +
		"a=rtpmap:97 MPEG4-GENERIC/16000/1\r\n" +
		"a=fmtp:97 mode=AAC-hbr; sizelength=13 ; config= 1408 ;cpresent\r\n")
	s, err := sdp.Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	tracks := s.Codecs()
	if len(tracks) != 1 {
		t.Fatalf("track count = %d, want 1", len(tracks))
	}
	aac, ok := tracks[0].Codec.(audiostream.CodecAAC)
	if !ok {
		t.Fatalf("Codec = %T, want CodecAAC", tracks[0].Codec)
	}
	if !bytes.Equal(aac.AudioSpecificConfig, []byte{0x14, 0x08}) {
		t.Errorf("ASC = % x, want 14 08 (config trimmed before hex decode)", aac.AudioSpecificConfig)
	}
	if p := tracks[0].AAC; p == nil || p.SizeLength != 13 || !strings.EqualFold(p.Mode, "AAC-hbr") {
		t.Errorf("AAC params = %+v, want sizelength 13 mode AAC-hbr", p)
	}
	// The raw fmtp params (everything after the payload type) are retained
	// verbatim for diagnostics, whitespace and bare flags included.
	if want := "mode=AAC-hbr; sizelength=13 ; config= 1408 ;cpresent"; tracks[0].FMTP != want {
		t.Errorf("FMTP = %q, want %q", tracks[0].FMTP, want)
	}
}

// TestCodecsL16Dynamic covers L16 linear PCM resolved from an a=rtpmap. The
// target ESP32/M5Stack microphones advertise a dynamic payload type with an
// explicit L16/<rate>/<channels>.
func TestCodecsL16Dynamic(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		rtpmap   string
		clock    int
		channels int
	}{
		{"mono 48k", "L16/48000/1", 48000, 1},
		{"stereo 48k", "L16/48000/2", 48000, 2},
		{"mono 24k", "L16/24000/1", 24000, 1},
		// A rtpmap that omits the channel segment defaults to 1 channel, the
		// same normalization the other codecs get.
		{"no channel segment", "L16/48000", 48000, 1},
		// A nonsensical negative channel count (the segment is a plain Atoi) is
		// clamped to 1 rather than surfacing negative on CodecL16.Channels.
		{"negative channels", "L16/48000/-2", 48000, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := []byte("v=0\r\n" +
				"m=audio 0 RTP/AVP 97\r\n" +
				"a=rtpmap:97 " + tc.rtpmap + "\r\n")
			s, err := sdp.Parse(body)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			tracks := s.Codecs()
			if len(tracks) != 1 {
				t.Fatalf("track count = %d, want 1", len(tracks))
			}
			l16, ok := tracks[0].Codec.(audiostream.CodecL16)
			if !ok {
				t.Fatalf("Codec = %T, want CodecL16", tracks[0].Codec)
			}
			if l16.ClockRate != tc.clock || l16.Channels != tc.channels {
				t.Errorf("CodecL16 = %+v, want {%d %d}", l16, tc.clock, tc.channels)
			}
			if tracks[0].ClockRate != tc.clock || tracks[0].Channels != tc.channels {
				t.Errorf("track clock/channels = %d/%d, want %d/%d",
					tracks[0].ClockRate, tracks[0].Channels, tc.clock, tc.channels)
			}
		})
	}
}

// TestCodecsG726Dynamic covers the four G.726 rtpmap encoding names resolving
// to CodecG726 with the matching bit rate, and the two forms that must NOT: an
// AAL2-G726 name (a different, unsupported bit order) and an unknown rate both
// fall back to CodecUnknown.
func TestCodecsG726Dynamic(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		rtpmap  string
		want    audiostream.G726BitRate
		unknown bool
	}{
		{"16k", "G726-16/8000", audiostream.G726Rate16, false},
		{"24k", "G726-24/8000", audiostream.G726Rate24, false},
		{"32k", "G726-32/8000", audiostream.G726Rate32, false},
		{"40k", "G726-40/8000", audiostream.G726Rate40, false},
		// Lower-case encoding names resolve too (the switch upper-cases).
		{"lowercase", "g726-32/8000", audiostream.G726Rate32, false},
		// AAL2-G726 packs its codewords in the opposite bit order, which this
		// package does not decode, so it must not masquerade as a CodecG726.
		{"aal2", "AAL2-G726-32/8000", 0, true},
		// An out-of-range rate is not a G.726 this package knows.
		{"bad rate", "G726-99/8000", 0, true},
		// G.726 is single-channel; a multi-channel advertisement cannot be
		// decoded by the one-state decoder, so it stays CodecUnknown.
		{"stereo", "G726-32/8000/2", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := []byte("v=0\r\n" +
				"m=audio 0 RTP/AVP 97\r\n" +
				"a=rtpmap:97 " + tc.rtpmap + "\r\n")
			s, err := sdp.Parse(body)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			tracks := s.Codecs()
			if len(tracks) != 1 {
				t.Fatalf("track count = %d, want 1", len(tracks))
			}
			if tc.unknown {
				if _, ok := tracks[0].Codec.(audiostream.CodecUnknown); !ok {
					t.Fatalf("Codec = %T, want CodecUnknown", tracks[0].Codec)
				}
				return
			}
			g, ok := tracks[0].Codec.(audiostream.CodecG726)
			if !ok {
				t.Fatalf("Codec = %T, want CodecG726", tracks[0].Codec)
			}
			if g.BitRate != tc.want {
				t.Errorf("BitRate = %v, want %v", g.BitRate, tc.want)
			}
			if g.ClockRate != 8000 || g.Channels != 1 {
				t.Errorf("clock/channels = %d/%d, want 8000/1", g.ClockRate, g.Channels)
			}
		})
	}
}

// TestCodecsG726Static covers RFC 3551 static payload type 2 (G.721, identical
// to G.726 at 32 kbps) resolving with no a=rtpmap present.
func TestCodecsG726Static(t *testing.T) {
	t.Parallel()
	body := []byte("v=0\r\n" +
		"m=audio 0 RTP/AVP 2\r\n")
	s, err := sdp.Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	tracks := s.Codecs()
	if len(tracks) != 1 {
		t.Fatalf("track count = %d, want 1", len(tracks))
	}
	g, ok := tracks[0].Codec.(audiostream.CodecG726)
	if !ok {
		t.Fatalf("PT 2 Codec = %T, want CodecG726", tracks[0].Codec)
	}
	if g.BitRate != audiostream.G726Rate32 || g.ClockRate != 8000 || g.Channels != 1 {
		t.Errorf("CodecG726 = %+v, want {32kbps 8000 1}", g)
	}
}

// TestCodecsL16Static covers the RFC 3551 static payload types 10 (L16 stereo
// 44100) and 11 (L16 mono 44100) resolved with no a=rtpmap present.
func TestCodecsL16Static(t *testing.T) {
	t.Parallel()
	cases := []struct {
		pt       int
		clock    int
		channels int
	}{
		{10, 44100, 2},
		{11, 44100, 1},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.pt), func(t *testing.T) {
			t.Parallel()
			body := []byte("v=0\r\n" +
				"m=audio 0 RTP/AVP " + strconv.Itoa(tc.pt) + "\r\n")
			s, err := sdp.Parse(body)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			tracks := s.Codecs()
			if len(tracks) != 1 {
				t.Fatalf("track count = %d, want 1", len(tracks))
			}
			l16, ok := tracks[0].Codec.(audiostream.CodecL16)
			if !ok {
				t.Fatalf("PT %d Codec = %T, want CodecL16", tc.pt, tracks[0].Codec)
			}
			if l16.ClockRate != tc.clock || l16.Channels != tc.channels {
				t.Errorf("CodecL16 = %+v, want {%d %d}", l16, tc.clock, tc.channels)
			}
		})
	}
}
