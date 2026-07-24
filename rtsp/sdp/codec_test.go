package sdp_test

import (
	"bytes"
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
