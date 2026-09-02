package rtsp

import (
	"bytes"
	"encoding/binary"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/depacket/aac"
	"github.com/tphakala/go-audio-stream/depacket/g711"
	"github.com/tphakala/go-audio-stream/depacket/g726"
	"github.com/tphakala/go-audio-stream/rtsp/rtp"
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
			tr := newTrack(0, desc, SetupOptions{}, 1, nil)
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
	tr := newTrack(0, desc, SetupOptions{}, 1, nil)
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
		{name: "l16", codec: audiostream.CodecL16{ClockRate: 48000, Channels: 1}, wantKind: deliverL16},
		{name: "g726", codec: audiostream.CodecG726{BitRate: audiostream.G726Rate32, ClockRate: 8000, Channels: 1}, wantKind: deliverG726},
		{name: "flac", codec: audiostream.CodecFLAC{}, wantKind: deliverFLAC},
		{name: "unknown", codec: audiostream.CodecUnknown{}, wantKind: deliverRaw},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			desc := describedTrack{codec: tc.codec, media: audiostream.MediaAudio}
			tr := newTrack(0, desc, SetupOptions{}, 1, nil)
			if tr.kind != tc.wantKind {
				t.Errorf("kind = %d, want %d", tr.kind, tc.wantKind)
			}
			if _, isFLAC := tc.codec.(audiostream.CodecFLAC); isFLAC && tr.flac == nil {
				t.Error("FLAC track must build a depacketizer")
			}
		})
	}
}

// TestNewTrackG726OverrideDoesNotRescueUnknown pins the documented contract that
// SetupOptions.G726Packing applies only to a track the SDP already resolved to
// CodecG726. newTrack consults the override solely in the CodecG726 dispatch arm,
// so a CodecUnknown track never reaches it and stays on deliverRaw whatever the
// packing value: the forced override in the input is inert by construction. The
// test therefore guards the dispatch itself, that passing a forced override
// alongside a CodecUnknown descriptor is a no-op and never promotes it to
// deliverG726.
func TestNewTrackG726OverrideDoesNotRescueUnknown(t *testing.T) {
	t.Parallel()
	desc := describedTrack{
		codec:     audiostream.CodecUnknown{RTPMap: "G726-32/8000"},
		clockRate: 8000,
		media:     audiostream.MediaAudio,
	}
	tr := newTrack(0, desc, SetupOptions{G726Packing: G726PackingForceAAL2}, 1, nil)
	if tr.kind != deliverRaw {
		t.Errorf("kind = %d, want deliverRaw (override must not rescue a CodecUnknown track)", tr.kind)
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
			tr := newTrack(0, desc, SetupOptions{}, 1, nil)
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
		if got := newTrack(0, desc, SetupOptions{Discard: want}, 1, nil).discard; got != want {
			t.Errorf("discard = %v, want %v", got, want)
		}
	}
}

// The clock rate is remote input parsed with a plain Atoi, so both ends of the
// range have to be clamped to the "unknown" sentinel rather than reaching the
// PTS arithmetic.
func TestClockRateTicks(t *testing.T) {
	t.Parallel()
	// The rate is carried as int64 rather than int because the interesting
	// values do not fit in an int on a 32-bit platform, where the untyped
	// constants would not compile at all.
	cases := []struct {
		name string
		rate int64
		want uint64
	}{
		{name: "typical", rate: 16000, want: 16000},
		{name: "video clock", rate: 90000, want: 90000},
		{name: "max representable", rate: math.MaxUint32, want: math.MaxUint32},
		{name: "absent", rate: 0, want: 0},
		{name: "negative", rate: -1, want: 0},
		{name: "above 32 bits", rate: math.MaxUint32 + 1, want: 0},
		{name: "max int64", rate: math.MaxInt64, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if int64(int(tc.rate)) != tc.rate {
				t.Skipf("rate %d is not representable in an int on this platform", tc.rate)
			}
			if got := clockRateTicks(int(tc.rate)); got != tc.want {
				t.Errorf("clockRateTicks(%d) = %d, want %d", tc.rate, got, tc.want)
			}
		})
	}
}

// buildAUHeaders builds an AAC-hbr payload (sizelength=13, indexlength=3,
// indexdeltalength=3) carrying the given access units in one packet: a 16-bit
// AU-headers-length in bits, one 16-bit AU-header per AU (13-bit size, 3-bit
// index or delta of zero), then the concatenated AU data.
func buildAUHeaders(aus ...[]byte) []byte {
	buf := binary.BigEndian.AppendUint16(nil, uint16(len(aus)*16))
	for _, au := range aus {
		buf = binary.BigEndian.AppendUint16(buf, uint16(len(au))<<3)
	}
	for _, au := range aus {
		buf = append(buf, au...)
	}
	return buf
}

// buildAUFragment builds an AAC-hbr packet carrying one fragment of an access
// unit: a 16-bit AU-headers-length, a single 16-bit AU-header whose 13-bit size
// is the TOTAL size of the complete access unit (declaredSize), then the
// fragment's data bytes. When declaredSize exceeds len(data) and the RTP marker
// is clear, the depacketizer buffers this as a fragment start (yielding no AU);
// the completing fragment carries the same declaredSize with the marker set.
func buildAUFragment(declaredSize int, data []byte) []byte {
	buf := binary.BigEndian.AppendUint16(nil, 16)
	buf = binary.BigEndian.AppendUint16(buf, uint16(declaredSize)<<3)
	return append(buf, data...)
}

// newAACDepacketizer returns a depacketizer configured for the common AAC-hbr
// field widths used across the delivery tests.
func newAACDepacketizer(t *testing.T) *aac.Depacketizer {
	t.Helper()
	dp, err := aac.New(aac.Config{SizeLength: 13, IndexLength: 3, IndexDeltaLength: 3, SamplesPerFrame: 1024})
	if err != nil {
		t.Fatalf("aac.New: %v", err)
	}
	return dp
}

func TestPtsOf(t *testing.T) {
	t.Parallel()
	tr := &track{clockRate: 16000, baseTS: 1000}
	cases := []struct {
		ts   uint64
		want time.Duration
	}{
		{ts: 1000, want: 0},
		{ts: 1000 + 16000, want: time.Second},
		{ts: 1000 + 8000, want: 500 * time.Millisecond},
		{ts: 1000 + 1600, want: 100 * time.Millisecond},
	}
	for _, tc := range cases {
		if got := tr.ptsOf(tc.ts); got != tc.want {
			t.Errorf("ptsOf(ts=%d) = %v, want %v", tc.ts, got, tc.want)
		}
	}
	// A non-positive clock rate yields a zero PTS rather than a divide-by-zero.
	zero := &track{clockRate: 0, baseTS: 0}
	if got := zero.ptsOf(123456); got != 0 {
		t.Errorf("ptsOf with zero clock rate = %v, want 0", got)
	}
}

func TestDeliverOpus(t *testing.T) {
	t.Parallel()
	tr := &track{id: 2, kind: deliverOpus, clockRate: 48000}
	tr.baseSet.Store(true)
	payload := []byte{0x78, 0x01, 0x02, 0x03}
	pkt := rtp.Packet{Header: rtp.Header{Timestamp: 480}, Payload: payload}
	up := rtp.Update{Timestamp: 480, Gap: 0}

	var got audiostream.Frame
	n := 0
	tr.deliver(pkt, up, time.Unix(1, 0), func(f audiostream.Frame) { got = copyFrame(&f); n++ })
	if n != 1 {
		t.Fatalf("delivered %d frames, want 1", n)
	}
	if got.TrackID != 2 {
		t.Errorf("TrackID = %d, want 2", got.TrackID)
	}
	if !bytes.Equal(got.Data, payload) {
		t.Errorf("Data = % x, want the payload % x", got.Data, payload)
	}
	if got.RTPTime != 480 {
		t.Errorf("RTPTime = %d, want 480", got.RTPTime)
	}
	if got.PTS != 10*time.Millisecond { // 480 ticks / 48000 Hz
		t.Errorf("PTS = %v, want 10ms", got.PTS)
	}
}

func TestDeliverOpusEmptyPayloadMalformed(t *testing.T) {
	t.Parallel()
	tr := &track{id: 0, kind: deliverOpus, clockRate: 48000}
	tr.baseSet.Store(true)
	pkt := rtp.Packet{Header: rtp.Header{}, Payload: nil}
	n := 0
	tr.deliver(pkt, rtp.Update{}, time.Unix(1, 0), func(audiostream.Frame) { n++ })
	if n != 0 {
		t.Errorf("delivered %d frames for an empty Opus payload, want 0", n)
	}
	if tr.malformed.Load() != 1 {
		t.Errorf("malformed = %d, want 1", tr.malformed.Load())
	}
}

func TestDeliverG711(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		law  audiostream.Law
	}{
		{name: "mu-law", law: audiostream.MuLaw},
		{name: "a-law", law: audiostream.ALaw},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := &track{id: 3, kind: deliverG711, clockRate: 8000, law: tc.law}
			tr.baseSet.Store(true)
			payload := []byte{0x00, 0x7f, 0x80, 0xff}
			pkt := rtp.Packet{Header: rtp.Header{Timestamp: 160}, Payload: payload}

			var got audiostream.Frame
			tr.deliver(pkt, rtp.Update{Timestamp: 160}, time.Unix(1, 0), func(f audiostream.Frame) { got = copyFrame(&f) })
			want, derr := g711.DepacketizeAlloc(payload, tc.law)
			if derr != nil {
				t.Fatalf("DepacketizeAlloc: %v", derr)
			}
			if !bytes.Equal(got.Data, want) {
				t.Errorf("Data = % x, want % x", got.Data, want)
			}
			if len(got.Data) != 2*len(payload) {
				t.Errorf("len(Data) = %d, want %d", len(got.Data), 2*len(payload))
			}
		})
	}
}

