package rtp_test

import (
	"testing"

	"github.com/tphakala/go-audio-stream/rtsp/rtp"
)

// ahead is the same wraparound-aware forward distance the Reorderer itself
// uses, duplicated here so tests can assert ordering without reaching into
// unexported state.
func ahead(a, b uint16) int {
	return int(int16(a - b))
}

func TestReordererInOrderPassthrough(t *testing.T) {
	t.Parallel()
	var r rtp.Reorderer
	var out []rtp.Released

	out = r.Push(100, []byte{100}, out)
	if want := []uint16{100}; !seqsEqual(out, want) {
		t.Fatalf("push 100: got %v, want %v", seqs(out), want)
	}

	out = r.Push(101, []byte{101}, out)
	if want := []uint16{101}; !seqsEqual(out, want) {
		t.Fatalf("push 101: got %v, want %v", seqs(out), want)
	}

	out = r.Push(102, []byte{102}, out)
	if want := []uint16{102}; !seqsEqual(out, want) {
		t.Fatalf("push 102: got %v, want %v", seqs(out), want)
	}

	if st := r.Stats(); st.Buffered != 0 || st.Late != 0 || st.Forced != 0 {
		t.Fatalf("stats = %+v, want all zero except releases", st)
	}
}

func TestReordererSimpleSwap(t *testing.T) {
	t.Parallel()
	var r rtp.Reorderer
	var out []rtp.Released

	out = r.Push(100, []byte{100}, out)
	if want := []uint16{100}; !seqsEqual(out, want) {
		t.Fatalf("push 100: got %v, want %v", seqs(out), want)
	}

	out = r.Push(102, []byte{102}, out)
	if len(out) != 0 {
		t.Fatalf("push 102: got %v, want nothing released yet", seqs(out))
	}

	out = r.Push(101, []byte{101}, out)
	if want := []uint16{101, 102}; !seqsEqual(out, want) {
		t.Fatalf("push 101: got %v, want %v", seqs(out), want)
	}

	if st := r.Stats(); st.Buffered != 0 {
		t.Fatalf("buffered = %d, want 0", st.Buffered)
	}
}

func TestReordererGapFillThenRun(t *testing.T) {
	t.Parallel()
	var r rtp.Reorderer
	var out []rtp.Released

	out = r.Push(100, []byte{100}, out)
	if want := []uint16{100}; !seqsEqual(out, want) {
		t.Fatalf("push 100: got %v, want %v", seqs(out), want)
	}

	out = r.Push(103, []byte{103}, out)
	if len(out) != 0 {
		t.Fatalf("push 103: got %v, want nothing released", seqs(out))
	}

	out = r.Push(102, []byte{102}, out)
	if len(out) != 0 {
		t.Fatalf("push 102: got %v, want nothing released", seqs(out))
	}

	out = r.Push(101, []byte{101}, out)
	if want := []uint16{101, 102, 103}; !seqsEqual(out, want) {
		t.Fatalf("push 101: got %v, want %v", seqs(out), want)
	}
}

func TestReordererLateDrop(t *testing.T) {
	t.Parallel()
	var r rtp.Reorderer
	var out []rtp.Released

	out = r.Push(100, []byte{100}, out)
	out = r.Push(101, []byte{101}, out)
	out = r.Push(102, []byte{102}, out)

	out = r.Push(101, []byte{101}, out)
	if len(out) != 0 {
		t.Fatalf("late push released %v, want nothing", seqs(out))
	}
	if st := r.Stats(); st.Late != 1 {
		t.Fatalf("Late = %d, want 1", st.Late)
	}
}

func TestReordererDuplicateDrop(t *testing.T) {
	t.Parallel()
	var r rtp.Reorderer
	var out []rtp.Released

	out = r.Push(100, []byte{100}, out)
	if want := []uint16{100}; !seqsEqual(out, want) {
		t.Fatalf("push 100: got %v, want %v", seqs(out), want)
	}

	out = r.Push(102, []byte{102}, out)
	if len(out) != 0 {
		t.Fatalf("push 102: got %v, want nothing released", seqs(out))
	}

	out = r.Push(102, []byte{0xFF}, out)
	if len(out) != 0 {
		t.Fatalf("duplicate 102: got %v, want nothing released", seqs(out))
	}
	if st := r.Stats(); st.Late != 1 {
		t.Fatalf("Late = %d, want 1", st.Late)
	}

	out = r.Push(101, []byte{101}, out)
	if want := []uint16{101, 102}; !seqsEqual(out, want) {
		t.Fatalf("push 101: got %v, want %v", seqs(out), want)
	}
	if out[1].Payload[0] != 102 {
		t.Fatalf("released 102 payload = %v, want the first copy, not the duplicate", out[1].Payload)
	}
}

