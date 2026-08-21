package hlssource

import (
	"testing"
	"time"
)

// FuzzTSDemux drives the MPEG-TS demuxer with arbitrary bytes as a single
// segment. It must never panic and must always terminate: the packet loop
// advances by a fixed 188 bytes (or resyncs forward), so it cannot spin. The
// demux behavior itself has dedicated tests; this is a totality check over
// untrusted segment bytes.
func FuzzTSDemux(f *testing.F) {
	stream, _ := adtsStream(2, 30)
	f.Add(buildTSSegment(stream, 0x1000, 0x0100))
	f.Add([]byte{0x47, 0x00, 0x00, 0x10})
	f.Add(make([]byte, tsPacketLen))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, seg []byte) {
		d := newTSDemux()
		delivered := 0
		_ = d.demux(seg, false, func(au []byte, _ time.Duration) {
			delivered++
			if delivered > len(seg)+16 {
				t.Fatal("demux delivered more access units than the input can hold")
			}
		})
		d.end(func([]byte, time.Duration) {})
	})
}