func TestDeliverG711BufferReuse(t *testing.T) {
	t.Parallel()
	tr := &track{id: 0, kind: deliverG711, clockRate: 8000, law: audiostream.MuLaw}
	tr.baseSet.Store(true)
	big := rtp.Packet{Payload: make([]byte, 200)}
	tr.deliver(big, rtp.Update{}, time.Unix(1, 0), func(audiostream.Frame) {})
	grown := cap(tr.pcmBuf)
	if grown < 400 {
		t.Fatalf("pcmBuf cap = %d, want >= 400 after a 200-byte packet", grown)
	}
	// A smaller subsequent packet must reuse the buffer, not grow it.
	small := rtp.Packet{Payload: make([]byte, 10)}
	tr.deliver(small, rtp.Update{}, time.Unix(1, 0), func(audiostream.Frame) {})
	if cap(tr.pcmBuf) != grown {
		t.Errorf("pcmBuf cap = %d after a smaller packet, want unchanged %d", cap(tr.pcmBuf), grown)
	}
}

// TestDeliverG726 checks the G.726 delivery path produces the decoder's s16le
// PCM as one frame, and that resetDepacketizer(true) restarts the adaptive
// decoder state on an SSRC change while a plain gap leaves it running.
func TestDeliverG726(t *testing.T) {
	t.Parallel()
	payload := []byte{0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde, 0xf0}

	newG726Track := func() *track {
		tr := &track{id: 3, kind: deliverG726, clockRate: 8000}
		dec, err := g726.New(audiostream.G726Rate32, audiostream.G726PackingRFC3551)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		tr.g726 = dec
		tr.baseSet.Store(true)
		return tr
	}

	// One packet decodes to the reference decoder's output.
	tr := newG726Track()
	ref, _ := g726.New(audiostream.G726Rate32, audiostream.G726PackingRFC3551)
	want, err := ref.DecodeAlloc(payload)
	if err != nil {
		t.Fatalf("DecodeAlloc: %v", err)
	}
	var got audiostream.Frame
	pkt := rtp.Packet{Header: rtp.Header{Timestamp: 160}, Payload: payload}
	tr.deliver(pkt, rtp.Update{Timestamp: 160}, time.Unix(1, 0), func(f audiostream.Frame) { got = copyFrame(&f) })
	if !bytes.Equal(got.Data, want) {
		t.Errorf("first frame Data = % x, want % x", got.Data, want)
	}

	// The reference for the SECOND packet, on a decoder that already consumed
	// the first (state carries across packets).
	wantSecond, err := ref.DecodeAlloc(payload)
	if err != nil {
		t.Fatalf("DecodeAlloc second: %v", err)
	}
	// Precondition the reset/no-reset checks below rely on: continued adaptive
	// state produces different output than a fresh decode of the same payload.
	// If these were equal, the reset assertions could pass trivially.
	if bytes.Equal(want, wantSecond) {
		t.Fatal("test payload does not distinguish fresh from continued decoder state")
	}

	// A plain gap must NOT reset the decoder: the second packet continues the
	// adapted state, matching wantSecond.
	tr.resetDepacketizer(false)
	tr.deliver(pkt, rtp.Update{Timestamp: 320, Gap: 1}, time.Unix(2, 0), func(f audiostream.Frame) { got = copyFrame(&f) })
	if !bytes.Equal(got.Data, wantSecond) {
		t.Errorf("after plain gap Data = % x, want continued-state % x", got.Data, wantSecond)
	}
	if got.SeqGap != 1 {
		t.Errorf("SeqGap = %d, want 1 folded onto the frame", got.SeqGap)
	}

	// An SSRC change MUST reset the decoder: after it, decoding the payload
	// matches a fresh decoder's first-packet output again.
	tr2 := newG726Track()
	tr2.deliver(pkt, rtp.Update{Timestamp: 160}, time.Unix(1, 0), func(audiostream.Frame) {})
	tr2.resetDepacketizer(true)
	tr2.deliver(pkt, rtp.Update{Timestamp: 160}, time.Unix(3, 0), func(f audiostream.Frame) { got = copyFrame(&f) })
	if !bytes.Equal(got.Data, want) {
		t.Errorf("after SSRC reset Data = % x, want fresh-state % x", got.Data, want)
	}
}

func TestDeliverL16(t *testing.T) {
	t.Parallel()
	// Two 16-bit samples, big-endian on the wire (RFC 3551 network byte order).
	// deliverL16 must hand them to OnFrame byte-swapped to little-endian s16le,
	// the same byte order G.711 delivers.
	tr := &track{id: 4, kind: deliverL16, clockRate: 48000}
	tr.baseSet.Store(true)
	// Three distinct-valued samples, so a length-dependent or short-prefix swap
	// bug (which a single 2-sample payload would miss) is caught.
	payload := []byte{0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc}
	want := []byte{0x34, 0x12, 0x78, 0x56, 0xbc, 0x9a}
	pkt := rtp.Packet{Header: rtp.Header{Timestamp: 480}, Payload: payload}

	var got audiostream.Frame
	n := 0
	tr.deliver(pkt, rtp.Update{Timestamp: 480}, time.Unix(1, 0), func(f audiostream.Frame) { got = copyFrame(&f); n++ })
	if n != 1 {
		t.Fatalf("delivered %d frames, want 1", n)
	}
	if got.TrackID != 4 {
		t.Errorf("TrackID = %d, want 4", got.TrackID)
	}
	if !bytes.Equal(got.Data, want) {
		t.Errorf("Data = % x, want s16le % x", got.Data, want)
	}
	if got.RTPTime != 480 {
		t.Errorf("RTPTime = %d, want 480", got.RTPTime)
	}
	if got.PTS != 10*time.Millisecond { // 480 ticks / 48000 Hz
		t.Errorf("PTS = %v, want 10ms", got.PTS)
	}
	// The original payload must be left untouched: the swap writes into pcmBuf.
	if !bytes.Equal(payload, []byte{0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc}) {
		t.Errorf("source payload mutated to % x", payload)
	}
}

func TestDeliverL16Malformed(t *testing.T) {
	t.Parallel()
	// A payload that is not a whole number of 16-bit samples (odd length) or is
	// empty counts malformed and yields no frame, mirroring the empty-Opus rule.
	for _, tc := range []struct {
		name    string
		payload []byte
	}{
		{name: "odd length", payload: []byte{0x00, 0x11, 0x22}},
		{name: "empty", payload: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := &track{id: 0, kind: deliverL16, clockRate: 48000}
			tr.baseSet.Store(true)
			pkt := rtp.Packet{Header: rtp.Header{}, Payload: tc.payload}
			n := 0
			tr.deliver(pkt, rtp.Update{}, time.Unix(1, 0), func(audiostream.Frame) { n++ })
			if n != 0 {
				t.Errorf("delivered %d frames for a %s L16 payload, want 0", n, tc.name)
			}
			if tr.malformed.Load() != 1 {
				t.Errorf("malformed = %d, want 1", tr.malformed.Load())
			}
		})
	}
}

func TestDeliverL16BufferReuse(t *testing.T) {
	t.Parallel()
	tr := &track{id: 0, kind: deliverL16, clockRate: 48000}
	tr.baseSet.Store(true)
	big := rtp.Packet{Payload: make([]byte, 320)}
	tr.deliver(big, rtp.Update{}, time.Unix(1, 0), func(audiostream.Frame) {})
	grown := cap(tr.pcmBuf)
	if grown < 320 {
		t.Fatalf("pcmBuf cap = %d, want >= 320 after a 320-byte packet", grown)
	}
	small := rtp.Packet{Payload: make([]byte, 40)}
	tr.deliver(small, rtp.Update{}, time.Unix(1, 0), func(audiostream.Frame) {})
	if cap(tr.pcmBuf) != grown {
		t.Errorf("pcmBuf cap = %d after a smaller packet, want unchanged %d", cap(tr.pcmBuf), grown)
	}
}

func TestDeliverL16NilOnFrame(t *testing.T) {
	t.Parallel()
	// A valid payload with a nil OnFrame delivers nothing and, like the Opus
	// path, records no malformed packet.
	valid := &track{id: 0, kind: deliverL16, clockRate: 48000}
	valid.baseSet.Store(true)
	valid.deliver(rtp.Packet{Payload: []byte{0x00, 0x11}}, rtp.Update{}, time.Unix(1, 0), nil)
	if valid.malformed.Load() != 0 {
		t.Errorf("malformed = %d for a valid payload with nil OnFrame, want 0", valid.malformed.Load())
	}
	// An odd-length payload with a nil OnFrame still counts malformed: the count
	// must not depend on a callback being registered. This pins the guard order
	// (malformed counted before the onFrame==nil check); reordering the two
	// guards would make this assertion fail.
	odd := &track{id: 0, kind: deliverL16, clockRate: 48000}
	odd.baseSet.Store(true)
	odd.deliver(rtp.Packet{Payload: []byte{0x00, 0x11, 0x22}}, rtp.Update{}, time.Unix(1, 0), nil)
	if odd.malformed.Load() != 1 {
		t.Errorf("malformed = %d for an odd payload with nil OnFrame, want 1", odd.malformed.Load())
	}
}

