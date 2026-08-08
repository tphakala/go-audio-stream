package sdp_test

import (
	"bytes"
	"testing"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/rtsp/sdp"
)

// latmV1 is the out-of-band StreamMuxConfig test vector shared with
// depacket/latm (vector V1): AAC-LC, 44.1 kHz, stereo, one subframe,
// frameLengthType 0. It is what "config=400024203fc0" decodes to.
var latmV1 = []byte{0x40, 0x00, 0x24, 0x20, 0x3F, 0xC0}

// TestCodecsLATMOutOfBand covers an MP4A-LATM fmtp carrying cpresent=0 and a
// config= hex StreamMuxConfig: the codec resolves to CodecMP4ALATM with
// MuxConfigPresent false and the decoded StreamMuxConfig bytes, and
// DescribedTrack.LATM carries the parsed fmtp parameters. sdp does not parse
// the StreamMuxConfig itself (that would require importing depacket/latm), so
// AudioSpecificConfig stays nil here; its extraction is asserted at the rtsp
// layer in describe_latm_test.go.
func TestCodecsLATMOutOfBand(t *testing.T) {
	t.Parallel()
	body := []byte("v=0\r\n" +
		"m=audio 0 RTP/AVP 96\r\n" +
		"a=rtpmap:96 MP4A-LATM/44100/2\r\n" +
		"a=fmtp:96 cpresent=0;object=2;config=400024203fc0\r\n")
	s, err := sdp.Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	tracks := s.Codecs()
	if len(tracks) != 1 {
		t.Fatalf("track count = %d, want 1", len(tracks))
	}

	latm, ok := tracks[0].Codec.(audiostream.CodecMP4ALATM)
	if !ok {
		t.Fatalf("Codec = %T, want CodecMP4ALATM", tracks[0].Codec)
	}
	if latm.MuxConfigPresent {
		t.Error("MuxConfigPresent = true, want false for cpresent=0")
	}
	if !bytes.Equal(latm.StreamMuxConfig, latmV1) {
		t.Errorf("StreamMuxConfig = % x, want % x", latm.StreamMuxConfig, latmV1)
	}
	if latm.AudioSpecificConfig != nil {
		t.Errorf("AudioSpecificConfig = % x, want nil (sdp does not extract it)", latm.AudioSpecificConfig)
	}

	p := tracks[0].LATM
	if p == nil {
		t.Fatal("DescribedTrack.LATM is nil, want the parsed fmtp parameters")
	}
	if p.Cpresent {
		t.Error("LATMParams.Cpresent = true, want false for cpresent=0")
	}
	if p.Object != 2 {
		t.Errorf("LATMParams.Object = %d, want 2", p.Object)
	}
	if !bytes.Equal(p.Config, latmV1) {
		t.Errorf("LATMParams.Config = % x, want % x", p.Config, latmV1)
	}

	if tracks[0].ClockRate != 44100 || tracks[0].Channels != 2 {
		t.Errorf("clock/channels = %d/%d, want 44100/2", tracks[0].ClockRate, tracks[0].Channels)
	}
	if tracks[0].AAC != nil {
		t.Error("DescribedTrack.AAC must be nil for a LATM track")
	}
}

// TestCodecsLATMInBandDefaultCpresent covers an MP4A-LATM fmtp with no
// cpresent parameter at all: it must default to present (cpresent=1 is the
// RFC 3016 default), and a fmtp with no config= must leave both
// StreamMuxConfig and LATMParams.Config nil rather than an empty non-nil
// slice.
func TestCodecsLATMInBandDefaultCpresent(t *testing.T) {
	t.Parallel()
	body := []byte("v=0\r\n" +
		"m=audio 0 RTP/AVP 96\r\n" +
		"a=rtpmap:96 MP4A-LATM/44100/2\r\n" +
		"a=fmtp:96 object=2\r\n")
	s, err := sdp.Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	tracks := s.Codecs()
	if len(tracks) != 1 {
		t.Fatalf("track count = %d, want 1", len(tracks))
	}

	latm, ok := tracks[0].Codec.(audiostream.CodecMP4ALATM)
	if !ok {
		t.Fatalf("Codec = %T, want CodecMP4ALATM", tracks[0].Codec)
	}
	if !latm.MuxConfigPresent {
		t.Error("MuxConfigPresent = false, want true when cpresent is absent (default present)")
	}
	if latm.StreamMuxConfig != nil {
		t.Errorf("StreamMuxConfig = % x, want nil when fmtp carries no config=", latm.StreamMuxConfig)
	}

	p := tracks[0].LATM
	if p == nil {
		t.Fatal("DescribedTrack.LATM is nil, want the parsed fmtp parameters")
	}
	if !p.Cpresent {
		t.Error("LATMParams.Cpresent = false, want true (default present)")
	}
	if p.Object != 2 {
		t.Errorf("LATMParams.Object = %d, want 2", p.Object)
	}
	if p.Config != nil {
		t.Errorf("LATMParams.Config = % x, want nil", p.Config)
	}
}

// TestCodecsLATMInvalidHexConfig covers a config= parameter that is present but
// not valid hex ("zz" is not a hex byte): parseLATMFmtp promises a non-hex
// config yields a nil Config rather than a partial or empty non-nil slice, so
// both LATMParams.Config and the codec's StreamMuxConfig must stay nil.
func TestCodecsLATMInvalidHexConfig(t *testing.T) {
	t.Parallel()
	body := []byte("v=0\r\n" +
		"m=audio 0 RTP/AVP 96\r\n" +
		"a=rtpmap:96 MP4A-LATM/44100/2\r\n" +
		"a=fmtp:96 cpresent=0;object=2;config=zz00zz\r\n")
	s, err := sdp.Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	tracks := s.Codecs()
	if len(tracks) != 1 {
		t.Fatalf("track count = %d, want 1", len(tracks))
	}

	p := tracks[0].LATM
	if p == nil {
		t.Fatal("DescribedTrack.LATM is nil, want the parsed fmtp parameters")
	}
	if p.Config != nil {
		t.Errorf("LATMParams.Config = % x, want nil for a non-hex config= value", p.Config)
	}

	latm, ok := tracks[0].Codec.(audiostream.CodecMP4ALATM)
	if !ok {
		t.Fatalf("Codec = %T, want CodecMP4ALATM", tracks[0].Codec)
	}
	if latm.StreamMuxConfig != nil {
		t.Errorf("StreamMuxConfig = % x, want nil for a non-hex config= value", latm.StreamMuxConfig)
	}
}
