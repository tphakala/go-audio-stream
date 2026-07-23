package opus_test

import (
	"testing"

	"github.com/tphakala/go-audio-stream/depacket/opus"
)

// BenchmarkDepacketize measures the identity pass-through for a realistic
// Opus packet: a TOC byte plus frame data, about 120 bytes.
func BenchmarkDepacketize(b *testing.B) {
	pkt := make([]byte, 120)
	pkt[0] = 0x78 // a plausible Opus TOC byte
	for i := 1; i < len(pkt); i++ {
		pkt[i] = byte(i)
	}

	b.ReportAllocs()
	b.SetBytes(int64(len(pkt)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := opus.Depacketize(pkt); err != nil {
			b.Fatal(err)
		}
	}
}