func TestDeliverL16StereoWholeFrames(t *testing.T) {
	t.Parallel()
	// A stereo L16 track validates whole sample-frames (2*channels = 4 bytes),
	// not just whole samples, so a truncated packet that is a whole number of
	// samples but not of frames is rejected rather than delivering a half-frame
	// that would shift L/R interleaving downstream.
	tr := newTrack(0, describedTrack{
		codec:     audiostream.CodecL16{ClockRate: 44100, Channels: 2},
		clockRate: 44100,
		media:     audiostream.MediaAudio,
	}, SetupOptions{}, 1, nil)
	if tr.kind != deliverL16 {
		t.Fatalf("kind = %d, want deliverL16", tr.kind)
	}
	if tr.l16FrameSize != 4 {
		t.Fatalf("l16FrameSize = %d, want 4 for stereo", tr.l16FrameSize)
	}

	// 6 bytes = 1.5 stereo frames: rejected as malformed, no frame.
	n := 0
	tr.deliver(rtp.Packet{Payload: make([]byte, 6)}, rtp.Update{}, time.Unix(1, 0), func(audiostream.Frame) { n++ })
	if n != 0 || tr.malformed.Load() != 1 {
		t.Errorf("half-frame stereo packet: frames=%d malformed=%d, want 0 and 1", n, tr.malformed.Load())
	}

	// 8 bytes = 2 whole stereo frames: delivered as one frame.
	var got audiostream.Frame
	tr.deliver(rtp.Packet{Payload: []byte{0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde, 0xf0}},
		rtp.Update{}, time.Unix(1, 0), func(f audiostream.Frame) { got = copyFrame(&f); n++ })
	if n != 1 {
		t.Fatalf("whole-frame stereo packet delivered %d frames, want 1", n)
	}
	if want := []byte{0x34, 0x12, 0x78, 0x56, 0xbc, 0x9a, 0xf0, 0xde}; !bytes.Equal(got.Data, want) {
		t.Errorf("Data = % x, want s16le % x", got.Data, want)
	}
	if tr.malformed.Load() != 1 {
		t.Errorf("malformed = %d after one valid packet, want 1 (only the half-frame)", tr.malformed.Load())
	}
}

func TestDeliverRaw(t *testing.T) {
	t.Parallel()
	tr := &track{id: 7, kind: deliverRaw, clockRate: 90000}
	tr.baseSet.Store(true)
	payload := []byte{0xde, 0xad, 0xbe, 0xef}
	pkt := rtp.Packet{Header: rtp.Header{Timestamp: 0}, Payload: payload}
	var got audiostream.Frame
	tr.deliver(pkt, rtp.Update{Timestamp: 0, Gap: 4}, time.Unix(1, 0), func(f audiostream.Frame) { got = copyFrame(&f) })
	if !bytes.Equal(got.Data, payload) {
		t.Errorf("Data = % x, want raw payload % x", got.Data, payload)
	}
	if got.SeqGap != 4 {
		t.Errorf("SeqGap = %d, want 4", got.SeqGap)
	}
}

func TestDeliverAACSingleAU(t *testing.T) {
	t.Parallel()
	tr := &track{id: 0, kind: deliverAAC, clockRate: 16000, aac: newAACDepacketizer(t)}
	tr.baseSet.Store(true)
	au := []byte{0x11, 0x22, 0x33}
	pkt := rtp.Packet{Header: rtp.Header{Timestamp: 100, Marker: true}, Payload: buildAUHeaders(au)}

	var frames []audiostream.Frame
	tr.deliver(pkt, rtp.Update{Timestamp: 100, Gap: 3}, time.Unix(1, 0), func(f audiostream.Frame) {
		frames = append(frames, copyFrame(&f))
	})
	if len(frames) != 1 {
		t.Fatalf("delivered %d frames, want 1", len(frames))
	}
	if !bytes.Equal(frames[0].Data, au) {
		t.Errorf("Data = % x, want AU % x", frames[0].Data, au)
	}
	if frames[0].SeqGap != 3 {
		t.Errorf("SeqGap = %d, want 3", frames[0].SeqGap)
	}
	if frames[0].RTPTime != 100 {
		t.Errorf("RTPTime = %d, want 100", frames[0].RTPTime)
	}
}

func TestDeliverAACMultipleAUs(t *testing.T) {
	t.Parallel()
	// baseTS equals the packet timestamp, as the hot path seeds it on the
	// first frame, so the first AU's PTS is 0 and later AUs interpolate.
	tr := &track{id: 0, kind: deliverAAC, clockRate: 16000, aac: newAACDepacketizer(t), baseTS: 200}
	tr.baseSet.Store(true)
	au0, au1, au2 := []byte{1, 2}, []byte{3, 4, 5}, []byte{6}
	pkt := rtp.Packet{Header: rtp.Header{Timestamp: 200, Marker: true}, Payload: buildAUHeaders(au0, au1, au2)}

	var frames []audiostream.Frame
	tr.deliver(pkt, rtp.Update{Timestamp: 200, Gap: 5}, time.Unix(1, 0), func(f audiostream.Frame) {
		frames = append(frames, copyFrame(&f))
	})
	if len(frames) != 3 {
		t.Fatalf("delivered %d frames, want 3", len(frames))
	}
	// SeqGap is reported once, on the first AU of the packet.
	if frames[0].SeqGap != 5 || frames[1].SeqGap != 0 || frames[2].SeqGap != 0 {
		t.Errorf("SeqGaps = %d/%d/%d, want 5/0/0", frames[0].SeqGap, frames[1].SeqGap, frames[2].SeqGap)
	}
	// PTS interpolates by SamplesPerFrame (1024) per AU at 16000 Hz.
	// 1024 and 2048 ticks at 16 kHz. Literals rather than a re-derivation, so
	// the assertion cannot drift along with the arithmetic it is checking.
	wantPTS := []time.Duration{0, 64 * time.Millisecond, 128 * time.Millisecond}
	for i := range frames {
		if frames[i].PTS != wantPTS[i] {
			t.Errorf("frame %d PTS = %v, want %v", i, frames[i].PTS, wantPTS[i])
		}
		if frames[i].RTPTime != 200 {
			t.Errorf("frame %d RTPTime = %d, want 200 (the packet timestamp)", i, frames[i].RTPTime)
		}
	}
	if !bytes.Equal(frames[0].Data, au0) || !bytes.Equal(frames[1].Data, au1) || !bytes.Equal(frames[2].Data, au2) {
		t.Error("AU data mismatch across the multi-AU packet")
	}
}

func TestDeliverAACMalformed(t *testing.T) {
	t.Parallel()
	tr := &track{id: 0, kind: deliverAAC, clockRate: 16000, aac: newAACDepacketizer(t)}
	tr.baseSet.Store(true)
	// A one-byte payload is too short for the AU-headers-length field.
	pkt := rtp.Packet{Header: rtp.Header{Marker: true}, Payload: []byte{0x00}}
	n := 0
	tr.deliver(pkt, rtp.Update{}, time.Unix(1, 0), func(audiostream.Frame) { n++ })
	if n != 0 {
		t.Errorf("delivered %d frames for a malformed AAC packet, want 0", n)
	}
	if tr.malformed.Load() != 1 {
		t.Errorf("malformed = %d, want 1", tr.malformed.Load())
	}
}

// TestDeliverAACSeqGapCarriesToReassembledAU is the core regression for issue
// #103: a loss immediately before a fragment-start packet must surface on the
// access unit that completes on a later in-order packet, not vanish because the
// fragment-start packet delivered no frame to carry it.
func TestDeliverAACSeqGapCarriesToReassembledAU(t *testing.T) {
	t.Parallel()
	tr := &track{id: 0, kind: deliverAAC, clockRate: 16000, aac: newAACDepacketizer(t)}
	tr.baseSet.Store(true)

	// A packet was lost right before the fragment start. The fragment-start
	// packet (marker clear, only 3 of the 6 declared bytes) completes no AU.
	frag1 := rtp.Packet{Header: rtp.Header{Timestamp: 100}, Payload: buildAUFragment(6, []byte{1, 2, 3})}
	n := 0
	tr.deliver(frag1, rtp.Update{Timestamp: 100, Gap: 1}, time.Unix(1, 0), func(audiostream.Frame) { n++ })
	if n != 0 {
		t.Fatalf("fragment start delivered %d frames, want 0 (buffering)", n)
	}

	// The completing fragment (marker set, the remaining 3 bytes) reassembles the
	// 6-byte AU. Its SeqGap must be 1, the loss that preceded the fragment start;
	// before the fix it was 0 because the gap reached no frame.
	frag2 := rtp.Packet{Header: rtp.Header{Timestamp: 100, Marker: true}, Payload: buildAUFragment(6, []byte{4, 5, 6})}
	var got audiostream.Frame
	tr.deliver(frag2, rtp.Update{Timestamp: 100, Gap: 0}, time.Unix(1, 0), func(f audiostream.Frame) {
		got = copyFrame(&f)
		n++
	})
	if n != 1 {
		t.Fatalf("completing fragment delivered %d frames, want 1", n)
	}
	if want := []byte{1, 2, 3, 4, 5, 6}; !bytes.Equal(got.Data, want) {
		t.Errorf("reassembled AU = % x, want % x", got.Data, want)
	}
	if got.SeqGap != 1 {
		t.Errorf("SeqGap = %d, want 1 (the loss before the fragment start must carry to the reassembled AU)", got.SeqGap)
	}
}

