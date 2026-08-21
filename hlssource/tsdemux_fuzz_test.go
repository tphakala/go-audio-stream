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
		checkAU := func(au []byte, dur time.Duration) {
			// A delivered access unit is the ADTS payload with its header stripped,
			// so it cannot be re-parsed as a frame; assert the invariants that do
			// hold for every AU the framer yields. adts.Parse rejects a frame no
			// longer than its header, so the payload is never empty, and it rejects
			// the reserved/escape sample-rate indices, so the frame duration derived
			// from a valid rate is always positive.
			if len(au) == 0 {
				t.Fatal("demux delivered a zero-length access unit")
			}
			if dur <= 0 {
				t.Fatalf("demux delivered an access unit with non-positive duration %v", dur)
			}
		}
		_ = d.demux(seg, false, func(au []byte, dur time.Duration) {
			delivered++
			if delivered > len(seg)+16 {
				t.Fatal("demux delivered more access units than the input can hold")
			}
			checkAU(au, dur)
		})
		d.end(checkAU)
		// Once any frame has been delivered the resolved AudioSpecificConfig is a
		// well-formed 2-byte AAC config; nil before the first frame is fine.
		if asc := d.audioSpecificConfig(); asc != nil && len(asc) != 2 {
			t.Fatalf("audioSpecificConfig len = %d, want 2", len(asc))
		}
	})
}
