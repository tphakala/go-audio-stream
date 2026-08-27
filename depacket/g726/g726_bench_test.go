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

func BenchmarkDecodeDst(b *testing.B) {
	for _, rc := range rateCases {
		b.Run(rc.name, func(b *testing.B) {
			d, err := g726.New(rc.rate)
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

func BenchmarkDecodeAlloc(b *testing.B) {
	d, err := g726.New(audiostream.G726Rate32)
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
