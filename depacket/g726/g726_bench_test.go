package g726_test

import (
	"testing"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/depacket/g726"
)

// benchPayload is a deterministic 32 kbps payload (160 bytes -> 320 samples,
// one 40 ms G.726 frame at 8 kHz).
func benchPayload(n int) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = byte(i*31 + 7)
	}
	return p
}

// BenchmarkDecodeDst covers both codeword packings. Decode hoists the packing
// branch out of its per-sample loop, which is only a defensible trade if the two
// loops can be measured against each other; with one packing benchmarked the
// claim would be unfalsifiable in-tree.
func BenchmarkDecodeDst(b *testing.B) {
	packings := []struct {
		name string
		p    audiostream.G726Packing
	}{
		{"rfc3551", audiostream.G726PackingRFC3551},
		{"aal2", audiostream.G726PackingAAL2},
	}
	for _, rc := range rateCases {
		for _, pk := range packings {
			b.Run(rc.name+"/"+pk.name, func(b *testing.B) {
				d, err := g726.New(rc.rate, pk.p)
				if err != nil {
					b.Fatalf("New: %v", err)
				}
				payload := benchPayload(rc.bits * 40)
				dst := make([]byte, 2*(len(payload)*8/rc.bits))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := d.Decode(dst, payload); err != nil {
						b.Fatalf("Decode: %v", err)
					}
				}
			})
		}
	}
}

func BenchmarkDecodeAlloc(b *testing.B) {
	d, err := g726.New(audiostream.G726Rate32, audiostream.G726PackingRFC3551)
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	payload := benchPayload(160)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.DecodeAlloc(payload); err != nil {
			b.Fatalf("DecodeAlloc: %v", err)
		}
	}
}