// TestDeliverAACMalformedRetainsGap covers the other no-frame path: a malformed
// packet carrying a gap must retain that gap and drain it onto the next good
// frame, and a plain gap in between (resetDepacketizer(false)) must not wipe it.
func TestDeliverAACMalformedRetainsGap(t *testing.T) {
	t.Parallel()
	tr := &track{id: 0, kind: deliverAAC, clockRate: 16000, aac: newAACDepacketizer(t)}
	tr.baseSet.Store(true)

	// A malformed packet (one byte, too short for the AU-headers-length) carrying
	// a gap of 2 delivers no frame but must retain the pending gap.
	bad := rtp.Packet{Header: rtp.Header{Marker: true}, Payload: []byte{0x00}}
	n := 0
	tr.deliver(bad, rtp.Update{Gap: 2}, time.Unix(1, 0), func(audiostream.Frame) { n++ })
	if n != 0 || tr.malformed.Load() != 1 {
		t.Fatalf("malformed packet: frames=%d malformed=%d, want 0 and 1", n, tr.malformed.Load())
	}

	// A plain gap (not an SSRC reset) between the two packets must not clear the
	// retained gap.
	tr.resetDepacketizer(false)

	good := rtp.Packet{Header: rtp.Header{Timestamp: 100, Marker: true}, Payload: buildAUHeaders([]byte{0x11, 0x22})}
	var got audiostream.Frame
	tr.deliver(good, rtp.Update{Timestamp: 100, Gap: 0}, time.Unix(1, 0), func(f audiostream.Frame) { got = copyFrame(&f) })
	if got.SeqGap != 2 {
		t.Errorf("SeqGap = %d, want 2 (a gap on a malformed packet must drain onto the next frame)", got.SeqGap)
	}
}

// TestDeliverAACAccumulatesStrandedGaps proves the accumulator sums: two
// separate losses stranded on two no-frame packets drain together onto the next
// completed AU, so no loss is dropped when several arrive back to back before a
// frame completes.
func TestDeliverAACAccumulatesStrandedGaps(t *testing.T) {
	t.Parallel()
	tr := &track{id: 0, kind: deliverAAC, clockRate: 16000, aac: newAACDepacketizer(t)}
	tr.baseSet.Store(true)

	// Two malformed packets, each carrying a distinct gap, deliver no frame; the
	// gaps must accumulate (2 then 5), not overwrite.
	bad := rtp.Packet{Header: rtp.Header{Marker: true}, Payload: []byte{0x00}}
	tr.deliver(bad, rtp.Update{Gap: 2}, time.Unix(1, 0), func(audiostream.Frame) {})
	tr.deliver(bad, rtp.Update{Gap: 3}, time.Unix(1, 0), func(audiostream.Frame) {})

	good := rtp.Packet{Header: rtp.Header{Timestamp: 100, Marker: true}, Payload: buildAUHeaders([]byte{0x11, 0x22})}
	var got audiostream.Frame
	tr.deliver(good, rtp.Update{Timestamp: 100, Gap: 0}, time.Unix(1, 0), func(f audiostream.Frame) { got = copyFrame(&f) })
	if got.SeqGap != 5 {
		t.Errorf("SeqGap = %d, want 5 (two stranded gaps of 2 and 3 must drain as their sum)", got.SeqGap)
	}
}

// TestDeliverAACSSRCResetClearsPendingGap locks in that an SSRC reset drops a
// pending gap from the old sequence space, so it cannot bleed onto the new
// source's first frame.
func TestDeliverAACSSRCResetClearsPendingGap(t *testing.T) {
	t.Parallel()
	tr := &track{id: 0, kind: deliverAAC, clockRate: 16000, aac: newAACDepacketizer(t)}
	tr.baseSet.Store(true)

	// A malformed packet leaves a gap of 3 pending.
	bad := rtp.Packet{Header: rtp.Header{Marker: true}, Payload: []byte{0x00}}
	tr.deliver(bad, rtp.Update{Gap: 3}, time.Unix(1, 0), func(audiostream.Frame) {})

	// The SSRC-reset path clears the pending gap.
	tr.resetDepacketizer(true)

	good := rtp.Packet{Header: rtp.Header{Timestamp: 0, Marker: true}, Payload: buildAUHeaders([]byte{0x11, 0x22})}
	var got audiostream.Frame
	tr.deliver(good, rtp.Update{Timestamp: 0, Gap: 0}, time.Unix(1, 0), func(f audiostream.Frame) { got = copyFrame(&f) })
	if got.SeqGap != 0 {
		t.Errorf("SeqGap = %d, want 0 (a gap from the old source must not carry across an SSRC reset)", got.SeqGap)
	}
}

// TestDeliverAACNilOnFrameDrainsPendingGap proves the pending gap drains even
// with no registered callback, so the counter cannot grow unbounded: a fragment
// buffered under a nil OnFrame, completed under a nil OnFrame, must leave a
// later frame reporting SeqGap 0 rather than a stale accumulated gap.
func TestDeliverAACNilOnFrameDrainsPendingGap(t *testing.T) {
	t.Parallel()
	tr := &track{id: 0, kind: deliverAAC, clockRate: 16000, aac: newAACDepacketizer(t)}
	tr.baseSet.Store(true)

	// Fragment start and completion, both with a nil callback. The gap of 4 is
	// folded on the fragment start and drained on the (nil-callback) completion.
	frag1 := rtp.Packet{Header: rtp.Header{Timestamp: 0}, Payload: buildAUFragment(6, []byte{1, 2, 3})}
	tr.deliver(frag1, rtp.Update{Timestamp: 0, Gap: 4}, time.Unix(1, 0), nil)
	frag2 := rtp.Packet{Header: rtp.Header{Timestamp: 0, Marker: true}, Payload: buildAUFragment(6, []byte{4, 5, 6})}
	tr.deliver(frag2, rtp.Update{Timestamp: 0, Gap: 0}, time.Unix(1, 0), nil)

	// A following complete AU with a registered callback must report SeqGap 0:
	// the earlier gap was drained under the nil callback, not accumulated.
	good := rtp.Packet{Header: rtp.Header{Timestamp: 0, Marker: true}, Payload: buildAUHeaders([]byte{0x11, 0x22})}
	var got audiostream.Frame
	tr.deliver(good, rtp.Update{Timestamp: 0, Gap: 0}, time.Unix(1, 0), func(f audiostream.Frame) { got = copyFrame(&f) })
	if got.SeqGap != 0 {
		t.Errorf("SeqGap = %d, want 0 (the pending gap must drain under a nil callback, not accumulate)", got.SeqGap)
	}
}

// The following tests are the issue #105 regressions: the per-track pendingGap
// accumulator, added for AAC/LATM in PR #104, must also carry a loss stranded
// on a no-frame single-frame packet (a malformed Opus/L16 payload) onto the
// next delivered frame, instead of deliverOne reading up.Gap directly and
// dropping it. They mirror the AAC gap tests above.

// TestDeliverOpusMalformedRetainsGap is the core issue #105 regression for the
// single-frame path: a gap on a malformed (empty) Opus packet must drain onto
// the next delivered frame rather than being lost.
func TestDeliverOpusMalformedRetainsGap(t *testing.T) {
	t.Parallel()
	tr := &track{id: 0, kind: deliverOpus, clockRate: 48000}
	tr.baseSet.Store(true)

	// An empty payload is malformed and delivers no frame, but must retain the
	// pending gap of 2.
	bad := rtp.Packet{Header: rtp.Header{}, Payload: nil}
	n := 0
	tr.deliver(bad, rtp.Update{Gap: 2}, time.Unix(1, 0), func(audiostream.Frame) { n++ })
	if n != 0 || tr.malformed.Load() != 1 {
		t.Fatalf("malformed packet: frames=%d malformed=%d, want 0 and 1", n, tr.malformed.Load())
	}

	// A plain gap (not an SSRC reset) between the two packets must not clear the
	// retained gap.
	tr.resetDepacketizer(false)

	good := rtp.Packet{Header: rtp.Header{Timestamp: 480}, Payload: []byte{0x78, 0x01, 0x02, 0x03}}
	gotGap := -1
	tr.deliver(good, rtp.Update{Timestamp: 480, Gap: 0}, time.Unix(1, 0), func(f audiostream.Frame) { gotGap = f.SeqGap })
	if gotGap != 2 {
		t.Errorf("SeqGap = %d, want 2 (a gap on a malformed Opus packet must drain onto the next frame)", gotGap)
	}
}

