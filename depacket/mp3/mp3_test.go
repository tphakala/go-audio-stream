package mp3

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	mpahdr "github.com/tphakala/go-audio-stream/internal/mp3"
)

// ts is an arbitrary RTP timestamp for a frame; fragments of one frame share it.
const ts = uint32(90000)

// hdrMP1L3 is a valid MPEG-1 Layer III header: 128 kbps, 44100 Hz, stereo, no
// padding. Its frame length is 144*128000/44100 = 417 bytes and it decodes to
// 1152 samples per channel.
var hdrMP1L3 = [4]byte{0xFF, 0xFB, 0x90, 0x00}

// mp1l3FrameLen and mp1l3Samples are hdrMP1L3's geometry, verified against the
// header parser so the test vectors and the depacketizer agree on the numbers.
const (
	mp1l3FrameLen = 417
	mp1l3Samples  = 1152
	mp1l3Rate     = 44100
)

func init() {
	h, err := mpahdr.Parse(binary.BigEndian.Uint32(hdrMP1L3[:]))
	if err != nil {
		panic("test header hdrMP1L3 is not valid: " + err.Error())
	}
	if h.FrameLen != mp1l3FrameLen || h.SamplesPerFrame != mp1l3Samples || h.SampleRate != mp1l3Rate {
		panic("test header geometry drifted from the parser")
	}
}

// frame builds a whole MPEG audio frame from a header: the 4 header bytes
// followed by filler up to the header's frame length. The depacketizer frames by
// the header alone and never inspects the body, so the filler content is
// irrelevant; it is made non-zero and position-dependent so a mis-sliced frame
// is easy to spot in a failure.
func frame(hdr [4]byte) []byte {
	h, err := mpahdr.Parse(binary.BigEndian.Uint32(hdr[:]))
	if err != nil {
		panic("frame: invalid header")
	}
	b := make([]byte, h.FrameLen)
	copy(b, hdr[:])
	for i := mpahdr.HeaderLen; i < len(b); i++ {
		b[i] = byte(i)
	}
	return b
}

// mpaPayload prepends the 4-byte RFC 2250 MPEG audio-specific header (MBZ + the
// given fragmentation offset) to data.
func mpaPayload(fragOffset uint16, data []byte) []byte {
	p := make([]byte, headerLen+len(data))
	binary.BigEndian.PutUint16(p[2:4], fragOffset)
	copy(p[headerLen:], data)
	return p
}

// A single whole frame in one packet is returned aliased, with RTP offset 0.
func TestSingleFrameAliasesPayload(t *testing.T) {
	d := New(0) // defaults to 90 kHz
	f := frame(hdrMP1L3)
	payload := mpaPayload(0, f)
	got, err := d.Depacketize(payload, ts)
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d frames, want 1", len(got))
	}
	if !bytes.Equal(got[0].Data, f) {
		t.Fatalf("frame = %x, want %x", got[0].Data, f)
	}
	if got[0].RTPOffset != 0 {
		t.Errorf("RTPOffset = %d, want 0", got[0].RTPOffset)
	}
	// The frame must alias the payload (offset headerLen into it), not be copied.
	if &got[0].Data[0] != &payload[headerLen] {
		t.Error("single frame should alias the payload, not copy it")
	}
}

// A packet aggregating several whole frames returns them all, spaced by their
// durations scaled to the RTP clock.
func TestAggregatedFramesRTPOffsets(t *testing.T) {
	d := New(90000)
	f := frame(hdrMP1L3)
	payload := mpaPayload(0, bytes.Join([][]byte{f, f, f}, nil))
	got, err := d.Depacketize(payload, ts)
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d frames, want 3", len(got))
	}
	// perFrame ticks = 1152 * 90000 / 44100 = 2351, pinned as a literal so a
	// mis-ordered production formula that happens to match the test's own
	// re-derivation cannot slip through.
	const perFrame uint32 = 2351
	for i, want := range []uint32{0, perFrame, 2 * perFrame} {
		if got[i].RTPOffset != want {
			t.Errorf("frame %d RTPOffset = %d, want %d", i, got[i].RTPOffset, want)
		}
		if !bytes.Equal(got[i].Data, f) {
			t.Errorf("frame %d data mismatch", i)
		}
	}
}

