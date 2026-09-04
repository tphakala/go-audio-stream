package g711_test

import (
	"testing"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/depacket/g711"
)

// benchG711Payload returns a realistic G.711 RTP payload: 160 companded
// bytes, one 20ms frame at the 8kHz clock rate G.711 always uses.
func benchG711Payload() []byte {
	p := make([]byte, 160)
	for i := range p {
		p[i] = byte(i)
	}
	return p
}

// BenchmarkDepacketizeDst measures expanding into a caller-owned, reused
// destination buffer, the path the reader takes on every packet. The goal
// is zero allocations per op; this benchmark reports the number so a
// regression shows up in -benchmem without the benchmark itself enforcing
// the bound.
func BenchmarkDepacketizeDst(b *testing.B) {
	payload := benchG711Payload()
	dst := make([]byte, 2*len(payload))

	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for b.Loop() {
		if _, err := g711.Depacketize(dst, payload, audiostream.MuLaw); err != nil {
			b.Fatal(err)
		}
	}
}

// sinkPCM keeps the allocated result reachable so the compiler cannot
// discard the allocation the benchmark exists to measure.
var sinkPCM []byte

// BenchmarkDepacketizeAlloc measures the allocating convenience wrapper,
// for contrast against BenchmarkDepacketizeDst's reused buffer.
func BenchmarkDepacketizeAlloc(b *testing.B) {
	payload := benchG711Payload()

	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for b.Loop() {
		sinkPCM, _ = g711.DepacketizeAlloc(payload, audiostream.MuLaw)
	}
}