func TestReordererWindowOverflowForceRelease(t *testing.T) {
	t.Parallel()
	var r rtp.Reorderer
	var out []rtp.Released

	out = r.Push(100, []byte{100}, out)
	if want := []uint16{100}; !seqsEqual(out, want) {
		t.Fatalf("push 100: got %v, want %v", seqs(out), want)
	}

	// 101 never arrives. Fill the window with 102..228 (127 packets), all
	// within MaxReorderWindow of the release point (101), so none of them
	// force a release on their own.
	for seq := 102; seq <= 228; seq++ {
		out = r.Push(uint16(seq), []byte{byte(seq)}, out)
		if len(out) != 0 {
			t.Fatalf("push %d: got %v, want nothing released while buffering", seq, seqs(out))
		}
	}
	if st := r.Stats(); st.Buffered != 127 {
		t.Fatalf("buffered = %d, want 127", st.Buffered)
	}

	// 229 is MaxReorderWindow ahead of the release point (101): forces
	// the gap at 101 open and releases the entire buffered run in one call.
	out = r.Push(229, []byte{229}, out)

	want := make([]uint16, 0, 128)
	for seq := 102; seq <= 229; seq++ {
		want = append(want, uint16(seq))
	}
	if !seqsEqual(out, want) {
		t.Fatalf("push 229: got %d releases starting %v, want %d releases 102..229", len(out), firstFew(out, 5), len(want))
	}

	st := r.Stats()
	if st.Forced < 1 {
		t.Fatalf("Forced = %d, want >= 1", st.Forced)
	}
	if st.Buffered != 0 {
		t.Fatalf("buffered = %d, want 0 after the forced release drained the run", st.Buffered)
	}
}

// A force-release whose walk passes over an already-BUFFERED slot must evict
// that slot into the released output WITHOUT counting it as forced loss: only
// the genuinely-missing sequence numbers the walk skips are forced. This
// exercises the "present" branch of Push's force-release loop, which the
// overflow tests that force only over empty gaps never reach.
func TestReordererForceReleaseEvictsOccupiedSlot(t *testing.T) {
	t.Parallel()
	var r rtp.Reorderer
	var out []rtp.Released

	const n = 1000
	// Release N, so the next release point is N+1.
	out = r.Push(n, []byte{0x00}, out)
	if want := []uint16{n}; !seqsEqual(out, want) {
		t.Fatalf("push %d: got %v, want %v", n, seqs(out), want)
	}

	// Buffer N+5 without releasing it: N+1..N+4 stay an open gap. Give it a
	// distinctive payload so the eviction path can be shown to release the
	// buffered copy rather than fabricate one.
	out = r.Push(n+5, []byte{0x55}, out)
	if len(out) != 0 {
		t.Fatalf("push %d: got %v, want nothing released (buffered behind the gap)", n+5, seqs(out))
	}

	// A packet MaxReorderWindow ahead of the occupied slot forces the release
	// point to walk from N+1 up past N+5: it counts N+1..N+4 (the empty gap) as
	// forced and evicts the present N+5 into the output, not as forced.
	forcing := uint16(n + 5 + rtp.MaxReorderWindow)
	out = r.Push(forcing, []byte{0x99}, out)

	if want := []uint16{n + 5}; !seqsEqual(out, want) {
		t.Fatalf("push %d: got %v, want [%d] (occupied slot evicted, forcing packet buffered)", forcing, seqs(out), n+5)
	}
	if out[0].Payload[0] != 0x55 {
		t.Fatalf("evicted N+5 payload = %v, want the buffered copy 0x55", out[0].Payload)
	}
	if st := r.Stats(); st.Forced != 4 {
		t.Fatalf("Forced = %d, want 4 (only N+1..N+4 are missing; the present N+5 is evicted, not forced)", st.Forced)
	}
	if st := r.Stats(); st.Buffered != 1 {
		t.Fatalf("Buffered = %d, want 1 (the forcing packet %d is now buffered)", st.Buffered, forcing)
	}
}