// A sender that clocks its RTP timestamps at the audio sample rate (a
// non-conformant but real case) advances aggregated frames by the raw sample
// count.
func TestAggregatedFramesAudioRateClock(t *testing.T) {
	d := New(mp1l3Rate)
	f := frame(hdrMP1L3)
	payload := mpaPayload(0, bytes.Join([][]byte{f, f}, nil))
	got, err := d.Depacketize(payload, ts)
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d frames, want 2", len(got))
	}
	if got[1].RTPOffset != mp1l3Samples {
		t.Errorf("second frame RTPOffset = %d, want %d", got[1].RTPOffset, mp1l3Samples)
	}
}

// A frame larger than one packet is reassembled across fragments and completes
// when the accumulated bytes reach the frame length its header declared.
func TestFragmentedReassembly(t *testing.T) {
	d := New(90000)
	f := frame(hdrMP1L3)
	const split1, split2 = 200, 400

	if got, err := d.Depacketize(mpaPayload(0, f[:split1]), ts); len(got) != 0 || err != nil {
		t.Fatalf("first fragment: got %d frames, %v; want 0, nil (buffering)", len(got), err)
	}
	if got, err := d.Depacketize(mpaPayload(split1, f[split1:split2]), ts); len(got) != 0 || err != nil {
		t.Fatalf("middle fragment: got %d frames, %v; want 0, nil (buffering)", len(got), err)
	}
	got, err := d.Depacketize(mpaPayload(split2, f[split2:]), ts)
	if err != nil {
		t.Fatalf("final fragment: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d frames, want 1", len(got))
	}
	if !bytes.Equal(got[0].Data, f) {
		t.Fatalf("reassembled = %x..., want %x...", got[0].Data[:8], f[:8])
	}
	if got[0].RTPOffset != 0 {
		t.Errorf("reassembled RTPOffset = %d, want 0", got[0].RTPOffset)
	}
}

// After completing a fragmented frame the depacketizer is clean, so the next
// boundary packet is delivered whole rather than appended to the last frame.
func TestReassemblyResetsAfterCompletion(t *testing.T) {
	d := New(90000)
	f := frame(hdrMP1L3)
	_, _ = d.Depacketize(mpaPayload(0, f[:200]), ts)
	if _, err := d.Depacketize(mpaPayload(200, f[200:]), ts); err != nil {
		t.Fatalf("complete first frame: %v", err)
	}
	got, err := d.Depacketize(mpaPayload(0, f), ts+2351)
	if err != nil {
		t.Fatalf("next whole frame: %v", err)
	}
	if len(got) != 1 || !bytes.Equal(got[0].Data, f) {
		t.Fatalf("next frame not delivered cleanly: %d frames", len(got))
	}
}

// A boundary packet arriving while a fragment is still in progress means the tail
// of the earlier frame was lost. The stale partial is dropped and the new
// boundary packet is processed as a fresh frame.
func TestLostTailDropsPartial(t *testing.T) {
	d := New(90000)
	f := frame(hdrMP1L3)
	if got, err := d.Depacketize(mpaPayload(0, f[:200]), ts); len(got) != 0 || err != nil {
		t.Fatalf("first fragment: got %d, %v", len(got), err)
	}
	// A new frame boundary arrives before the old frame finished.
	got, err := d.Depacketize(mpaPayload(0, f), ts+2351)
	if err != nil {
		t.Fatalf("new boundary after lost tail: %v", err)
	}
	if len(got) != 1 || !bytes.Equal(got[0].Data, f) {
		t.Fatalf("new frame not delivered after dropping partial: %d frames", len(got))
	}
}