// TestDeliverOpusAccumulatesStrandedGaps proves the accumulator sums for the
// single-frame path: two losses stranded on two malformed packets drain
// together onto the next delivered frame.
func TestDeliverOpusAccumulatesStrandedGaps(t *testing.T) {
	t.Parallel()
	tr := &track{id: 0, kind: deliverOpus, clockRate: 48000}
	tr.baseSet.Store(true)

	bad := rtp.Packet{Header: rtp.Header{}, Payload: nil}
	tr.deliver(bad, rtp.Update{Gap: 2}, time.Unix(1, 0), func(audiostream.Frame) {})
	tr.deliver(bad, rtp.Update{Gap: 3}, time.Unix(1, 0), func(audiostream.Frame) {})

	good := rtp.Packet{Header: rtp.Header{Timestamp: 480}, Payload: []byte{0x78, 0x01, 0x02, 0x03}}
	gotGap := -1
	tr.deliver(good, rtp.Update{Timestamp: 480, Gap: 0}, time.Unix(1, 0), func(f audiostream.Frame) { gotGap = f.SeqGap })
	if gotGap != 5 {
		t.Errorf("SeqGap = %d, want 5 (two stranded gaps of 2 and 3 must drain as their sum)", gotGap)
	}
}

// TestDeliverOpusSSRCResetClearsPendingGap locks in that an SSRC reset drops a
// pending gap from the old sequence space on the single-frame path too.
func TestDeliverOpusSSRCResetClearsPendingGap(t *testing.T) {
	t.Parallel()
	tr := &track{id: 0, kind: deliverOpus, clockRate: 48000}
	tr.baseSet.Store(true)

	bad := rtp.Packet{Header: rtp.Header{}, Payload: nil}
	tr.deliver(bad, rtp.Update{Gap: 3}, time.Unix(1, 0), func(audiostream.Frame) {})

	tr.resetDepacketizer(true)

	good := rtp.Packet{Header: rtp.Header{Timestamp: 0}, Payload: []byte{0x78, 0x01, 0x02, 0x03}}
	gotGap := -1
	tr.deliver(good, rtp.Update{Timestamp: 0, Gap: 0}, time.Unix(1, 0), func(f audiostream.Frame) { gotGap = f.SeqGap })
	if gotGap != 0 {
		t.Errorf("SeqGap = %d, want 0 (a gap from the old source must not carry across an SSRC reset)", gotGap)
	}
}

// TestDeliverOpusNilOnFrameDrainsPendingGap proves the pending gap drains even
// with no registered callback for a codec that always reaches deliverOne on a
// valid packet, so the counter cannot grow unbounded across a nil-callback
// stream: deliverOne clears the accumulator before its onFrame==nil check.
func TestDeliverOpusNilOnFrameDrainsPendingGap(t *testing.T) {
	t.Parallel()
	tr := &track{id: 0, kind: deliverOpus, clockRate: 48000}
	tr.baseSet.Store(true)

	// A malformed packet leaves a gap of 4 pending, then a valid packet under a
	// nil callback must drain it (deliverOne clears pendingGap even when it
	// delivers nothing).
	bad := rtp.Packet{Header: rtp.Header{}, Payload: nil}
	tr.deliver(bad, rtp.Update{Gap: 4}, time.Unix(1, 0), nil)
	good := rtp.Packet{Header: rtp.Header{Timestamp: 0}, Payload: []byte{0x78, 0x01, 0x02, 0x03}}
	tr.deliver(good, rtp.Update{Timestamp: 0, Gap: 0}, time.Unix(1, 0), nil)

	gotGap := -1
	tr.deliver(good, rtp.Update{Timestamp: 0, Gap: 0}, time.Unix(1, 0), func(f audiostream.Frame) { gotGap = f.SeqGap })
	if gotGap != 0 {
		t.Errorf("SeqGap = %d, want 0 (the pending gap must drain under a nil callback, not accumulate)", gotGap)
	}
}

// TestDeliverL16MalformedRetainsGap is the L16 counterpart of the Opus
// regression: a gap on a malformed (odd-length) L16 packet must drain onto the
// next delivered frame.
func TestDeliverL16MalformedRetainsGap(t *testing.T) {
	t.Parallel()
	tr := &track{id: 0, kind: deliverL16, clockRate: 48000}
	tr.baseSet.Store(true)

	bad := rtp.Packet{Header: rtp.Header{}, Payload: []byte{0x00, 0x11, 0x22}}
	n := 0
	tr.deliver(bad, rtp.Update{Gap: 2}, time.Unix(1, 0), func(audiostream.Frame) { n++ })
	if n != 0 || tr.malformed.Load() != 1 {
		t.Fatalf("malformed packet: frames=%d malformed=%d, want 0 and 1", n, tr.malformed.Load())
	}

	tr.resetDepacketizer(false)

	good := rtp.Packet{Header: rtp.Header{Timestamp: 0}, Payload: []byte{0x00, 0x11}}
	gotGap := -1
	tr.deliver(good, rtp.Update{Timestamp: 0, Gap: 0}, time.Unix(1, 0), func(f audiostream.Frame) { gotGap = f.SeqGap })
	if gotGap != 2 {
		t.Errorf("SeqGap = %d, want 2 (a gap on a malformed L16 packet must drain onto the next frame)", gotGap)
	}
}

// TestDeliverL16AccumulatesStrandedGaps proves the L16 accumulator sums.
func TestDeliverL16AccumulatesStrandedGaps(t *testing.T) {
	t.Parallel()
	tr := &track{id: 0, kind: deliverL16, clockRate: 48000}
	tr.baseSet.Store(true)

	bad := rtp.Packet{Header: rtp.Header{}, Payload: []byte{0x00, 0x11, 0x22}}
	tr.deliver(bad, rtp.Update{Gap: 2}, time.Unix(1, 0), func(audiostream.Frame) {})
	tr.deliver(bad, rtp.Update{Gap: 3}, time.Unix(1, 0), func(audiostream.Frame) {})

	good := rtp.Packet{Header: rtp.Header{Timestamp: 0}, Payload: []byte{0x00, 0x11}}
	gotGap := -1
	tr.deliver(good, rtp.Update{Timestamp: 0, Gap: 0}, time.Unix(1, 0), func(f audiostream.Frame) { gotGap = f.SeqGap })
	if gotGap != 5 {
		t.Errorf("SeqGap = %d, want 5 (two stranded gaps of 2 and 3 must drain as their sum)", gotGap)
	}
}

// TestDeliverL16SSRCResetClearsPendingGap locks in the SSRC-reset clear for L16.
func TestDeliverL16SSRCResetClearsPendingGap(t *testing.T) {
	t.Parallel()
	tr := &track{id: 0, kind: deliverL16, clockRate: 48000}
	tr.baseSet.Store(true)

	bad := rtp.Packet{Header: rtp.Header{}, Payload: []byte{0x00, 0x11, 0x22}}
	tr.deliver(bad, rtp.Update{Gap: 3}, time.Unix(1, 0), func(audiostream.Frame) {})

	tr.resetDepacketizer(true)

	good := rtp.Packet{Header: rtp.Header{Timestamp: 0}, Payload: []byte{0x00, 0x11}}
	gotGap := -1
	tr.deliver(good, rtp.Update{Timestamp: 0, Gap: 0}, time.Unix(1, 0), func(f audiostream.Frame) { gotGap = f.SeqGap })
	if gotGap != 0 {
		t.Errorf("SeqGap = %d, want 0 (a gap from the old source must not carry across an SSRC reset)", gotGap)
	}
}

// TestDeliverL16NilOnFrameNoLeak pins L16's narrower guarantee: because L16
// returns at its own onFrame==nil check before reaching deliverOne (the
// documented perf skip of the byte-swap), the accumulator is touched only when
// a callback is registered. A nil-callback stream must never accumulate, so a
// later registered frame reports SeqGap 0. This differs from the AAC nil test
// on purpose: folding at the top or leaving the malformed fold unguarded would
// leak a gap here and make this assertion fail.
func TestDeliverL16NilOnFrameNoLeak(t *testing.T) {
	t.Parallel()
	tr := &track{id: 0, kind: deliverL16, clockRate: 48000}
	tr.baseSet.Store(true)

	// Malformed packet under a nil callback: counted, but must not accumulate.
	bad := rtp.Packet{Header: rtp.Header{}, Payload: []byte{0x00, 0x11, 0x22}}
	tr.deliver(bad, rtp.Update{Gap: 4}, time.Unix(1, 0), nil)
	if tr.malformed.Load() != 1 {
		t.Fatalf("malformed = %d, want 1", tr.malformed.Load())
	}
	// Valid packet under a nil callback: returns at the nil check before
	// deliverOne, so it must not accumulate its gap either.
	good := rtp.Packet{Header: rtp.Header{Timestamp: 0}, Payload: []byte{0x00, 0x11}}
	tr.deliver(good, rtp.Update{Timestamp: 0, Gap: 5}, time.Unix(1, 0), nil)

	gotGap := -1
	tr.deliver(good, rtp.Update{Timestamp: 0, Gap: 0}, time.Unix(1, 0), func(f audiostream.Frame) { gotGap = f.SeqGap })
	if gotGap != 0 {
		t.Errorf("SeqGap = %d, want 0 (a nil-callback L16 stream must not accumulate a pending gap)", gotGap)
	}
}