func TestReordererWraparound(t *testing.T) {
	t.Parallel()
	var r rtp.Reorderer
	var out []rtp.Released

	for _, seq := range []uint16{65534, 65535, 0, 1} {
		out = r.Push(seq, []byte{byte(seq)}, out)
		if want := []uint16{seq}; !seqsEqual(out, want) {
			t.Fatalf("push %d: got %v, want %v", seq, seqs(out), want)
		}
	}
}

func TestReordererReorderedWraparound(t *testing.T) {
	t.Parallel()
	var r rtp.Reorderer
	var out []rtp.Released

	out = r.Push(65535, []byte{1}, out)
	if want := []uint16{65535}; !seqsEqual(out, want) {
		t.Fatalf("push 65535: got %v, want %v", seqs(out), want)
	}

	out = r.Push(1, []byte{2}, out)
	if len(out) != 0 {
		t.Fatalf("push 1: got %v, want nothing released", seqs(out))
	}

	out = r.Push(0, []byte{3}, out)
	if want := []uint16{0, 1}; !seqsEqual(out, want) {
		t.Fatalf("push 0: got %v, want %v", seqs(out), want)
	}
}

func TestReordererFlushOnSSRCChangeThenReset(t *testing.T) {
	t.Parallel()
	var r rtp.Reorderer
	var out []rtp.Released

	out = r.Push(100, []byte{100}, out)
	if want := []uint16{100}; !seqsEqual(out, want) {
		t.Fatalf("push 100: got %v, want %v", seqs(out), want)
	}

	out = r.Push(103, []byte{103}, out)
	if len(out) != 0 {
		t.Fatalf("push 103: got %v, want nothing released", seqs(out))
	}
	out = r.Push(102, []byte{102}, out)
	if len(out) != 0 {
		t.Fatalf("push 102: got %v, want nothing released", seqs(out))
	}

	out = r.Flush(out)
	if want := []uint16{102, 103}; !seqsEqual(out, want) {
		t.Fatalf("flush: got %v, want %v", seqs(out), want)
	}
	if st := r.Stats(); st.Buffered != 0 {
		t.Fatalf("buffered after flush = %d, want 0", st.Buffered)
	}

	r.Reset()

	out = r.Push(500, []byte{500 & 0xFF}, out)
	if want := []uint16{500}; !seqsEqual(out, want) {
		t.Fatalf("push 500 after reset: got %v, want %v", seqs(out), want)
	}
	if st := r.Stats(); st.Buffered != 0 {
		t.Fatalf("buffered after reset+push = %d, want 0", st.Buffered)
	}
}

func TestReordererResetClearsCumulativeCounters(t *testing.T) {
	t.Parallel()
	var r rtp.Reorderer
	var out []rtp.Released

	// Produce a late/duplicate drop.
	out = r.Push(100, []byte{100}, out)
	out = r.Push(101, []byte{101}, out)
	out = r.Push(101, []byte{101}, out) // late: already released
	if st := r.Stats(); st.Late == 0 {
		t.Fatalf("Late = %d, want > 0 before reset", st.Late)
	}

	// Produce a window-overflow force-release.
	out = r.Push(300, []byte{0}, out) // far ahead of the release point, forces the gap
	if st := r.Stats(); st.Forced == 0 {
		t.Fatalf("Forced = %d, want > 0 before reset", st.Forced)
	}

	r.Reset()

	if st := r.Stats(); st.Late != 0 || st.Forced != 0 || st.Buffered != 0 {
		t.Fatalf("stats after reset = %+v, want all zero", st)
	}

	out = r.Push(700, []byte{7}, out)
	if want := []uint16{700}; !seqsEqual(out, want) {
		t.Fatalf("push after reset: got %v, want %v", seqs(out), want)
	}
	if st := r.Stats(); st.Buffered != 0 {
		t.Fatalf("buffered after reset+push = %d, want 0", st.Buffered)
	}
}

func seqs(rel []rtp.Released) []uint16 {
	out := make([]uint16, len(rel))
	for i, r := range rel {
		out[i] = r.Seq
	}
	return out
}

func firstFew(rel []rtp.Released, n int) []uint16 {
	if len(rel) < n {
		n = len(rel)
	}
	return seqs(rel[:n])
}

func seqsEqual(rel []rtp.Released, want []uint16) bool {
	if len(rel) != len(want) {
		return false
	}
	for i, r := range rel {
		if r.Seq != want[i] {
			return false
		}
	}
	return true
}
