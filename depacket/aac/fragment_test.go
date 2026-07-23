package aac_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/tphakala/go-audio-stream/depacket/aac"
)

func TestFragmentTwoPackets(t *testing.T) {
	t.Parallel()
	d := newDepacketizer(t)
	// Whole AU is 6 bytes, split 4 + 2. Each fragment's header declares
	// the full size 6.
	p1 := buildHBR([][]byte{auHeader16(6, 0)}, [][]byte{{0x11, 0x22, 0x33, 0x44}})
	p2 := buildHBR([][]byte{auHeader16(6, 0)}, [][]byte{{0x55, 0x66}})

	aus, err := d.Depacketize(p1, false, 500) // not last: marker false
	if err != nil {
		t.Fatalf("frag 1: %v", err)
	}
	if aus != nil {
		t.Fatalf("frag 1 returned %d AUs, want none (buffering)", len(aus))
	}
	aus, err = d.Depacketize(p2, true, 500) // last fragment: marker true
	if err != nil {
		t.Fatalf("frag 2: %v", err)
	}
	if len(aus) != 1 {
		t.Fatalf("AU count = %d, want 1", len(aus))
	}
	want := []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66}
	if !bytes.Equal(aus[0].Data, want) {
		t.Errorf("Data = % x, want % x", aus[0].Data, want)
	}
	if aus[0].RTPOffset != 0 {
		t.Errorf("RTPOffset = %d, want 0", aus[0].RTPOffset)
	}
}

func TestFragmentThreePackets(t *testing.T) {
	t.Parallel()
	d := newDepacketizer(t)
	// Whole AU is 9 bytes, split 4 + 3 + 2.
	p1 := buildHBR([][]byte{auHeader16(9, 0)}, [][]byte{{0x11, 0x22, 0x33, 0x44}})
	p2 := buildHBR([][]byte{auHeader16(9, 0)}, [][]byte{{0x55, 0x66, 0x77}})
	p3 := buildHBR([][]byte{auHeader16(9, 0)}, [][]byte{{0x88, 0x99}})

	if aus, err := d.Depacketize(p1, false, 0); err != nil || aus != nil {
		t.Fatalf("frag 1: aus=%v err=%v", aus, err)
	}
	if aus, err := d.Depacketize(p2, false, 0); err != nil || aus != nil {
		t.Fatalf("frag 2: aus=%v err=%v", aus, err)
	}
	aus, err := d.Depacketize(p3, true, 0)
	if err != nil {
		t.Fatalf("frag 3: %v", err)
	}
	want := []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99}
	if len(aus) != 1 || !bytes.Equal(aus[0].Data, want) {
		t.Fatalf("reassembled = % x, want % x", aus, want)
	}
}

func TestFragmentOverflowAccumulated(t *testing.T) {
	t.Parallel()
	d := newDepacketizer(t)
	// First fragment declares total size 6 and delivers 4 bytes; the
	// second delivers 5, pushing accumulated to 9 > 6.
	p1 := buildHBR([][]byte{auHeader16(6, 0)}, [][]byte{{0x11, 0x22, 0x33, 0x44}})
	p2 := buildHBR([][]byte{auHeader16(6, 0)}, [][]byte{{0x55, 0x66, 0x77, 0x88, 0x99}})
	if _, err := d.Depacketize(p1, false, 0); err != nil {
		t.Fatalf("frag 1: %v", err)
	}
	if _, err := d.Depacketize(p2, true, 0); !errors.Is(err, aac.ErrFragmentOverflow) {
		t.Fatalf("err = %v, want ErrFragmentOverflow", err)
	}
	// State must be reset: a following clean packet parses normally.
	pkt := buildHBR([][]byte{auHeader16(2, 0)}, [][]byte{{0xAB, 0xCD}})
	if aus, err := d.Depacketize(pkt, true, 0); err != nil || len(aus) != 1 {
		t.Fatalf("post-reset packet: aus=%v err=%v", aus, err)
	}
}

