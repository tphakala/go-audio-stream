package g726

import (
	"fmt"
	"math/rand"
	"testing"
)

// This file covers the two codeword readers directly. They had no tests of
// their own: the committed conformance vectors exercise them only through
// Decode, at the whole-codeword offsets one payload happens to produce.
//
// The readers are checked against an independent model implementation rather
// than against golden constants. modelCodewordLSB and modelCodewordMSB compute
// the same fields by a different method (load the byte span the field covers
// into an accumulator and shift it out, the idiom depacket/aac's readBits and
// depacket/latm's bitReader.read use) instead of walking one bit at a time as
// the shipped readers do. Two independently derived algorithms agreeing across
// the whole input domain is stronger evidence than a handful of hand-computed
// values, and it costs nothing to keep.
//
// The accumulator form is also the rewrite issue #128 proposed for the shipped
// readers. It is NOT shipped, and BenchmarkCodewordReader below records why:
// measured on a quiet host, it does not pay for itself. See that benchmark's
// comment for the numbers.

// modelCodewordLSB is the accumulator model for readCodewordLSB, the RFC 3551
// section 4.5.4 order. Because that order numbers bits from the least
// significant, the bytes load little-endian: byte firstByte+i contributes at
// accumulator bit 8*i, which puts global bit index g at accumulator bit
// g-8*firstByte and makes the field a single shift and mask. width is at most
// 5, so a codeword straddles at most two bytes and the accumulator cannot
// overflow.
func modelCodewordLSB(payload []byte, pos, width int) int32 {
	firstByte := pos >> 3
	lastByte := (pos + width - 1) >> 3
	var acc uint32
	for i, x := range payload[firstByte : lastByte+1] {
		acc |= uint32(x) << uint(8*i)
	}
	return int32((acc >> uint(pos-8*firstByte)) & (1<<uint(width) - 1))
}

// modelCodewordMSB is the accumulator model for readCodewordMSB, the AAL2-G726
// (ITU-T I.366.2 Annex E) order. It loads big-endian because that order numbers
// bits from the most significant: the field then sits rightPad bits above the
// bottom of the loaded bytes.
func modelCodewordMSB(payload []byte, pos, width int) int32 {
	firstByte := pos >> 3
	lastByte := (pos + width - 1) >> 3
	var acc uint32
	for _, x := range payload[firstByte : lastByte+1] {
		acc = acc<<8 | uint32(x)
	}
	rightPad := (lastByte+1)*8 - (pos + width)
	return int32((acc >> uint(rightPad)) & (1<<uint(width) - 1))
}

// codewordWidths are the four G.726 codeword widths, in bits, one per bit rate
// (16, 24, 32 and 40 kbps at an 8 kHz clock).
var codewordWidths = []int{2, 3, 4, 5}

// TestCodewordReadersMatchModel sweeps every codeword position of a
// pseudo-random payload at every width and asserts both readers agree with the
// model bit for bit. Decode calls them at whole-codeword offsets into a payload
// holding a whole number of codeword groups, so this covers exactly the
// positions reachable in production, including every alignment of a field
// against the byte boundaries it straddles.
func TestCodewordReadersMatchModel(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // deterministic test input, not security material
	payload := make([]byte, 256)
	if _, err := rng.Read(payload); err != nil {
		t.Fatalf("seed payload: %v", err)
	}
	for _, width := range codewordWidths {
		nsamp := (len(payload) * 8) / width
		for k := 0; k < nsamp; k++ {
			pos := k * width
			if got, want := readCodewordLSB(payload, pos, width), modelCodewordLSB(payload, pos, width); got != want {
				t.Fatalf("readCodewordLSB(width=%d, pos=%d) = %d, want %d", width, pos, got, want)
			}
			if got, want := readCodewordMSB(payload, pos, width), modelCodewordMSB(payload, pos, width); got != want {
				t.Fatalf("readCodewordMSB(width=%d, pos=%d) = %d, want %d", width, pos, got, want)
			}
		}
	}
}

// TestCodewordReadersAtEveryBitOffset walks both readers across every bit
// offset a field can start at, not just the whole-codeword offsets Decode uses.
// Decode cannot reach a non-multiple offset today, but the readers take pos in
// bits and nothing in their signature says otherwise, so pinning the whole
// domain keeps a future caller (a byte-aligned resync, a different framing)
// from finding a reader that is only correct on multiples of width.
func TestCodewordReadersAtEveryBitOffset(t *testing.T) {
	t.Parallel()
	payload := []byte{0x00, 0xFF, 0xA5, 0x5A, 0x0F, 0xF0, 0xC3, 0x3C}
	for _, width := range codewordWidths {
		for pos := 0; pos+width <= len(payload)*8; pos++ {
			if got, want := readCodewordLSB(payload, pos, width), modelCodewordLSB(payload, pos, width); got != want {
				t.Fatalf("readCodewordLSB(width=%d, pos=%d) = %d, want %d", width, pos, got, want)
			}
			if got, want := readCodewordMSB(payload, pos, width), modelCodewordMSB(payload, pos, width); got != want {
				t.Fatalf("readCodewordMSB(width=%d, pos=%d) = %d, want %d", width, pos, got, want)
			}
		}
	}
}

