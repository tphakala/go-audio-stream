package sdp_test

import (
	"bytes"
	"encoding/base64"
	"testing"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/rtsp/sdp"
)

// flacStreamInfo is a stand-in 34-byte FLAC STREAMINFO block. The parser does not
// interpret it, so the exact bytes only need to round-trip through base64.
var flacStreamInfo = func() []byte {
	b := make([]byte, 34)
	for i := range b {
		b[i] = byte(i + 1)
	}
	return b
}()

// A FLAC rtpmap with a base64 streaminfo fmtp resolves to CodecFLAC carrying the
// decoded STREAMINFO, and the rtpmap clock and channels flow through.
func TestCodecsFLAC(t *testing.T) {
	t.Parallel()
	b64 := base64.StdEncoding.EncodeToString(flacStreamInfo)
	body := []byte("v=0\r\n" +
		"m=audio 0 RTP/AVP 96\r\n" +
		"a=rtpmap:96 FLAC/48000/2\r\n" +
		"a=fmtp:96 streaminfo=" + b64 + "\r\n")
	tracks := parseCodecs(t, body)
	f, ok := tracks[0].Codec.(audiostream.CodecFLAC)
	if !ok {
		t.Fatalf("Codec = %T, want CodecFLAC", tracks[0].Codec)
	}
	if !bytes.Equal(f.StreamInfo, flacStreamInfo) {
		t.Errorf("StreamInfo = % x, want % x", f.StreamInfo, flacStreamInfo)
	}
	if tracks[0].ClockRate != 48000 || tracks[0].Channels != 2 {
		t.Errorf("clock/channels = %d/%d, want 48000/2", tracks[0].ClockRate, tracks[0].Channels)
	}
	if audiostream.PayloadKindFor(tracks[0].Codec) != audiostream.KindCompressed {
		t.Error("FLAC must map to KindCompressed")
	}
}

// A FLAC rtpmap with no fmtp is still FLAC, with a nil STREAMINFO: a decoder can
// recover geometry from the frame headers, so the track must not be demoted.
func TestCodecsFLACNoFmtp(t *testing.T) {
	t.Parallel()
	body := []byte("v=0\r\n" +
		"m=audio 0 RTP/AVP 96\r\n" +
		"a=rtpmap:96 FLAC/44100/1\r\n")
	tracks := parseCodecs(t, body)
	f, ok := tracks[0].Codec.(audiostream.CodecFLAC)
	if !ok {
		t.Fatalf("Codec = %T, want CodecFLAC", tracks[0].Codec)
	}
	if f.StreamInfo != nil {
		t.Errorf("StreamInfo = % x, want nil for a FLAC track with no fmtp", f.StreamInfo)
	}
	if tracks[0].ClockRate != 44100 || tracks[0].Channels != 1 {
		t.Errorf("clock/channels = %d/%d, want 44100/1", tracks[0].ClockRate, tracks[0].Channels)
	}
}

// A base64-valid streaminfo that decodes to the wrong length is malformed
// metadata: StreamInfo stays nil (a decoder recovers from the frame headers)
// while the track stays FLAC. A STREAMINFO block is exactly 34 bytes.
func TestCodecsFLACWrongLengthStreaminfo(t *testing.T) {
	t.Parallel()
	short := base64.StdEncoding.EncodeToString(make([]byte, 10)) // decodes cleanly, but not 34 bytes
	body := []byte("v=0\r\n" +
		"m=audio 0 RTP/AVP 96\r\n" +
		"a=rtpmap:96 FLAC/48000/2\r\n" +
		"a=fmtp:96 streaminfo=" + short + "\r\n")
	tracks := parseCodecs(t, body)
	f, ok := tracks[0].Codec.(audiostream.CodecFLAC)
	if !ok {
		t.Fatalf("Codec = %T, want CodecFLAC", tracks[0].Codec)
	}
	if f.StreamInfo != nil {
		t.Errorf("StreamInfo = % x, want nil for a non-34-byte STREAMINFO", f.StreamInfo)
	}
}

// A streaminfo value that is not valid base64 leaves StreamInfo nil without
// demoting the track: an unusable fmtp must not make a FLAC stream unplayable.
func TestCodecsFLACMalformedStreaminfo(t *testing.T) {
	t.Parallel()
	body := []byte("v=0\r\n" +
		"m=audio 0 RTP/AVP 96\r\n" +
		"a=rtpmap:96 FLAC/48000/2\r\n" +
		"a=fmtp:96 streaminfo=not!valid!base64!\r\n")
	tracks := parseCodecs(t, body)
	f, ok := tracks[0].Codec.(audiostream.CodecFLAC)
	if !ok {
		t.Fatalf("Codec = %T, want CodecFLAC", tracks[0].Codec)
	}
	if f.StreamInfo != nil {
		t.Errorf("StreamInfo = % x, want nil for a malformed streaminfo", f.StreamInfo)
	}
}

// A present-but-empty streaminfo value leaves StreamInfo nil, matching the
// documented "nil otherwise": base64-decoding "" yields a non-nil zero-length
// slice, which the parser must not surface as a 0-byte STREAMINFO.
func TestCodecsFLACEmptyStreaminfo(t *testing.T) {
	t.Parallel()
	body := []byte("v=0\r\n" +
		"m=audio 0 RTP/AVP 96\r\n" +
		"a=rtpmap:96 FLAC/48000/2\r\n" +
		"a=fmtp:96 streaminfo=\r\n")
	tracks := parseCodecs(t, body)
	f, ok := tracks[0].Codec.(audiostream.CodecFLAC)
	if !ok {
		t.Fatalf("Codec = %T, want CodecFLAC", tracks[0].Codec)
	}
	if f.StreamInfo != nil {
		t.Errorf("StreamInfo = % x, want nil for an empty streaminfo value", f.StreamInfo)
	}
}

// Unpadded base64 (some senders omit the trailing '=') still decodes.
func TestCodecsFLACUnpaddedStreaminfo(t *testing.T) {
	t.Parallel()
	b64 := base64.RawStdEncoding.EncodeToString(flacStreamInfo) // no padding
	body := []byte("v=0\r\n" +
		"m=audio 0 RTP/AVP 96\r\n" +
		"a=rtpmap:96 FLAC/48000/2\r\n" +
		"a=fmtp:96 streaminfo=" + b64 + "\r\n")
	tracks := parseCodecs(t, body)
	f, ok := tracks[0].Codec.(audiostream.CodecFLAC)
	if !ok {
		t.Fatalf("Codec = %T, want CodecFLAC", tracks[0].Codec)
	}
	if !bytes.Equal(f.StreamInfo, flacStreamInfo) {
		t.Errorf("StreamInfo = % x, want % x (unpadded base64 must decode)", f.StreamInfo, flacStreamInfo)
	}
}

func parseCodecs(t *testing.T, body []byte) []sdp.DescribedTrack {
	t.Helper()
	s, err := sdp.Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	tracks := s.Codecs()
	if len(tracks) != 1 {
		t.Fatalf("track count = %d, want 1", len(tracks))
	}
	return tracks
}
