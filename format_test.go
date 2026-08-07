package audiostream_test

import (
	"testing"

	audiostream "github.com/tphakala/go-audio-stream"
)

func TestPayloadKindFor(t *testing.T) {
	tests := []struct {
		name  string
		codec audiostream.Codec
		want  audiostream.PayloadKind
	}{
		{"aac", audiostream.CodecAAC{}, audiostream.KindCompressed},
		{"opus", audiostream.CodecOpus{}, audiostream.KindCompressed},
		{"g711 mu-law", audiostream.CodecG711{Law: audiostream.MuLaw}, audiostream.KindPCMS16LE},
		{"g711 a-law", audiostream.CodecG711{Law: audiostream.ALaw}, audiostream.KindPCMS16LE},
		{"l16", audiostream.CodecL16{ClockRate: 44100, Channels: 2}, audiostream.KindPCMS16LE},
		{"codec-unknown is opaque", audiostream.CodecUnknown{RTPMap: "X/8000"}, audiostream.KindOpaque},
		{"nil codec defaults to unknown", nil, audiostream.KindUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := audiostream.PayloadKindFor(tt.codec); got != tt.want {
				t.Fatalf("PayloadKindFor(%T) = %v, want %v", tt.codec, got, tt.want)
			}
		})
	}
}

func TestPayloadKindString(t *testing.T) {
	tests := []struct {
		kind audiostream.PayloadKind
		want string
	}{
		{audiostream.KindUnknown, "unknown"},
		{audiostream.KindCompressed, "compressed"},
		{audiostream.KindPCMS16LE, "pcm-s16le"},
		{audiostream.KindOpaque, "opaque"},
		{audiostream.PayloadKind(255), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.kind.String(); got != tt.want {
			t.Errorf("PayloadKind(%d).String() = %q, want %q", tt.kind, got, tt.want)
		}
	}
}

// A zero-value AudioFormat must report KindUnknown, not a decodable kind, so an
// uninitialized or map-miss descriptor never tells a consumer to decode a nil
// codec.
func TestAudioFormatZeroValueIsUnknown(t *testing.T) {
	var f audiostream.AudioFormat
	if f.Kind != audiostream.KindUnknown {
		t.Errorf("zero AudioFormat Kind = %v, want KindUnknown", f.Kind)
	}
}