// TestDeliverG711NilOnFrameNoAccumulate pins that G.711's first-line
// onFrame==nil return (the perf skip of the companding expansion) runs before
// the accumulator is touched, so a nil-callback stream never accumulates a gap.
// G.711 has no reachable malformed path for a track built by newTrack (empty
// payloads decode to a zero-length frame; ErrUnknownLaw cannot occur), so this
// nil-callback guard is the behavior-critical case to lock.
func TestDeliverG711NilOnFrameNoAccumulate(t *testing.T) {
	t.Parallel()
	tr := &track{id: 0, kind: deliverG711, clockRate: 8000, law: audiostream.MuLaw}
	tr.baseSet.Store(true)

	// A packet carrying a gap under a nil callback returns before folding.
	pkt := rtp.Packet{Header: rtp.Header{}, Payload: []byte{0x00, 0x7f, 0x80, 0xff}}
	tr.deliver(pkt, rtp.Update{Gap: 4}, time.Unix(1, 0), nil)

	gotGap := -1
	good := rtp.Packet{Header: rtp.Header{Timestamp: 160}, Payload: []byte{0x00, 0x7f, 0x80, 0xff}}
	tr.deliver(good, rtp.Update{Timestamp: 160, Gap: 0}, time.Unix(1, 0), func(f audiostream.Frame) { gotGap = f.SeqGap })
	if gotGap != 0 {
		t.Errorf("SeqGap = %d, want 0 (a nil-callback G.711 stream must not accumulate a pending gap)", gotGap)
	}
}

// TestDeliverRawNilOnFrameDrainsPendingGap proves deliverOne's drain-before-nil
// check for the raw path, which (like Opus) always reaches deliverOne.
func TestDeliverRawNilOnFrameDrainsPendingGap(t *testing.T) {
	t.Parallel()
	tr := &track{id: 0, kind: deliverRaw, clockRate: 48000}
	tr.baseSet.Store(true)

	// Raw never malforms; a packet under a nil callback still drains its own gap
	// through deliverOne, leaving nothing pending for the next frame.
	raw := rtp.Packet{Header: rtp.Header{Timestamp: 0}, Payload: []byte{0x01, 0x02, 0x03}}
	tr.deliver(raw, rtp.Update{Timestamp: 0, Gap: 4}, time.Unix(1, 0), nil)

	gotGap := -1
	tr.deliver(raw, rtp.Update{Timestamp: 0, Gap: 0}, time.Unix(1, 0), func(f audiostream.Frame) { gotGap = f.SeqGap })
	if gotGap != 0 {
		t.Errorf("SeqGap = %d, want 0 (raw delivers every packet, so no gap stays pending)", gotGap)
	}
}

// TestDeliverG711CarriesGap pins the normal-path fold for a VALID G.711 packet:
// a valid packet carrying its own gap must report that gap on its frame, with no
// preceding no-frame packet involved. This guards the fold that deliverG711 does
// past its onFrame==nil return; without it a valid G.711 packet would silently
// lose its own SeqGap and the rest of the suite would stay green.
func TestDeliverG711CarriesGap(t *testing.T) {
	t.Parallel()
	tr := &track{id: 0, kind: deliverG711, clockRate: 8000, law: audiostream.MuLaw}
	tr.baseSet.Store(true)

	pkt := rtp.Packet{Header: rtp.Header{Timestamp: 160}, Payload: []byte{0x00, 0x7f, 0x80, 0xff}}
	gotGap := -1
	tr.deliver(pkt, rtp.Update{Timestamp: 160, Gap: 3}, time.Unix(1, 0), func(f audiostream.Frame) { gotGap = f.SeqGap })
	if gotGap != 3 {
		t.Errorf("SeqGap = %d, want 3 (a valid G.711 packet must carry its own gap onto its frame)", gotGap)
	}
}

// TestDeliverL16CarriesGap pins the normal-path fold for a VALID L16 packet, the
// counterpart of TestDeliverG711CarriesGap: it guards the fold deliverL16 does on
// the valid branch past its onFrame==nil return.
func TestDeliverL16CarriesGap(t *testing.T) {
	t.Parallel()
	tr := &track{id: 0, kind: deliverL16, clockRate: 48000}
	tr.baseSet.Store(true)

	pkt := rtp.Packet{Header: rtp.Header{Timestamp: 0}, Payload: []byte{0x00, 0x11}}
	gotGap := -1
	tr.deliver(pkt, rtp.Update{Timestamp: 0, Gap: 3}, time.Unix(1, 0), func(f audiostream.Frame) { gotGap = f.SeqGap })
	if gotGap != 3 {
		t.Errorf("SeqGap = %d, want 3 (a valid L16 packet must carry its own gap onto its frame)", gotGap)
	}
}

func TestDeliverNilOnFrameCounts(t *testing.T) {
	t.Parallel()
	tr := &track{id: 0, kind: deliverOpus, clockRate: 48000}
	tr.baseSet.Store(true)
	pkt := rtp.Packet{Header: rtp.Header{}, Payload: []byte{0x78, 0x01}}
	// A nil OnFrame must not panic; it delivers nothing but also does not
	// record a malformed packet.
	tr.deliver(pkt, rtp.Update{}, time.Unix(1, 0), nil)
	if tr.malformed.Load() != 0 {
		t.Errorf("malformed = %d, want 0", tr.malformed.Load())
	}
}

func TestSeededOrigin(t *testing.T) {
	t.Parallel()
	c := &Client{}
	// No recorded seed: the first-packet timestamp is the baseline.
	if got := c.seededOrigin(&track{}, 500); got != 500 {
		t.Errorf("seededOrigin without a seed = %d, want 500", got)
	}
	// A plausible seed (seed <= firstTS) seeds the origin.
	plausible := &track{}
	plausible.seed.Store(200)
	plausible.hasSeed.Store(true)
	if got := c.seededOrigin(plausible, 500); got != 200 {
		t.Errorf("seededOrigin with a plausible seed = %d, want 200", got)
	}
	// An implausible seed (seed > firstTS) is discarded; no negative PTS.
	implausible := &track{}
	implausible.seed.Store(800)
	implausible.hasSeed.Store(true)
	if got := c.seededOrigin(implausible, 500); got != 500 {
		t.Errorf("seededOrigin with an implausible seed = %d, want 500", got)
	}
	// Once a baseline has been fixed, the seed is permanently ignored, so an
	// SSRC-reset re-baseline uses the first-packet timestamp even when the
	// recorded seed would otherwise be plausible (decision 6).
	afterReset := &track{baselineFixed: true}
	afterReset.seed.Store(200)
	afterReset.hasSeed.Store(true)
	if got := c.seededOrigin(afterReset, 500); got != 500 {
		t.Errorf("seededOrigin after a fixed baseline = %d, want 500 (stale seed must not re-apply)", got)
	}
}

func TestParseRTPInfoEntry(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		entry   string
		wantURL string
		wantSeq uint64
		wantTS  uint64
		wantOK  bool
	}{
		{name: "full", entry: "url=rtsp://x/audio;seq=1000;rtptime=123456", wantURL: "rtsp://x/audio", wantSeq: 1000, wantTS: 123456, wantOK: true},
		{name: "spaces", entry: " url=rtsp://x/a ;seq=1;rtptime=2 ", wantURL: "rtsp://x/a", wantSeq: 1, wantTS: 2, wantOK: true},
		{name: "missing rtptime", entry: "url=rtsp://x/a;seq=1", wantOK: false},
		{name: "missing seq", entry: "url=rtsp://x/a;rtptime=2", wantOK: false},
		{name: "garbage rtptime", entry: "url=rtsp://x/a;seq=1;rtptime=abc", wantOK: false},
		{name: "negative rtptime", entry: "url=rtsp://x/a;seq=1;rtptime=-5", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			url, seq, ts, ok := parseRTPInfoEntry(tc.entry)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if url != tc.wantURL || seq != tc.wantSeq || ts != tc.wantTS {
				t.Errorf("got (%q, %d, %d), want (%q, %d, %d)", url, seq, ts, tc.wantURL, tc.wantSeq, tc.wantTS)
			}
		})
	}
}

func TestMatchTrack(t *testing.T) {
	t.Parallel()
	tr0 := &track{id: 0, control: "rtsp://cam/stream/audio"}
	tr1 := &track{id: 1, control: "rtsp://cam/stream/video"}
	tracks := []*track{tr0, tr1}
	base := "rtsp://cam/stream/"

	if got := matchTrack(tracks, base, "rtsp://cam/stream/video", 0); got != tr1 {
		t.Errorf("absolute-url match = %v, want tr1", got)
	}
	// A relative control resolves against the base before comparison.
	if got := matchTrack(tracks, base, "video", 0); got != tr1 {
		t.Errorf("relative-url match = %v, want tr1", got)
	}
	// A url naming a track this client did not set up yields NO match. Falling
	// back positionally there would seed this entry's origin onto an unrelated
	// track, which is far worse than not seeding it at all.
	if got := matchTrack(tracks, base, "rtsp://cam/stream/other", 0); got != nil {
		t.Errorf("unmatched url = %v, want nil rather than the positional track", got)
	}
	// An absent url uses positional order directly.
	if got := matchTrack(tracks, base, "", 1); got != tr1 {
		t.Errorf("empty-url positional = %v, want tr1", got)
	}
	// A positional index past the end yields no track.
	if got := matchTrack(tracks, base, "", 5); got != nil {
		t.Errorf("out-of-range positional = %v, want nil", got)
	}
}