func TestFragmentShortFinalMarker(t *testing.T) {
	t.Parallel()
	d := newDepacketizer(t)
	// First fragment declares total size 6 and delivers 4 bytes with
	// marker false; the second sets marker=true but delivers only 1 byte
	// (accumulated 5 < 6). A fragment flagged final that is short is an
	// error, and the state must reset so the next packet parses clean.
	p1 := buildHBR([][]byte{auHeader16(6, 0)}, [][]byte{{0x11, 0x22, 0x33, 0x44}})
	p2 := buildHBR([][]byte{auHeader16(6, 0)}, [][]byte{{0x55}})
	if _, err := d.Depacketize(p1, false, 0); err != nil {
		t.Fatalf("frag 1: %v", err)
	}
	if _, err := d.Depacketize(p2, true, 0); !errors.Is(err, aac.ErrAUSizeOverflow) {
		t.Fatalf("err = %v, want ErrAUSizeOverflow", err)
	}
	// Reset must have cleared the stale fragment: a fresh single-AU
	// packet depacketizes cleanly and alone.
	pkt := buildHBR([][]byte{auHeader16(2, 0)}, [][]byte{{0xAB, 0xCD}})
	if aus, err := d.Depacketize(pkt, true, 0); err != nil || len(aus) != 1 {
		t.Fatalf("post-reset packet: aus=%v err=%v", aus, err)
	}
}

func TestFragmentSizeExhaustedNoMarker(t *testing.T) {
	t.Parallel()
	d := newDepacketizer(t)
	// First fragment declares total size 6 and delivers 4 bytes; the
	// second delivers the remaining 2 (accumulated 6 == 6) but leaves
	// marker=false. The declared size is exhausted with no final marker,
	// which is inconsistent: error and reset.
	p1 := buildHBR([][]byte{auHeader16(6, 0)}, [][]byte{{0x11, 0x22, 0x33, 0x44}})
	p2 := buildHBR([][]byte{auHeader16(6, 0)}, [][]byte{{0x55, 0x66}})
	if _, err := d.Depacketize(p1, false, 0); err != nil {
		t.Fatalf("frag 1: %v", err)
	}
	if _, err := d.Depacketize(p2, false, 0); !errors.Is(err, aac.ErrAUSizeOverflow) {
		t.Fatalf("err = %v, want ErrAUSizeOverflow", err)
	}
	// Reset must have cleared the stale fragment: a fresh single-AU
	// packet depacketizes cleanly and alone.
	pkt := buildHBR([][]byte{auHeader16(3, 0)}, [][]byte{{0xDE, 0xAD, 0xBE}})
	if aus, err := d.Depacketize(pkt, true, 0); err != nil || len(aus) != 1 {
		t.Fatalf("post-reset packet: aus=%v err=%v", aus, err)
	}
}

func TestFragmentSizeCap(t *testing.T) {
	t.Parallel()
	// A config with a 31-bit size field can declare an AU larger than
	// MaxFragmentSize; the first fragment is rejected before buffering.
	d, err := aac.New(aac.Config{SizeLength: 31, IndexLength: 3, IndexDeltaLength: 3, SamplesPerFrame: 1024})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Build one 34-bit header (31-bit size + 3-bit index) declaring size
	// MaxFragmentSize+1, with a short data section and marker false.
	size := aac.MaxFragmentSize + 1
	pkt := buildWideHeader(t, 34, size, 0, []byte{0x00, 0x01, 0x02})
	if _, err := d.Depacketize(pkt, false, 0); !errors.Is(err, aac.ErrFragmentOverflow) {
		t.Fatalf("err = %v, want ErrFragmentOverflow", err)
	}
}