func TestContinuationErrors(t *testing.T) {
	f := frame(hdrMP1L3)
	tests := []struct {
		name  string
		setup func(d *Depacketizer)
		pkt   []byte
		time  uint32
		want  error
	}{
		{
			name: "orphan continuation with no active fragment",
			pkt:  mpaPayload(100, f[100:200]),
			time: ts,
			want: ErrOrphanFragment,
		},
		{
			name:  "continuation timestamp mismatch",
			setup: func(d *Depacketizer) { _, _ = d.Depacketize(mpaPayload(0, f[:200]), ts) },
			pkt:   mpaPayload(200, f[200:]),
			time:  ts + 1,
			want:  ErrOrphanFragment,
		},
		{
			name:  "continuation offset does not continue the frame",
			setup: func(d *Depacketizer) { _, _ = d.Depacketize(mpaPayload(0, f[:200]), ts) },
			pkt:   mpaPayload(150, f[150:]),
			time:  ts,
			want:  ErrOrphanFragment,
		},
		{
			name:  "reassembly overflow past declared length",
			setup: func(d *Depacketizer) { _, _ = d.Depacketize(mpaPayload(0, f[:200]), ts) },
			pkt:   mpaPayload(200, make([]byte, mp1l3FrameLen)), // 200+417 > 417
			time:  ts,
			want:  ErrFrameOverflow,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := New(90000)
			if tc.setup != nil {
				tc.setup(d)
			}
			got, err := d.Depacketize(tc.pkt, tc.time)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if len(got) != 0 {
				t.Errorf("got %d frames, want 0 on error", len(got))
			}
		})
	}
}

func TestBoundaryErrors(t *testing.T) {
	tests := []struct {
		name string
		pkt  []byte
		want error
	}{
		{"empty payload", nil, ErrShortHeader},
		{"payload shorter than the 4-byte header", []byte{0x00, 0x00, 0x00}, ErrShortHeader},
		{"boundary payload with no valid header", mpaPayload(0, []byte{0x01, 0x02, 0x03, 0x04, 0x05}), ErrInvalidFrame},
		{"boundary payload too short for a header", mpaPayload(0, []byte{0xFF, 0xFB}), ErrInvalidFrame},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := New(90000)
			got, err := d.Depacketize(tc.pkt, ts)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if len(got) != 0 {
				t.Errorf("got %d frames, want 0 on error", len(got))
			}
		})
	}
}

// A short-header packet arriving mid-reassembly abandons the partial, so a later
// in-order continuation cannot splice onto it.
func TestShortHeaderMidReassemblyDropsPartial(t *testing.T) {
	d := New(90000)
	f := frame(hdrMP1L3)
	_, _ = d.Depacketize(mpaPayload(0, f[:200]), ts)
	if _, err := d.Depacketize([]byte{0x00}, ts); !errors.Is(err, ErrShortHeader) {
		t.Fatalf("short header: err = %v, want ErrShortHeader", err)
	}
	// The partial was dropped, so the continuation that would have completed it is
	// now an orphan.
	if _, err := d.Depacketize(mpaPayload(200, f[200:]), ts); !errors.Is(err, ErrOrphanFragment) {
		t.Fatalf("after short-header reset: err = %v, want ErrOrphanFragment", err)
	}
}

// Reset discards a partial reassembly, so a following continuation is an orphan.
func TestResetDiscardsPartial(t *testing.T) {
	d := New(90000)
	f := frame(hdrMP1L3)
	_, _ = d.Depacketize(mpaPayload(0, f[:200]), ts)
	d.Reset()
	if _, err := d.Depacketize(mpaPayload(200, f[200:]), ts); !errors.Is(err, ErrOrphanFragment) {
		t.Fatalf("after Reset: err = %v, want ErrOrphanFragment", err)
	}
}

