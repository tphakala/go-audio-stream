package sdp_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tphakala/go-audio-stream/rtsp/sdp"
)

// BenchmarkParse measures parsing the reolink-aac fixture: a real
// two-media (video and AAC-hbr audio) session description, the shape
// Describe parses once per Dial.
func BenchmarkParse(b *testing.B) {
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "sdp", "reolink-aac.sdp"))
	if err != nil {
		b.Fatalf("read fixture: %v", err)
	}

	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := sdp.Parse(body); err != nil {
			b.Fatal(err)
		}
	}
}
