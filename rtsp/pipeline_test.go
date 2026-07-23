package rtsp

import (
	"math"
	"testing"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/rtsp/sdp"
)

// testAACHbrMode is deliberately a test-local literal rather than a reference to
// aacHbrMode: feeding the production constant back in would make a rename of it
// invisible to these tests.
const testAACHbrMode = "AAC-hbr"

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
		{name: "canonical hbr", mode: testAACHbrMode, wantKind: deliverAAC, wantAAC: true},
		// A real RFC 3640 mode this milestone does not decode. MP4A-LATM would
		// be the wrong fixture here: it is an encoding name, so such a track
		// never resolves to CodecAAC and never reaches the mode comparison.
		{name: "non-hbr mode falls back to raw", mode: "AAC-lbr", wantKind: deliverRaw, wantAAC: false},
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
		media: audiostream.MediaAudio,
		aac:   &sdp.AACParams{Mode: testAACHbrMode, SizeLength: 0},
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
			desc := describedTrack{codec: tc.codec, media: audiostream.MediaAudio}
			tr := newTrack(0, desc, SetupOptions{}, 0, 1, nil)
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

// A video section can advertise an audio encoding name, and the SDP layer
// resolves the codec from that name without regard to the m= kind. Only the
// media check keeps such a track off the AAC depacketizer, so assert it with a
// descriptor that would otherwise select deliverAAC.
func TestNewTrackNonAudioFallsBackToRaw(t *testing.T) {
	t.Parallel()
	aacParams := &sdp.AACParams{Mode: testAACHbrMode, SizeLength: 13, IndexLength: 3, IndexDeltaLength: 3}
	for _, media := range []audiostream.MediaKind{
		audiostream.MediaVideo,
		audiostream.MediaOther,
		audiostream.MediaUnknown,
	} {
		t.Run(media.String(), func(t *testing.T) {
			t.Parallel()
			desc := describedTrack{
				codec:     audiostream.CodecAAC{},
				clockRate: 90000,
				media:     media,
				aac:       aacParams,
			}
			tr := newTrack(0, desc, SetupOptions{}, 0, 1, nil)
			if tr.kind != deliverRaw {
				t.Errorf("kind = %d, want deliverRaw for a %s track", tr.kind, media)
			}
			if tr.aac != nil {
				t.Error("a non-audio track must not get an AAC depacketizer")
			}
		})
	}
}

func TestNewTrackDiscardRecorded(t *testing.T) {
	t.Parallel()
	desc := describedTrack{codec: audiostream.CodecOpus{}, media: audiostream.MediaAudio}
	for _, want := range []bool{false, true} {
		if got := newTrack(0, desc, SetupOptions{Discard: want}, 0, 1, nil).discard; got != want {
			t.Errorf("discard = %v, want %v", got, want)
		}
	}
}

// The clock rate is remote input parsed with a plain Atoi, so both ends of the
// range have to be clamped to the "unknown" sentinel rather than reaching the
// PTS arithmetic.
func TestClockRateTicks(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		rate int
		want uint64
	}{
		{name: "typical", rate: 16000, want: 16000},
		{name: "video clock", rate: 90000, want: 90000},
		{name: "max representable", rate: math.MaxUint32, want: math.MaxUint32},
		{name: "absent", rate: 0, want: 0},
		{name: "negative", rate: -1, want: 0},
		{name: "above 32 bits", rate: math.MaxUint32 + 1, want: 0},
		{name: "max int", rate: math.MaxInt64, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := clockRateTicks(tc.rate); got != tc.want {
				t.Errorf("clockRateTicks(%d) = %d, want %d", tc.rate, got, tc.want)
			}
		})
	}
}

// Rebinding a channel that another track already holds is not something Setup
// can produce (InterleavedChannels rejects an overlapping pair first), so this
// pins which way the table resolves it if that guard is ever bypassed: last
// writer wins, and the earlier track silently loses the channel.
func TestChannelTableRebindOverwrites(t *testing.T) {
	t.Parallel()
	tr0 := &track{id: 0}
	tr1 := &track{id: 1}
	table := newChannelTable(newChannelTable(nil, tr0, 0, 1), tr1, 0, 1)
	b, ok := table.lookup(0)
	if !ok || b.track != tr1 {
		t.Errorf("lookup(0).track = %+v ok=%v, want the later track", b, ok)
	}
	if len(table.bindings) != 2 {
		t.Errorf("bindings = %d, want 2 (the rebind must not grow the table)", len(table.bindings))
	}
}
