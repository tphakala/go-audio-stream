package sdp

import (
	"errors"
	"testing"

	audiostream "github.com/tphakala/go-audio-stream"
)

const testControl = "trackID=0"

func TestWriteSessionParsesBackL16(t *testing.T) {
	spec := WriteSpec{
		SessionID:    0,
		PayloadType:  96,
		EncodingName: encodingL16,
		ClockRate:    256000,
		Channels:     1,
		Control:      testControl,
		Ptime:        20,
	}
	raw, err := WriteSession(spec)
	if err != nil {
		t.Fatalf("WriteSession: %v", err)
	}
	sess, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	tracks := sess.Codecs()
	if len(tracks) != 1 {
		t.Fatalf("Codecs returned %d tracks, want 1", len(tracks))
	}
	tr := tracks[0]
	l16, ok := tr.Codec.(audiostream.CodecL16)
	if !ok {
		t.Fatalf("Codec = %T, want CodecL16", tr.Codec)
	}
	if l16.ClockRate != 256000 || l16.Channels != 1 {
		t.Errorf("CodecL16 = %+v, want {256000, 1}", l16)
	}
	if tr.ClockRate != 256000 || tr.Channels != 1 || tr.PayloadType != 96 {
		t.Errorf("track = pt %d, clock %d, ch %d; want 96, 256000, 1", tr.PayloadType, tr.ClockRate, tr.Channels)
	}
	if tr.Control != testControl {
		t.Errorf("Control = %q, want trackID=0", tr.Control)
	}
}

func TestWriteSessionParsesBackOpus(t *testing.T) {
	spec := WriteSpec{
		PayloadType:  97,
		EncodingName: "opus",
		ClockRate:    48000,
		Channels:     2, // RFC 7587: always 2, even for a mono source
		Control:      testControl,
		FMTP:         "sprop-stereo=0",
	}
	raw, err := WriteSession(spec)
	if err != nil {
		t.Fatalf("WriteSession: %v", err)
	}
	sess, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	tr := sess.Codecs()[0]
	if _, ok := tr.Codec.(audiostream.CodecOpus); !ok {
		t.Fatalf("Codec = %T, want CodecOpus", tr.Codec)
	}
	if tr.ClockRate != 48000 || tr.Channels != 2 {
		t.Errorf("track clock %d ch %d, want 48000, 2", tr.ClockRate, tr.Channels)
	}
	if tr.FMTP != "sprop-stereo=0" {
		t.Errorf("FMTP = %q, want sprop-stereo=0", tr.FMTP)
	}
}

func TestWriteSessionGoldenBytes(t *testing.T) {
	spec := WriteSpec{
		SessionID:    0,
		PayloadType:  96,
		EncodingName: encodingL16,
		ClockRate:    384000,
		Channels:     1,
		Control:      testControl,
		Ptime:        20,
	}
	raw, err := WriteSession(spec)
	if err != nil {
		t.Fatalf("WriteSession: %v", err)
	}
	want := "v=0\r\n" +
		"o=- 0 0 IN IP4 0.0.0.0\r\n" +
		"s= \r\n" +
		"c=IN IP4 0.0.0.0\r\n" +
		"t=0 0\r\n" +
		"m=audio 0 RTP/AVP 96\r\n" +
		"a=rtpmap:96 L16/384000/1\r\n" +
		"a=ptime:20\r\n" +
		"a=control:trackID=0\r\n"
	if string(raw) != want {
		t.Errorf("golden mismatch:\n got %q\nwant %q", raw, want)
	}
}

func TestWriteSessionRejectsInjection(t *testing.T) {
	for _, spec := range []WriteSpec{
		{PayloadType: 96, EncodingName: encodingL16, ClockRate: 48000, Channels: 1, Name: "bad\r\nv=9"},
		{PayloadType: 96, EncodingName: encodingL16, ClockRate: 48000, Channels: 1, Control: "trackID=0\r\na=recvonly"},
	} {
		if _, err := WriteSession(spec); !errors.Is(err, ErrInjection) {
			t.Errorf("WriteSession(%+v) err = %v, want ErrInjection", spec, err)
		}
	}
}

func TestWriteSessionRejectsBadPayloadType(t *testing.T) {
	for _, pt := range []int{-1, 128, 999} {
		spec := WriteSpec{PayloadType: pt, EncodingName: encodingL16, ClockRate: 48000, Channels: 1}
		if _, err := WriteSession(spec); !errors.Is(err, ErrBadPayloadType) {
			t.Errorf("WriteSession(pt=%d) err = %v, want ErrBadPayloadType", pt, err)
		}
	}
}
