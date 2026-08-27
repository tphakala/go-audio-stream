package hlssource

import (
	"testing"
	"time"
)

// FuzzFMP4Demux drives the fMP4 demuxer with arbitrary bytes as a single media
// fragment, against a fixed, valid initialization segment. It must never panic and
// must always terminate: box iteration advances by the validated box length and
// the trun sample_count is bounds-checked before iterating. The demux behavior has
// dedicated tests; this is a totality check over untrusted fragment bytes.
func FuzzFMP4Demux(f *testing.F) {
	f.Add(buildFragment(1, fmp4Samples(2, 30), 1024))
	f.Add(buildMultiplexedFragment(1, []byte{0xFF, 0xFF}, 2, fmp4Samples(2, 20), 1024))
	f.Add([]byte{0x00, 0x00, 0x00, 0x08, 'm', 'o', 'o', 'f'})
	f.Add([]byte{})

	init := buildInitSegment(wantASC, 44100, 1)
	f.Fuzz(func(t *testing.T, frag []byte) {
		d, err := newFMP4Demux(init)
		if err != nil {
			t.Fatalf("newFMP4Demux on the fixed init: %v", err)
		}
		delivered := 0
		_ = d.demux(frag, false, func(au []byte, dur time.Duration) {
			delivered++
			if delivered > len(frag)+16 {
				t.Fatal("demux delivered more samples than the input can hold")
			}
			if len(au) == 0 {
				t.Fatal("demux delivered a zero-length access unit")
			}
			if dur < 0 {
				t.Fatalf("demux delivered a negative duration %v", dur)
			}
		})
		d.end(func([]byte, time.Duration) {})
	})
}
