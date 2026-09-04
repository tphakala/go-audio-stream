package sdp_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/rtsp/sdp"
)

// fixtureBytes reads a committed SDP fixture. Tests run with the working
// directory set to the package directory (rtsp/sdp), two levels below the
// repo root, so the fixtures resolve at ../../testdata/fixtures/sdp.
func fixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	p := filepath.Join("..", "..", "testdata", "fixtures", "sdp", name)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func TestParseReolink(t *testing.T) {
	t.Parallel()
	s, err := sdp.Parse(fixtureBytes(t, "reolink-aac.sdp"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.Control != "rtsp://192.0.2.10:554/h264Preview_01_main/" {
		t.Errorf("session control = %q", s.Control)
	}
	if s.Name != "Media Presentation" {
		t.Errorf("session name = %q, want %q", s.Name, "Media Presentation")
	}
	if s.Tool != "" {
		t.Errorf("session tool = %q, want empty (fixture has no a=tool)", s.Tool)
	}
	if len(s.Media) != 2 {
		t.Fatalf("media count = %d, want 2", len(s.Media))
	}

	v := s.Media[0]
	if v.Kind != audiostream.MediaVideo {
		t.Errorf("media[0].Kind = %v, want video", v.Kind)
	}
	if v.Proto != "RTP/AVP" {
		t.Errorf("media[0].Proto = %q", v.Proto)
	}
	if len(v.Formats) != 1 || v.Formats[0] != 96 {
		t.Errorf("media[0].Formats = %v, want [96]", v.Formats)
	}
	if v.Control != "trackID=0" {
		t.Errorf("media[0].Control = %q", v.Control)
	}
	if rm := v.RTPMaps[96]; rm.EncodingName != "H264" || rm.ClockRate != 90000 {
		t.Errorf("media[0] rtpmap = %+v", rm)
	}

	a := s.Media[1]
	if a.Kind != audiostream.MediaAudio {
		t.Errorf("media[1].Kind = %v, want audio", a.Kind)
	}
	if len(a.Formats) != 1 || a.Formats[0] != 97 {
		t.Errorf("media[1].Formats = %v, want [97]", a.Formats)
	}
	if a.Control != "trackID=1" {
		t.Errorf("media[1].Control = %q", a.Control)
	}
	rm := a.RTPMaps[97]
	if rm.EncodingName != "MPEG4-GENERIC" || rm.ClockRate != 16000 || rm.Channels != 1 {
		t.Errorf("media[1] rtpmap = %+v", rm)
	}
	if fm := a.FMTPs[97]; !strings.Contains(fm, "config=1408") {
		t.Errorf("media[1] fmtp = %q", fm)
	}
}

// TestParseSessionIdentity covers the session name (s=) and tool (a=tool:)
// capture with an inline body modeled on a real Reolink camera, whose tool line
// identifies the Baichuan streaming stack. A per-media a=tool must not overwrite
// the session-level one.
func TestParseSessionIdentity(t *testing.T) {
	t.Parallel()
	body := "v=0\r\n" +
		"o=- 1787316399865599 1 IN IP4 192.0.2.10\r\n" +
		"s=Session streamed by \"preview\"\r\n" +
		"t=0 0\r\n" +
		"a=tool:BC Streaming Media v202210012022.10.01\r\n" +
		"a=control:*\r\n" +
		"m=audio 0 RTP/AVP 97\r\n" +
		"a=tool:should-be-ignored\r\n" +
		"a=rtpmap:97 MPEG4-GENERIC/16000/1\r\n"
	s, err := sdp.Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.Name != `Session streamed by "preview"` {
		t.Errorf("session name = %q", s.Name)
	}
	if s.Tool != "BC Streaming Media v202210012022.10.01" {
		t.Errorf("session tool = %q, want the session-level a=tool", s.Tool)
	}
}

func TestParsePCMUStaticNoRTPMap(t *testing.T) {
	t.Parallel()
	s, err := sdp.Parse(fixtureBytes(t, "pcmu-static.sdp"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(s.Media) != 1 {
		t.Fatalf("media count = %d, want 1", len(s.Media))
	}
	m := s.Media[0]
	if len(m.Formats) != 1 || m.Formats[0] != 0 {
		t.Errorf("Formats = %v, want [0]", m.Formats)
	}
	if len(m.RTPMaps) != 0 {
		t.Errorf("RTPMaps = %v, want empty (static PT, no a=rtpmap)", m.RTPMaps)
	}
}

func TestParseMediaOther(t *testing.T) {
	t.Parallel()
	s, err := sdp.Parse(fixtureBytes(t, "unknown-codec.sdp"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(s.Media) != 2 {
		t.Fatalf("media count = %d, want 2", len(s.Media))
	}
	if s.Media[0].Kind != audiostream.MediaAudio {
		t.Errorf("media[0].Kind = %v, want audio", s.Media[0].Kind)
	}
	if s.Media[1].Kind != audiostream.MediaOther {
		t.Errorf("media[1].Kind = %v, want other", s.Media[1].Kind)
	}
}

func TestParseCaps(t *testing.T) {
	t.Parallel()

	big := make([]byte, sdp.MaxInputSize+1)
	if _, err := sdp.Parse(big); !errors.Is(err, sdp.ErrInputTooLarge) {
		t.Errorf("oversized body err = %v, want ErrInputTooLarge", err)
	}

	var sb strings.Builder
	sb.WriteString("v=0\r\n")
	for range sdp.MaxMediaSections + 1 {
		sb.WriteString("m=audio 0 RTP/AVP 0\r\n")
	}
	if _, err := sdp.Parse([]byte(sb.String())); !errors.Is(err, sdp.ErrTooManyMedia) {
		t.Errorf("too many media err = %v, want ErrTooManyMedia", err)
	}

	var ab strings.Builder
	ab.WriteString("v=0\r\nm=audio 0 RTP/AVP 0\r\n")
	for range sdp.MaxAttributesPerSection + 1 {
		ab.WriteString("a=recvonly\r\n")
	}
	if _, err := sdp.Parse([]byte(ab.String())); !errors.Is(err, sdp.ErrTooManyAttributes) {
		t.Errorf("too many attrs err = %v, want ErrTooManyAttributes", err)
	}
}

func TestParseLenient(t *testing.T) {
	t.Parallel()
	// Empty input, unknown line types, a line with no '=', and every
	// split-hazard site (valueless attribute, rtpmap/fmtp with no space,
	// control with no value) must all parse without error and without
	// panicking (total, lenient). These are the concrete sites the
	// binding split-guard rule protects.
	for _, in := range [][]byte{
		nil,
		[]byte(""),
		[]byte("garbage without equals\n"),
		[]byte("v=0\nm=audio 0 RTP/AVP\n"), // m= with no formats
		[]byte("v=0\nm=audio 0 RTP/AVP 0\na=recvonly\n"),        // valueless attribute, no ':'
		[]byte("v=0\nm=audio 0 RTP/AVP 97\na=rtpmap:97\n"),      // rtpmap, no space after pt
		[]byte("v=0\nm=audio 0 RTP/AVP 97\na=fmtp:97\n"),        // fmtp, no space after pt
		[]byte("v=0\nm=audio 0 RTP/AVP 97\na=rtpmap:97 OPUS\n"), // rtpmap, no '/' clock
		[]byte("a=control\n"),                                   // control attribute, no ':' value
	} {
		s, err := sdp.Parse(in)
		if err != nil {
			t.Errorf("Parse(%q) = %v, want nil", in, err)
			continue
		}
		_ = s.Codecs() // Codecs must also stay total over these inputs
	}
}

func TestParseSkipsOutOfRangePayloadTypes(t *testing.T) {
	t.Parallel()
	// An RTP payload type is 7 bits, so an m= line can only ever declare
	// 0 to 127. Attributes naming anything outside that range are skipped
	// rather than stored, so a caller ranging over RTPMaps or FMTPs never
	// sees a key that Formats could not contain.
	body := []byte("v=0\r\n" +
		"m=audio 0 RTP/AVP 97 128 -1 99999\r\n" +
		"a=rtpmap:97 opus/48000/2\r\n" +
		"a=rtpmap:128 BOGUS/8000\r\n" +
		"a=rtpmap:-1 BOGUS/8000\r\n" +
		"a=rtpmap:99999 BOGUS/8000\r\n" +
		"a=fmtp:128 mode=bogus\r\n" +
		"a=fmtp:-1 mode=bogus\r\n")

	s, err := sdp.Parse(body)
	if err != nil {
		t.Fatalf("Parse() = %v, want nil", err)
	}
	if len(s.Media) != 1 {
		t.Fatalf("len(Media) = %d, want 1", len(s.Media))
	}
	m := s.Media[0]

	if len(m.Formats) != 1 || m.Formats[0] != 97 {
		t.Errorf("Formats = %v, want [97] (the m= line also lists 128, -1 and 99999)", m.Formats)
	}
	if len(m.RTPMaps) != 1 {
		t.Errorf("len(RTPMaps) = %d, want 1 (only payload type 97): %v", len(m.RTPMaps), m.RTPMaps)
	}
	if _, ok := m.RTPMaps[97]; !ok {
		t.Error("RTPMaps is missing the valid payload type 97")
	}
	if len(m.FMTPs) != 0 {
		t.Errorf("len(FMTPs) = %d, want 0 (both fmtp lines are out of range): %v", len(m.FMTPs), m.FMTPs)
	}
	for pt := range m.RTPMaps {
		if pt < 0 || pt > 127 {
			t.Errorf("RTPMaps holds out-of-range payload type %d", pt)
		}
	}
	for pt := range m.FMTPs {
		if pt < 0 || pt > 127 {
			t.Errorf("FMTPs holds out-of-range payload type %d", pt)
		}
	}
}
