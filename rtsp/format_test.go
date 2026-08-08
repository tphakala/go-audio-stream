package rtsp

import (
	"reflect"
	"testing"

	audiostream "github.com/tphakala/go-audio-stream"
)

func TestTrackFormat(t *testing.T) {
	tests := []struct {
		name     string
		track    Track
		wantKind audiostream.PayloadKind
		wantRate int
		wantChan int
	}{
		{
			name:     "aac zeroes geometry even when rtpmap present",
			track:    Track{Codec: audiostream.CodecAAC{}, ClockRate: 16000, Channels: 1},
			wantKind: audiostream.KindCompressed,
			wantRate: 0,
			wantChan: 0,
		},
		{
			name:     "mp4a-latm zeroes geometry even when rtpmap present",
			track:    Track{Codec: audiostream.CodecMP4ALATM{AudioSpecificConfig: []byte{0x12, 0x10}}, ClockRate: 44100, Channels: 2},
			wantKind: audiostream.KindCompressed,
			wantRate: 0,
			wantChan: 0,
		},
		{
			name:     "opus zeroes geometry",
			track:    Track{Codec: audiostream.CodecOpus{}, ClockRate: 48000, Channels: 2},
			wantKind: audiostream.KindCompressed,
			wantRate: 0,
			wantChan: 0,
		},
		{
			name:     "g711 carries pcm geometry",
			track:    Track{Codec: audiostream.CodecG711{Law: audiostream.MuLaw}, ClockRate: 8000, Channels: 1},
			wantKind: audiostream.KindPCMS16LE,
			wantRate: 8000,
			wantChan: 1,
		},
		{
			name:     "l16 carries pcm geometry",
			track:    Track{Codec: audiostream.CodecL16{ClockRate: 44100, Channels: 2}, ClockRate: 44100, Channels: 2},
			wantKind: audiostream.KindPCMS16LE,
			wantRate: 44100,
			wantChan: 2,
		},
		{
			name:     "unknown is opaque",
			track:    Track{Codec: audiostream.CodecUnknown{RTPMap: "X/8000"}},
			wantKind: audiostream.KindOpaque,
			wantRate: 0,
			wantChan: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.track.Format()
			if got.Kind != tt.wantKind {
				t.Errorf("Kind = %v, want %v", got.Kind, tt.wantKind)
			}
			if got.SampleRate != tt.wantRate {
				t.Errorf("SampleRate = %d, want %d", got.SampleRate, tt.wantRate)
			}
			if got.Channels != tt.wantChan {
				t.Errorf("Channels = %d, want %d", got.Channels, tt.wantChan)
			}
			// Codec is passed through unchanged, value and all. Use
			// reflect.DeepEqual rather than ==: CodecAAC holds a byte slice, so ==
			// would panic on it.
			if got.Codec == nil {
				t.Fatalf("Codec is nil")
			}
			if !reflect.DeepEqual(got.Codec, tt.track.Codec) {
				t.Errorf("Codec = %#v, want %#v", got.Codec, tt.track.Codec)
			}
		})
	}
}