// TestCodewordReadersKnownVectors pins a handful of fully hand-computed values,
// so a fault common to both the reader and the model (a shared misreading of
// which bit is transmitted first) cannot pass unnoticed. The payload is
// 0xA5 = 1010_0101 followed by 0xC3 = 1100_0011.
//
// LSB-first reads bit 0 upward, so the first 4-bit codeword of 0xA5 is the low
// nibble 0101 = 5 and the second is the high nibble 1010 = 10. MSB-first reads
// bit 7 downward, so the first is 1010 = 10 and the second 0101 = 5. The two
// orders therefore swap the nibbles of this byte, which is the clearest
// possible statement of what distinguishes them.
//
// The two straddle cases are the ones that matter most here, because they are
// where the orders disagree about which byte contributes the high bits: a
// codeword crossing into 0xC3 takes its continuation from that byte's low end
// under LSB-first and from its high end under MSB-first.
func TestCodewordReadersKnownVectors(t *testing.T) {
	t.Parallel()
	payload := []byte{0xA5, 0xC3}
	cases := []struct {
		name         string
		pos, width   int
		wantLSBFirst int32
		wantMSBFirst int32
	}{
		{"4bit/first", 0, 4, 0x5, 0xA},
		{"4bit/second", 4, 4, 0xA, 0x5},
		{"2bit/first", 0, 2, 0x1, 0x2},
		{"5bit/straddle", 4, 5, 0x1A, 0x0B},
		{"4bit/straddle", 6, 4, 0xE, 0x7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := readCodewordLSB(payload, tc.pos, tc.width); got != tc.wantLSBFirst {
				t.Errorf("readCodewordLSB(pos=%d, width=%d) = %#x, want %#x", tc.pos, tc.width, got, tc.wantLSBFirst)
			}
			if got := readCodewordMSB(payload, tc.pos, tc.width); got != tc.wantMSBFirst {
				t.Errorf("readCodewordMSB(pos=%d, width=%d) = %#x, want %#x", tc.pos, tc.width, got, tc.wantMSBFirst)
			}
		})
	}
}

// TestCodewordReadersRangeIsFullWidth asserts both readers can produce every
// value a width-bit codeword can hold, so a masking or shifting error that
// clipped the top bit would fail here rather than silently narrowing the
// codeword space the ADPCM tables are indexed by.
func TestCodewordReadersRangeIsFullWidth(t *testing.T) {
	t.Parallel()
	for _, width := range codewordWidths {
		maxCode := int32(1)<<uint(width) - 1
		seenLSB := make(map[int32]bool, maxCode+1)
		seenMSB := make(map[int32]bool, maxCode+1)
		payload := make([]byte, 2)
		for v := int32(0); v <= maxCode; v++ {
			payload[0] = byte(v)
			seenLSB[readCodewordLSB(payload, 0, width)] = true
			payload[0] = byte(v << uint(8-width))
			seenMSB[readCodewordMSB(payload, 0, width)] = true
		}
		for v := int32(0); v <= maxCode; v++ {
			if !seenLSB[v] {
				t.Errorf("width %d: readCodewordLSB never produced codeword %d", width, v)
			}
			if !seenMSB[v] {
				t.Errorf("width %d: readCodewordMSB never produced codeword %d", width, v)
			}
		}
	}
}

// benchCodewords is the codeword count one benchmark iteration reads, matching
// the isolated 320-codeword measurement issue #128 quoted.
const benchCodewords = 320

// BenchmarkCodewordReader measures the shipped per-bit readers against the
// accumulator model, at every width and in both packings.
//
// It is the evidence for NOT taking issue #128's proposed rewrite. Measured on
// a quiet amd64 host (Intel i7-1260P, pinned to one core, -benchtime=1s
// -count=10, benchstat), the accumulator form wins this isolated benchmark at
// the wider codewords (3-bit -18%, 4-bit -30 to -34%, 5-bit -37 to -39%) and
// ties at 2-bit. But end to end through Decode, where both forms inline
// identically, the picture inverts at the narrow widths and flattens overall:
// 16 kbps +8.1%/+5.4% SLOWER, 24 kbps +1.9%/~, 32 kbps -2.1%/-3.0%, 40 kbps
// -4.0%/-4.0%, geomean +0.22%, i.e. no net change and a real regression at the
// lowest bit rate. At 2 bits the accumulator's setup (two shifts, a slice
// bound, a range loop) costs more than the two-iteration bit loop it replaces.
//
// Note the gap between the two measurements. Both arms here dispatch through a
// function value (r.fn), so neither gets the inlining Decode gives its readers;
// a roughly equal fixed dispatch cost added to both the numerator and the
// denominator compresses the ratio toward 1.0 rather than inflating it, so this
// isolated benchmark does not predict the end-to-end result in either direction.
// That is why the end-to-end DecodeDst numbers, not these, decided it. The
// isolated ratios issue #128 reported (about 4.2x and 8x on this same shape) did
// not reproduce here; that discrepancy is recorded, not explained, and it does
// not change the decision, which rests on the end-to-end measurement above.
func BenchmarkCodewordReader(b *testing.B) {
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // deterministic bench input, not security material
	readers := []struct {
		name string
		fn   func([]byte, int, int) int32
	}{
		{"perbit/lsb", readCodewordLSB},
		{"accum/lsb", modelCodewordLSB},
		{"perbit/msb", readCodewordMSB},
		{"accum/msb", modelCodewordMSB},
	}
	for _, width := range codewordWidths {
		payload := make([]byte, (benchCodewords*width+7)/8)
		if _, err := rng.Read(payload); err != nil {
			b.Fatalf("seed payload: %v", err)
		}
		for _, r := range readers {
			b.Run(fmt.Sprintf("%dbit/%s", width, r.name), func(b *testing.B) {
				var sink int32
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					pos := 0
					for k := 0; k < benchCodewords; k++ {
						sink ^= r.fn(payload, pos, width)
						pos += width
					}
				}
				// Consume sink so the reads cannot be optimized away.
				codewordSink = sink
			})
		}
	}
}

// codewordSink is written by BenchmarkCodewordReader so the compiler cannot
// eliminate the reads it is measuring.
var codewordSink int32
