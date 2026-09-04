package aac_test

import (
	"testing"

	"github.com/tphakala/go-audio-stream/depacket/aac"
)

// benchDepacketizer returns a Depacketizer configured for the ubiquitous
// AAC-hbr field widths, failing the benchmark (instead of a *testing.T
// helper) on the construction error that never happens with these
// constants.
func benchDepacketizer(b *testing.B) *aac.Depacketizer {
	b.Helper()
	dp, err := aac.New(hbrConfig())
	if err != nil {
		b.Fatalf("aac.New: %v", err)
	}
	return dp
}

// benchAUData returns n bytes of non-repeating access-unit content.
func benchAUData(n int, seed byte) []byte {
	d := make([]byte, n)
	for i := range d {
		d[i] = seed + byte(i)
	}
	return d
}

// BenchmarkDepacketizeSingleAU measures the single-AU packet path with a
// realistic AAC-LC access unit. The size (317 bytes) is deliberately not a
// power of two, so no size-aligned fast path in a future implementation
// could mask its true cost.
func BenchmarkDepacketizeSingleAU(b *testing.B) {
	dp := benchDepacketizer(b)
	data := benchAUData(317, 0x01)
	pkt := buildHBR([][]byte{auHeader16(len(data), 0)}, [][]byte{data})

	b.ReportAllocs()
	b.SetBytes(int64(len(pkt)))
	b.ResetTimer()
	for b.Loop() {
		if _, err := dp.Depacketize(pkt, true, 0); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDepacketizeMultiAU measures a packet carrying three access
// units, the shape a low-latency AAC track sends when it batches frames.
// The sizes are non-power-of-two and mutually distinct.
func BenchmarkDepacketizeMultiAU(b *testing.B) {
	dp := benchDepacketizer(b)
	sizes := []int{211, 173, 199}
	headers := make([][]byte, len(sizes))
	data := make([][]byte, len(sizes))
	for i, sz := range sizes {
		headers[i] = auHeader16(sz, 0)
		data[i] = benchAUData(sz, byte(i*31))
	}
	pkt := buildHBR(headers, data)

	b.ReportAllocs()
	b.SetBytes(int64(len(pkt)))
	b.ResetTimer()
	for b.Loop() {
		if _, err := dp.Depacketize(pkt, true, 0); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDepacketizeFragmented measures reassembling one access unit
// split across two packets: two Depacketize calls per iteration, with no
// Reset between iterations, so the benchmark reflects the steady state a
// live stream runs in rather than a cold-start reset each time. The AU
// size and the split point are both non-power-of-two.
func BenchmarkDepacketizeFragmented(b *testing.B) {
	dp := benchDepacketizer(b)
	const total = 953
	const split = 601
	first := benchAUData(split, 0x01)
	second := benchAUData(total-split, 0x81)
	p1 := buildHBR([][]byte{auHeader16(total, 0)}, [][]byte{first})
	p2 := buildHBR([][]byte{auHeader16(total, 0)}, [][]byte{second})

	b.ReportAllocs()
	b.SetBytes(int64(len(p1) + len(p2)))
	b.ResetTimer()
	for b.Loop() {
		if _, err := dp.Depacketize(p1, false, 0); err != nil {
			b.Fatal(err)
		}
		aus, err := dp.Depacketize(p2, true, 0)
		if err != nil {
			b.Fatal(err)
		}
		if len(aus) != 1 {
			b.Fatalf("AU count = %d, want 1", len(aus))
		}
	}
}
