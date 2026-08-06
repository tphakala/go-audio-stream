package rtp_test

import (
	"testing"

	"github.com/tphakala/go-audio-stream/rtsp/rtp"
)

// reorderSeed encodes a sequence of Push calls as records of (delta int8,
// payload byte), matching how FuzzReorderer's driver walks a wide, unwrapped
// cursor: each seeded absolute sequence number is reconstructed as the small
// forward or backward step from the one before it (0 for the first).
func reorderSeed(seqs ...uint16) []byte {
	b := make([]byte, 0, len(seqs)*2)
	var prev uint16
	for i, s := range seqs {
		b = append(b, byte(int8(ahead(s, prev))), byte(i))
		prev = s
	}
	return b
}

func FuzzReorderer(f *testing.F) {
	f.Add(reorderSeed(100, 101, 102))      // in-order
	f.Add(reorderSeed(100, 102, 101))      // simple swap
	f.Add(reorderSeed(100, 103, 102, 101)) // gap fill then run
	f.Add(reorderSeed(65534, 65535, 0, 1)) // wraparound
	f.Add(reorderSeed(65535, 1, 0))        // reordered wrap
	f.Add(reorderSeed(100, 101, 100))      // duplicate
	{
		seeds := make([]uint16, 0, 130)
		seeds = append(seeds, 100)
		for seq := 102; seq <= 229; seq++ {
			seeds = append(seeds, uint16(seq))
		}
		f.Add(reorderSeed(seeds...)) // window overflow force-release
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var r rtp.Reorderer
		var out []rtp.Released

		// cursor is a wide, never-wrapped tracking position. Each record
		// nudges it by a small int8 step before truncating to the 16-bit
		// seq handed to Push. Bounding the per-step delta (rather than
		// letting each pushed seq be a fully arbitrary uint16) keeps every
		// true, unwrapped gap between two consecutive Push calls well
		// under half the 16-bit sequence space: this is a fundamental
		// requirement for any 16-bit sequence comparison, not a Reorderer
		// limitation. A raw, unconstrained jump can land exactly on the
		// ambiguous halfway point (int16 cannot distinguish "32768 forward"
		// from "32768 backward"), which is the same boundary stream.go's
		// own d < 0x8000 convention exists to define one side of. Capping
		// the number of records processed bounds the worst-case cumulative
		// drift in one direction to far less than half the sequence space,
		// so this ambiguity cannot arise no matter how long the fuzz run.
		const maxRecords = 200
		var cursor int32

		haveLast := false
		var last uint16

		checkReleases := func(rel []rtp.Released) {
			for _, one := range rel {
				if haveLast && ahead(one.Seq, last) <= 0 {
					t.Fatalf("release %d not strictly ascending after %d", one.Seq, last)
				}
				last = one.Seq
				haveLast = true
			}
			if st := r.Stats(); st.Buffered > rtp.MaxReorderWindow {
				t.Fatalf("buffered %d exceeds MaxReorderWindow %d", st.Buffered, rtp.MaxReorderWindow)
			}
		}

		records := 0
		for i := 0; i+1 < len(data) && records < maxRecords; i += 2 {
			cursor += int32(int8(data[i]))
			seq := uint16(cursor)
			payload := []byte{data[i+1]}
			out = r.Push(seq, payload, out)
			checkReleases(out)
			records++
		}

		out = r.Flush(out)
		checkReleases(out)
	})
}