// The MBZ field is ignored: a non-zero MBZ from a non-conformant sender still
// yields the frame.
func TestMBZIgnored(t *testing.T) {
	d := New(90000)
	f := frame(hdrMP1L3)
	payload := mpaPayload(0, f)
	binary.BigEndian.PutUint16(payload[0:2], 0xFFFF) // set MBZ
	got, err := d.Depacketize(payload, ts)
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}
	if len(got) != 1 || !bytes.Equal(got[0].Data, f) {
		t.Fatalf("non-zero MBZ was not tolerated: %d frames", len(got))
	}
}

// After at least one whole frame in an aggregating packet, trailing bytes that
// are not another whole frame are discarded (RFC 2250 aggregates integral frames
// only): a stray tail shorter than a header, junk that fails to parse as a
// header, and a valid header whose frame runs past the payload all yield only the
// whole frame already parsed, never an error and never a partial reassembly.
func TestAggregationDiscardsTrailingBytes(t *testing.T) {
	f := frame(hdrMP1L3)
	tests := []struct {
		name  string
		trail []byte
	}{
		{"tail shorter than a header", []byte{0xAA, 0xBB}},
		{"junk that fails header parse", []byte{0x00, 0x00, 0x00, 0x00}},
		{"valid header but frame exceeds the payload", f[:100]}, // header ok, FrameLen 417 > 100
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := New(90000)
			payload := mpaPayload(0, append(bytes.Clone(f), tc.trail...))
			got, err := d.Depacketize(payload, ts)
			if err != nil {
				t.Fatalf("Depacketize: %v", err)
			}
			if len(got) != 1 || !bytes.Equal(got[0].Data, f) {
				t.Fatalf("got %d frames, want exactly the one whole frame", len(got))
			}
			// A trailing partial must not have started a reassembly.
			if _, err := d.Depacketize(mpaPayload(100, f[100:]), ts); !errors.Is(err, ErrOrphanFragment) {
				t.Errorf("a continuation was accepted, so the trailing bytes wrongly began a reassembly: %v", err)
			}
		})
	}
}

// A pathological SDP clock plus a heavily aggregated packet can push the
// accumulated RTP offset past the 32-bit range. The offset saturates at
// maxRTPOffset instead of wrapping, so per-frame offsets stay non-decreasing (a
// wrap would hand a later frame a smaller offset and a backwards PTS). A real
// stream never reaches the cap.
func TestAggregatedFramesOffsetSaturates(t *testing.T) {
	d := New(math.MaxUint32) // absurd but permitted (clockRateTicks allows up to MaxUint32)
	f := frame(hdrMP1L3)
	const n = 45 // enough frames that a MaxUint32 clock overflows uint32 ticks
	payload := make([]byte, 0, n*len(f))
	for range n {
		payload = append(payload, f...)
	}
	got, err := d.Depacketize(mpaPayload(0, payload), ts)
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}
	if len(got) != n {
		t.Fatalf("got %d frames, want %d", len(got), n)
	}
	var prev uint32
	sawCap := false
	for i, fr := range got {
		if i > 0 && fr.RTPOffset < prev {
			t.Fatalf("frame %d RTPOffset %d < previous %d (offset wrapped)", i, fr.RTPOffset, prev)
		}
		if fr.RTPOffset == math.MaxUint32 {
			sawCap = true
		}
		prev = fr.RTPOffset
	}
	if !sawCap {
		t.Fatal("no frame reached the saturation cap; the test did not exercise the clamp")
	}
}

// The single-frame delivery path allocates nothing after the first call warms
// the reused return slice.
func TestSingleFrameZeroAlloc(t *testing.T) {
	d := New(90000)
	payload := mpaPayload(0, frame(hdrMP1L3))
	// Warm the return slice.
	if _, err := d.Depacketize(payload, ts); err != nil {
		t.Fatalf("warmup: %v", err)
	}
	allocs := testing.AllocsPerRun(100, func() {
		if _, err := d.Depacketize(payload, ts); err != nil {
			t.Fatalf("Depacketize: %v", err)
		}
	})
	if allocs != 0 {
		t.Errorf("Depacketize allocated %.1f times per call, want 0", allocs)
	}
}
