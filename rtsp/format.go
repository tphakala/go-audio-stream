package rtsp

import audiostream "github.com/tphakala/go-audio-stream"

// Format returns the source-agnostic audio format descriptor for the track.
//
// The codec is passed through and the payload kind is derived from it. SampleRate
// and Channels are filled from the track's rtpmap (ClockRate, Channels) only for
// a PCM payload; for a compressed or opaque payload they stay 0, because the
// rtpmap geometry is not reliable for compressed audio and the consumer must
// read the true geometry from its decoder. See audiostream.AudioFormat.
//
//nolint:gocritic // Track is a public value type (Describe returns []Track); a value-receiver getter keeps Format callable on any Track expression, and the per-track copy is a one-time cost.
func (t Track) Format() audiostream.AudioFormat {
	f := audiostream.AudioFormat{
		Codec: t.Codec,
		Kind:  audiostream.PayloadKindFor(t.Codec),
	}
	if f.Kind == audiostream.KindPCMS16LE {
		f.SampleRate = t.ClockRate
		f.Channels = t.Channels
	}
	return f
}
