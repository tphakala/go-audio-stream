package rtsp

import (
	"testing"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/rtsp/sdp"
)

func TestConfigFromAAC(t *testing.T) {
	t.Parallel()
	cfg := configFromAAC(&sdp.AACParams{SizeLength: 13, IndexLength: 3, IndexDeltaLength: 3})
	if cfg.SizeLength != 13 || cfg.IndexLength != 3 || cfg.IndexDeltaLength != 3 {
		t.Errorf("field widths = %d/%d/%d, want 13/3/3", cfg.SizeLength, cfg.IndexLength, cfg.IndexDeltaLength)
	}
	if cfg.SamplesPerFrame != 1024 {
		t.Errorf("SamplesPerFrame = %d, want 1024", cfg.SamplesPerFrame)
	}
}

func TestNewTrackAACHbrCaseInsensitive(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		mode     string
		wantKind codecKind
		wantAAC  bool
	}{
		{name: "lowercase hbr", mode: "aac-hbr", wantKind: deliverAAC, wantAAC: true},
		{name: "canonical hbr", mode: "AAC-hbr", wantKind: deliverAAC, wantAAC: true},
		{name: "latm falls back to raw", mode: "MP4A-LATM", wantKind: deliverRaw, wantAAC: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			desc := describedTrack{
				codec:     audiostream.CodecAAC{},
				clockRate: 16000,
				media:     audiostream.MediaAudio,
				aac:       &sdp.AACParams{Mode: tc.mode, SizeLength: 13, IndexLength: 3, IndexDeltaLength: 3},
			}
			tr := newTrack(0, desc, SetupOptions{}, 0, 1, nil)
			if tr.kind != tc.wantKind {
				t.Errorf("kind = %d, want %d", tr.kind, tc.wantKind)
			}
			if (tr.aac != nil) != tc.wantAAC {
				t.Errorf("aac != nil = %v, want %v", tr.aac != nil, tc.wantAAC)
			}
		})
	}
}

func TestNewTrackInvalidAACFmtpFallsBackToRaw(t *testing.T) {
	t.Parallel()
	// SizeLength 0 is rejected by aac.New; the track must degrade to raw
	// delivery rather than failing.
	desc := describedTrack{
		codec: audiostream.CodecAAC{},
		aac:   &sdp.AACParams{Mode: "AAC-hbr", SizeLength: 0},
	}
	tr := newTrack(0, desc, SetupOptions{}, 0, 1, nil)
	if tr.kind != deliverRaw {
		t.Errorf("kind = %d, want deliverRaw for invalid fmtp", tr.kind)
	}
	if tr.aac != nil {
		t.Error("aac depacketizer must be nil when fmtp is invalid")
	}
}

func TestNewTrackCodecSelection(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		codec    audiostream.Codec
		wantKind codecKind
	}{
		{name: "opus", codec: audiostream.CodecOpus{}, wantKind: deliverOpus},
		{name: "g711 mulaw", codec: audiostream.CodecG711{Law: audiostream.MuLaw}, wantKind: deliverG711},
		{name: "unknown", codec: audiostream.CodecUnknown{}, wantKind: deliverRaw},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newTrack(0, describedTrack{codec: tc.codec}, SetupOptions{}, 0, 1, nil)
			if tr.kind != tc.wantKind {
				t.Errorf("kind = %d, want %d", tr.kind, tc.wantKind)
			}
		})
	}
}

func TestChannelTableLookup(t *testing.T) {
	t.Parallel()
	tr0 := &track{id: 0}
	tr1 := &track{id: 1}
	table := newChannelTable(nil, tr0, 0, 1)
	table = newChannelTable(table, tr1, 2, 3)

	if b, ok := table.lookup(0); !ok || b.track != tr0 || b.isRTCP {
		t.Errorf("lookup(0) = %+v ok=%v, want tr0 RTP", b, ok)
	}
	if b, ok := table.lookup(1); !ok || b.track != tr0 || !b.isRTCP {
		t.Errorf("lookup(1) = %+v ok=%v, want tr0 RTCP", b, ok)
	}
	if b, ok := table.lookup(2); !ok || b.track != tr1 || b.isRTCP {
		t.Errorf("lookup(2) = %+v ok=%v, want tr1 RTP", b, ok)
	}
	if _, ok := table.lookup(9); ok {
		t.Error("lookup(9) ok = true, want false for an unbound channel")
	}
	var nilTable *channelTable
	if _, ok := nilTable.lookup(0); ok {
		t.Error("nil table lookup ok = true, want false")
	}
}