func TestZeroAllocSteadyStateDelivery(t *testing.T) {
	now := time.Unix(1, 0)
	noop := func(audiostream.Frame) {}

	aacTr := &track{id: 0, kind: deliverAAC, clockRate: 16000, aac: newAACDepacketizer(t)}
	aacTr.baseSet.Store(true)
	aacPkt := rtp.Packet{Header: rtp.Header{Timestamp: 0, Marker: true}, Payload: buildAUHeaders([]byte{1, 2, 3, 4})}

	opusTr := &track{id: 1, kind: deliverOpus, clockRate: 48000}
	opusTr.baseSet.Store(true)
	opusPkt := rtp.Packet{Header: rtp.Header{Timestamp: 0}, Payload: []byte{0x78, 0x01, 0x02, 0x03}}

	g711Tr := &track{id: 2, kind: deliverG711, clockRate: 8000, law: audiostream.MuLaw}
	g711Tr.baseSet.Store(true)
	g711Pkt := rtp.Packet{Header: rtp.Header{Timestamp: 0}, Payload: make([]byte, 160)}

	rawTr := &track{id: 3, kind: deliverRaw, clockRate: 90000}
	rawTr.baseSet.Store(true)
	rawPkt := rtp.Packet{Header: rtp.Header{Timestamp: 0}, Payload: []byte{0xaa, 0xbb, 0xcc}}

	l16Tr := &track{id: 4, kind: deliverL16, clockRate: 48000}
	l16Tr.baseSet.Store(true)
	l16Pkt := rtp.Packet{Header: rtp.Header{Timestamp: 0}, Payload: make([]byte, 320)}

	cases := []struct {
		name string
		tr   *track
		pkt  rtp.Packet
		// maxAlloc is the permitted allocations per frame, and it is zero for
		// every path. The AAC depacketizer reuses its AU-header and AU slices
		// across calls (its own benchmarks report 0 allocs/op), so the tolerance
		// of 1 this table used to carry for AAC was slack that would have
		// absorbed a real regression rather than a cost anything actually paid.
		maxAlloc float64
	}{
		{name: "aac", tr: aacTr, pkt: aacPkt, maxAlloc: 0},
		{name: "opus", tr: opusTr, pkt: opusPkt, maxAlloc: 0},
		{name: "g711", tr: g711Tr, pkt: g711Pkt, maxAlloc: 0},
		{name: "raw", tr: rawTr, pkt: rawPkt, maxAlloc: 0},
		{name: "l16", tr: l16Tr, pkt: l16Pkt, maxAlloc: 0},
	}
	for _, tc := range cases {
		tc.tr.deliver(tc.pkt, rtp.Update{Timestamp: 0}, now, noop) // pre-warm reusable buffers
		got := testing.AllocsPerRun(200, func() {
			tc.tr.deliver(tc.pkt, rtp.Update{Timestamp: 0}, now, noop)
		})
		if got > tc.maxAlloc {
			t.Errorf("%s: steady-state deliver allocates %v per frame, want <= %v", tc.name, got, tc.maxAlloc)
		}
	}
}

func TestDiscardTrackDropsAllocationFree(t *testing.T) {
	tr := &track{id: 5, discard: true}
	c := &Client{}
	c.channels.Store(newChannelTable(nil, tr, 6, 7))
	// A valid RTP packet carrying two CSRCs. A full rtp.ParsePacket would
	// allocate a CSRC slice for this; the discard path must not, and an
	// all-zero payload would not exercise that branch at all (it fails the
	// version check first).
	payload := make([]byte, 12+2*4+40)
	payload[0] = 0x82 // version 2, CC = 2
	frame := InterleavedFrame{Channel: 6, Payload: payload}

	c.handleInterleaved(frame) // warm up
	got := testing.AllocsPerRun(200, func() { c.handleInterleaved(frame) })
	if got != 0 {
		t.Errorf("discard drop allocates %v per packet, want 0", got)
	}
	if tr.packets.Load() == 0 {
		t.Error("discard track packet counter did not advance")
	}
	if tr.wireBytes.Load() == 0 {
		t.Error("discard track wire-byte counter did not advance")
	}
	if tr.payloadBytes.Load() != 0 {
		t.Error("discard track payload-byte counter advanced, want 0 (a discard track is never parsed)")
	}
}

// copyFrame deep-copies a Frame's Data so a test may retain it past the
// delivery callback, honoring the Frame.Data ownership contract.
func copyFrame(f *audiostream.Frame) audiostream.Frame {
	cp := *f
	cp.Data = append([]byte(nil), f.Data...)
	return cp
}

// The payload-type rule has two halves and both matter: the declared type
// always wins and settles the track, and an undeclared type is adopted only
// after a consistent run, so a stray packet cannot capture the track.
func TestAcceptPayloadType(t *testing.T) {
	t.Parallel()

	// Ordinary case: the stream matches its SDP, and a second format is
	// rejected from the first packet onward.
	normal := &track{sdpPayloadType: 96}
	if !normal.acceptPayloadType(96, nil) {
		t.Error("the declared payload type was rejected")
	}
	if normal.acceptPayloadType(101, nil) {
		t.Error("a second format was accepted alongside the declared one")
	}

	// The interloper-first case: a session joined mid-DTMF sees a
	// telephone-event before any speech. One stray packet must not capture the
	// track, and the declared type must be delivered as soon as it appears.
	midDTMF := &track{sdpPayloadType: 96}
	if midDTMF.acceptPayloadType(101, nil) {
		t.Error("a stray undeclared packet was accepted immediately")
	}
	if !midDTMF.acceptPayloadType(96, nil) {
		t.Error("the declared payload type was rejected after a stray packet")
	}
	if midDTMF.acceptPayloadType(101, nil) {
		t.Error("the interloper was accepted after the track settled")
	}

	// A camera whose stream consistently disagrees with its own SDP is adopted
	// once the run of the SAME type is long enough to rule out a stray.
	lying := &track{sdpPayloadType: 97}
	for i := range ptAdoptThreshold - 1 {
		if lying.acceptPayloadType(96, nil) {
			t.Fatalf("undeclared packet %d accepted before the adopt threshold", i)
		}
	}
	if !lying.acceptPayloadType(96, nil) {
		t.Error("the stream's payload type was not adopted after a consistent run")
	}
	if !lying.acceptPayloadType(96, nil) {
		t.Error("packet after adoption rejected")
	}
	if lying.acceptPayloadType(101, nil) {
		t.Error("an undeclared third format was accepted after the track settled")
	}
	if got := lying.wirePayloadType; got != 96 {
		t.Errorf("wirePayloadType = %d, want the adopted 96", got)
	}
	// The declared type still wins if it ever turns up.
	if !lying.acceptPayloadType(97, nil) {
		t.Error("the declared payload type was rejected after an adoption")
	}

	// The run must be the SAME undeclared type. A near-complete run of one type
	// broken by a single stray of another does not adopt either: the stray
	// restarts the count, so the packet that finally reaches the threshold is
	// the one actually seen consistently, never whichever happened to land on
	// the boundary.
	mixed := &track{sdpPayloadType: 97}
	for range ptAdoptThreshold - 1 {
		if mixed.acceptPayloadType(96, nil) {
			t.Fatal("undeclared packet accepted before the threshold")
		}
	}
	if mixed.acceptPayloadType(101, nil) {
		t.Error("a stray of a different type was adopted at the run boundary")
	}
	if mixed.wirePTSet {
		t.Error("the track settled on a stray rather than the consistent type")
	}
	// The consistent type still gets there, one full run later.
	for i := range ptAdoptThreshold {
		got := mixed.acceptPayloadType(96, nil)
		if i < ptAdoptThreshold-1 && got {
			t.Fatalf("packet %d of the restarted run accepted early", i)
		}
		if i == ptAdoptThreshold-1 && !got {
			t.Error("the consistent type was not adopted after a full clean run")
		}
	}

	// A run broken by the declared type restarts: the declared type settles the
	// track, so a later undeclared run is rejected outright rather than adopted.
	interrupted := &track{sdpPayloadType: 96}
	if interrupted.acceptPayloadType(101, nil) {
		t.Error("undeclared packet accepted immediately")
	}
	if !interrupted.acceptPayloadType(96, nil) {
		t.Error("declared packet rejected")
	}
	for i := range ptAdoptThreshold + 2 {
		if interrupted.acceptPayloadType(101, nil) {
			t.Fatalf("undeclared packet %d adopted after the track had settled", i)
		}
	}

	// A track with no declared payload type has nothing to prefer, so the first
	// type seen settles it immediately.
	unknown := &track{sdpPayloadType: payloadTypeUnknown}
	if !unknown.acceptPayloadType(0, nil) {
		t.Error("first packet rejected for a track with no declared PT")
	}
	if unknown.acceptPayloadType(8, nil) {
		t.Error("a second format was accepted after the track settled")
	}

	// The zero value declares PT 0, which no path other than newTrack should
	// produce. It is not silently permanent: the adopt threshold heals it after
	// a short run rather than rejecting every packet for the session.
	var zero track
	for range ptAdoptThreshold - 1 {
		_ = zero.acceptPayloadType(96, nil)
	}
	if !zero.acceptPayloadType(96, nil) {
		t.Error("a zero-value track never healed; it would filter to PT 0 forever")
	}
}