// TestWideAUSizeNoPanic feeds a complete single-AU packet whose 31-bit size
// field is at the platform int32 max. The never-panics contract must hold:
// the oversized AU is rejected with ErrAUSizeOverflow, never a slice-bounds
// panic. SizeLength caps at 31 (New's bound) so the decoded size stays
// non-negative even where int is 32 bits.
func TestWideAUSizeNoPanic(t *testing.T) {
	t.Parallel()
	d, err := aac.New(aac.Config{SizeLength: 31, IndexLength: 3, IndexDeltaLength: 3, SamplesPerFrame: 1024})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const nearInt32Max = 1<<31 - 1 // 2147483647, the largest a 31-bit field holds
	pkt := buildWideHeader(t, 34, nearInt32Max, 0, []byte{0x00, 0x01, 0x02})
	if _, err := d.Depacketize(pkt, true, 0); !errors.Is(err, aac.ErrAUSizeOverflow) {
		t.Fatalf("err = %v, want ErrAUSizeOverflow", err)
	}
}

// TestMultiAUTrailingSizeOverflow reaches the complete-packet loop (not the
// fragment-start branch) with a trailing AU whose declared size overruns the
// payload, exercising the overflow-safe size-vs-payload subtraction guard.
func TestMultiAUTrailingSizeOverflow(t *testing.T) {
	t.Parallel()
	d := newDepacketizer(t)
	// Two AUs: the first (size 2) fits, the second (size 8000) runs past
	// the three data bytes present, so the loop rejects it.
	pkt := buildHBR(
		[][]byte{auHeader16(2, 0), auHeader16(8000, 0)},
		[][]byte{{0x01, 0x02}, {0x03}},
	)
	if _, err := d.Depacketize(pkt, true, 0); !errors.Is(err, aac.ErrAUSizeOverflow) {
		t.Fatalf("err = %v, want ErrAUSizeOverflow", err)
	}
}

func TestResetOnDiscontinuity(t *testing.T) {
	t.Parallel()
	d := newDepacketizer(t)
	// Begin a fragment, then simulate a sequence gap: Reset, then a fresh
	// complete packet must not be concatenated with the stale fragment.
	p1 := buildHBR([][]byte{auHeader16(6, 0)}, [][]byte{{0x11, 0x22, 0x33, 0x44}})
	if _, err := d.Depacketize(p1, false, 0); err != nil {
		t.Fatalf("frag start: %v", err)
	}
	d.Reset()
	pkt := buildHBR([][]byte{auHeader16(3, 0)}, [][]byte{{0xDE, 0xAD, 0xBE}})
	aus, err := d.Depacketize(pkt, true, 0)
	if err != nil {
		t.Fatalf("post-reset: %v", err)
	}
	if len(aus) != 1 || !bytes.Equal(aus[0].Data, []byte{0xDE, 0xAD, 0xBE}) {
		t.Fatalf("post-reset AU = %v, want DE AD BE alone", aus)
	}
}

// buildWideHeader assembles a payload with a single AU-header of
// headerBits bits: the high (headerBits-3) bits hold size, the low 3 bits
// hold index. AU-headers-length is headerBits; the header bytes are the
// MSB-first packing padded to a byte boundary; data follows. Used to
// exercise field widths other than the 16-bit AAC-hbr common case. The
// index field is fixed at 3 bits, so headerBits is SizeLength+3.
func buildWideHeader(t *testing.T, headerBits, size, index int, data []byte) []byte {
	t.Helper()
	// A 3-bit index caps SizeLength at 31 (New's bound), so the header is
	// at most 34 bits and acc never overflows the uint64 accumulator.
	if headerBits < 4 || headerBits > 34 {
		t.Fatalf("buildWideHeader supports 4..34-bit headers, got %d", headerBits)
	}
	acc := (uint64(uint32(size)) << 3) | uint64(index&0x07)
	headerBytes := (headerBits + 7) / 8
	hb := make([]byte, headerBytes)
	// Left-justify the headerBits into headerBytes*8 bits, MSB first.
	acc <<= uint(headerBytes*8 - headerBits) // shift left by the pad bits
	for i := 0; i < headerBytes; i++ {
		hb[headerBytes-1-i] = byte(acc >> (8 * uint(i)))
	}
	out := make([]byte, 0, 2+len(hb)+len(data))
	out = append(out, byte(headerBits>>8), byte(headerBits))
	out = append(out, hb...)
	out = append(out, data...)
	return out
}