// A timestamp below the baseline (a reordered packet, or an AU offset applied
// to one) clamps to zero rather than wrapping the unsigned subtraction into an
// enormous PTS, and a timestamp far above it clamps to what a Duration holds
// rather than wrapping negative.
func TestPtsOfClamps(t *testing.T) {
	t.Parallel()
	tr := &track{clockRate: 16000, baseTS: 1000}
	if got := tr.ptsOf(999); got != 0 {
		t.Errorf("ptsOf(below baseline) = %v, want 0", got)
	}
	// A sender may advance the RTP timestamp by up to 2^31 per packet, so the
	// unwrapped 64-bit value can reach a delta whose seconds term overflows an
	// int64 nanosecond count.
	huge := tr.ptsOf(^uint64(0))
	if huge < 0 {
		t.Errorf("ptsOf(max timestamp) = %v, want a clamp rather than a negative wrap", huge)
	}
	if want := time.Duration(maxPTSSeconds) * time.Second; huge != want {
		t.Errorf("ptsOf(max timestamp) = %v, want the clamp %v", huge, want)
	}
}

// The RFC 3550 report block carries cumulative loss as a SIGNED 24-bit field,
// so the clamp is 0x7FFFFF (Appendix A.3), not 0xFFFFFF. Saturating at the
// unsigned maximum would put -1 on the wire: one packet MORE than expected, the
// opposite of the very lossy stream being reported.
func TestCumulativeLostSaturatesAtTheSignedMaximum(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   uint64
		want uint32
	}{
		{in: 0, want: 0},
		{in: 1234, want: 1234},
		{in: 0x7FFFFF, want: 0x7FFFFF},
		{in: 0x800000, want: 0x7FFFFF},
		{in: 0xFFFFFF, want: 0x7FFFFF},
		{in: ^uint64(0), want: 0x7FFFFF},
	}
	for _, tc := range cases {
		got := cumulativeLost(tc.in)
		if got != tc.want {
			t.Errorf("cumulativeLost(%d) = %#x, want %#x", tc.in, got, tc.want)
		}
		// Whatever the input, the value must stay non-negative once the wire
		// truncates it to 24 bits and a receiver sign-extends it.
		if signed := int32(got<<8) >> 8; signed < 0 {
			t.Errorf("cumulativeLost(%d) = %#x, which decodes as %d in a signed 24-bit field", tc.in, got, signed)
		}
	}
}

// TestNewTrackG726PackingOverride covers SetupOptions.G726Packing: the caller's
// override must decide the codeword bit order the track's decoder uses,
// overruling whatever the rtpmap encoding name resolved to, and the zero value
// must leave the SDP in charge.
//
// This is the escape hatch for a camera that advertises one packing and sends
// the other. Decoding with the wrong order cannot fail (the two carry the same
// codewords in reversed bit numbering), so the assertion is that the delivered
// PCM matches the reference decoder for the EXPECTED packing and differs from
// the other one.
func TestNewTrackG726PackingOverride(t *testing.T) {
	t.Parallel()
	payload := []byte{0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde, 0xf0}

	decodeWith := func(t *testing.T, p audiostream.G726Packing) []byte {
		t.Helper()
		ref, err := g726.New(audiostream.G726Rate32, p)
		if err != nil {
			t.Fatalf("New(%v): %v", p, err)
		}
		out, err := ref.DecodeAlloc(payload)
		if err != nil {
			t.Fatalf("DecodeAlloc(%v): %v", p, err)
		}
		return out
	}
	wantPlain := decodeWith(t, audiostream.G726PackingRFC3551)
	wantAAL2 := decodeWith(t, audiostream.G726PackingAAL2)
	// Precondition: the payload must distinguish the two packings, else every
	// assertion below would pass regardless of which decoder was built.
	if bytes.Equal(wantPlain, wantAAL2) {
		t.Fatal("test payload does not distinguish the two codeword packings")
	}

	for _, tc := range []struct {
		name     string
		sdp      audiostream.G726Packing
		override G726PackingOverride
		want     []byte
	}{
		{"fromSDP/plain", audiostream.G726PackingRFC3551, G726PackingFromSDP, wantPlain},
		{"fromSDP/aal2", audiostream.G726PackingAAL2, G726PackingFromSDP, wantAAL2},
		// The two that matter: the override contradicts the SDP.
		{"override/aal2SDPForcedPlain", audiostream.G726PackingAAL2, G726PackingForceRFC3551, wantPlain},
		{"override/plainSDPForcedAAL2", audiostream.G726PackingRFC3551, G726PackingForceAAL2, wantAAL2},
		// Redundant overrides agreeing with the SDP must be inert.
		{"override/plainSDPForcedPlain", audiostream.G726PackingRFC3551, G726PackingForceRFC3551, wantPlain},
		{"override/aal2SDPForcedAAL2", audiostream.G726PackingAAL2, G726PackingForceAAL2, wantAAL2},
		// An out-of-range value must fall back to the SDP, never fail Setup.
		{"override/outOfRange", audiostream.G726PackingAAL2, G726PackingOverride(99), wantAAL2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			desc := describedTrack{
				codec: audiostream.CodecG726{
					BitRate:   audiostream.G726Rate32,
					Packing:   tc.sdp,
					ClockRate: 8000,
					Channels:  1,
				},
				clockRate: 8000,
				media:     audiostream.MediaAudio,
			}
			tr := newTrack(0, desc, SetupOptions{G726Packing: tc.override}, 1, nil)
			if tr.kind != deliverG726 {
				t.Fatalf("kind = %d, want deliverG726", tr.kind)
			}
			tr.baseSet.Store(true)

			var got audiostream.Frame
			pkt := rtp.Packet{Header: rtp.Header{Timestamp: 160}, Payload: payload}
			tr.deliver(pkt, rtp.Update{Timestamp: 160}, time.Unix(1, 0), func(f audiostream.Frame) { got = copyFrame(&f) })
			if !bytes.Equal(got.Data, tc.want) {
				t.Errorf("Data = % x, want % x", got.Data, tc.want)
			}
		})
	}
}

// TestG726PackingOverrideResolution covers the override resolver directly,
// including that it never invents a packing for a value it does not know.
func TestG726PackingOverrideResolution(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		override G726PackingOverride
		fromSDP  audiostream.G726Packing
		want     audiostream.G726Packing
		wantOK   bool
	}{
		{"fromSDP keeps plain", G726PackingFromSDP, audiostream.G726PackingRFC3551, audiostream.G726PackingRFC3551, true},
		{"fromSDP keeps aal2", G726PackingFromSDP, audiostream.G726PackingAAL2, audiostream.G726PackingAAL2, true},
		{"force plain over aal2", G726PackingForceRFC3551, audiostream.G726PackingAAL2, audiostream.G726PackingRFC3551, true},
		{"force aal2 over plain", G726PackingForceAAL2, audiostream.G726PackingRFC3551, audiostream.G726PackingAAL2, true},
		{"unknown falls back and reports not ok", G726PackingOverride(200), audiostream.G726PackingAAL2, audiostream.G726PackingAAL2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := tc.override.resolve(tc.fromSDP)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("resolve(%v) = (%v, %t), want (%v, %t)", tc.fromSDP, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestConfigureG726OutOfRangeWarns pins the diagnostic for an out-of-range
// SetupOptions.G726Packing value. Such a value resolves to the SDP packing
// (fail-open, so it never fails Setup), which on its own reproduces exactly the
// silent-wrong-audio failure the option exists to escape. configureG726 must
// warn that the value is out of range, and the decoder must still use the SDP
// packing.
func TestConfigureG726OutOfRangeWarns(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	payload := []byte{0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde, 0xf0}
	desc := describedTrack{
		codec: audiostream.CodecG726{
			BitRate:   audiostream.G726Rate32,
			Packing:   audiostream.G726PackingAAL2,
			ClockRate: 8000,
			Channels:  1,
		},
		clockRate: 8000,
		media:     audiostream.MediaAudio,
	}
	tr := newTrack(0, desc, SetupOptions{G726Packing: G726PackingOverride(99)}, 1, logger)
	if tr.kind != deliverG726 {
		t.Fatalf("kind = %d, want deliverG726 (an out-of-range override must not fail Setup)", tr.kind)
	}

	// The out-of-range value must produce a diagnostic rather than resolve
	// silently to the SDP packing.
	if logged := buf.String(); !strings.Contains(logged, "out of range") {
		t.Errorf("no out-of-range warning logged; got %q", logged)
	}

	// And the SDP packing (AAL2 here) must still be what the decoder uses: an
	// out-of-range override is fail-open, not a switch to some other order.
	tr.baseSet.Store(true)
	var got audiostream.Frame
	pkt := rtp.Packet{Header: rtp.Header{Timestamp: 160}, Payload: payload}
	tr.deliver(pkt, rtp.Update{Timestamp: 160}, time.Unix(1, 0), func(f audiostream.Frame) { got = copyFrame(&f) })

	ref, err := g726.New(audiostream.G726Rate32, audiostream.G726PackingAAL2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	wantAAL2, err := ref.DecodeAlloc(payload)
	if err != nil {
		t.Fatalf("DecodeAlloc: %v", err)
	}
	if !bytes.Equal(got.Data, wantAAL2) {
		t.Errorf("Data = % x, want SDP (AAL2) packing % x", got.Data, wantAAL2)
	}
}
