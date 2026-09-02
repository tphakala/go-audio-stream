package flac

import (
	"bytes"
	"errors"
	"testing"
)

// ts is an arbitrary RTP timestamp for a frame; fragments of one frame share it.
const ts = uint32(9000)

// An unfragmented frame (marker set, no reassembly in progress) is returned as
// the payload itself, aliased, with no allocation.
func TestUnfragmentedFrameAliasesPayload(t *testing.T) {
	d := New()
	frame := []byte{0xFF, 0xF8, 0x69, 0x18, 0x00, 0x01, 0x02, 0x03}
	got, err := d.Depacketize(frame, true, ts)
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}
	if !bytes.Equal(got, frame) {
		t.Fatalf("frame = %x, want %x", got, frame)
	}
	if &got[0] != &frame[0] {
		t.Error("unfragmented frame should alias the payload, not copy it")
	}
}

// A frame split across packets reassembles into the concatenation, completing
// only when the marker arrives. Every fragment shares the frame's timestamp.
func TestFragmentedReassembly(t *testing.T) {
	d := New()
	p0 := []byte{0xFF, 0xF8, 0x01, 0x02}
	p1 := []byte{0x03, 0x04, 0x05}
	p2 := []byte{0x06, 0x07}

	if got, err := d.Depacketize(p0, false, ts); got != nil || err != nil {
		t.Fatalf("first fragment: got %x, %v; want nil, nil (buffering)", got, err)
	}
	if got, err := d.Depacketize(p1, false, ts); got != nil || err != nil {
		t.Fatalf("middle fragment: got %x, %v; want nil, nil (buffering)", got, err)
	}
	got, err := d.Depacketize(p2, true, ts)
	if err != nil {
		t.Fatalf("final fragment: %v", err)
	}
	want := bytes.Join([][]byte{p0, p1, p2}, nil)
	if !bytes.Equal(got, want) {
		t.Fatalf("reassembled = %x, want %x", got, want)
	}
}

// After completing a fragmented frame the depacketizer is clean, so the next
// unfragmented packet is delivered whole rather than appended to the last frame.
func TestReassemblyResetsAfterCompletion(t *testing.T) {
	d := New()
	_, _ = d.Depacketize([]byte{0x01, 0x02}, false, ts)
	if _, err := d.Depacketize([]byte{0x03}, true, ts); err != nil {
		t.Fatalf("complete first frame: %v", err)
	}
	next := []byte{0xAA, 0xBB}
	got, err := d.Depacketize(next, true, ts+1)
	if err != nil {
		t.Fatalf("second frame: %v", err)
	}
	if !bytes.Equal(got, next) {
		t.Fatalf("second frame = %x, want %x (state leaked from first frame)", got, next)
	}
}

// An empty payload is malformed. Mid-reassembly it also discards the partial
// frame so the next packet starts clean.
func TestEmptyPayload(t *testing.T) {
	d := New()
	if _, err := d.Depacketize(nil, true, ts); !errors.Is(err, ErrEmptyPayload) {
		t.Fatalf("empty payload err = %v, want ErrEmptyPayload", err)
	}

	_, _ = d.Depacketize([]byte{0x01, 0x02}, false, ts) // buffering
	if _, err := d.Depacketize(nil, false, ts); !errors.Is(err, ErrEmptyPayload) {
		t.Fatalf("empty mid-reassembly err = %v, want ErrEmptyPayload", err)
	}
	// The partial frame must have been dropped: a fresh unfragmented packet is
	// delivered whole.
	next := []byte{0x09, 0x08}
	got, err := d.Depacketize(next, true, ts)
	if err != nil {
		t.Fatalf("after empty reset: %v", err)
	}
	if !bytes.Equal(got, next) {
		t.Fatalf("frame = %x, want %x (partial frame not dropped)", got, next)
	}
}

// A fragmented frame whose accumulated size exceeds MaxFrameSize is rejected and
// its partial reassembly discarded.
func TestFragmentOverflow(t *testing.T) {
	d := New()
	// Start at the cap (allowed), then push one byte over it.
	if _, err := d.Depacketize(make([]byte, MaxFrameSize), false, ts); err != nil {
		t.Fatalf("cap-sized first fragment: %v", err)
	}
	if _, err := d.Depacketize([]byte{0x00}, false, ts); !errors.Is(err, ErrFrameOverflow) {
		t.Fatalf("overflow err = %v, want ErrFrameOverflow", err)
	}
	// Reassembly was reset, so a normal frame works again.
	if _, err := d.Depacketize([]byte{0x01}, true, ts); err != nil {
		t.Fatalf("after overflow reset: %v", err)
	}
}

// A single payload larger than the cap cannot even start reassembly.
func TestOversizeFirstFragment(t *testing.T) {
	d := New()
	if _, err := d.Depacketize(make([]byte, MaxFrameSize+1), false, ts); !errors.Is(err, ErrFrameOverflow) {
		t.Fatalf("oversize first fragment err = %v, want ErrFrameOverflow", err)
	}
}

// A continuation fragment carrying a different RTP timestamp than the fragment
// that started the frame means the frame boundary was lost. The stale partial is
// dropped and the mismatched packet is reprocessed as the start of the new frame,
// so the new frame reassembles cleanly and never carries the old frame's bytes,
// nor loses its own opening fragment.
func TestTimestampMismatchDropsPartialAndRecovers(t *testing.T) {
	d := New()
	// Buffer the start of frame A at timestamp ts.
	if _, err := d.Depacketize([]byte{0x0A, 0x0A}, false, ts); err != nil {
		t.Fatalf("frame A fragment: %v", err)
	}
	// Frame B's first fragment arrives at a different timestamp (A's boundary was
	// lost). It must START frame B (buffering), not error and not drop it.
	if got, err := d.Depacketize([]byte{0x0B, 0x0B}, false, ts+1); got != nil || err != nil {
		t.Fatalf("mismatched fragment: got %x, %v; want nil, nil (buffering the new frame)", got, err)
	}
	got, err := d.Depacketize([]byte{0x0C}, true, ts+1)
	if err != nil {
		t.Fatalf("frame B final fragment: %v", err)
	}
	// Frame B only; frame A's 0x0A bytes must not be spliced in, and frame B's
	// opening 0x0B fragment must not be lost.
	if want := []byte{0x0B, 0x0B, 0x0C}; !bytes.Equal(got, want) {
		t.Fatalf("frame = %x, want %x (stale partial spliced, or new frame's start dropped)", got, want)
	}
}

// A mismatched packet that is itself a complete single-packet frame (marker set)
// is delivered as that frame, not dropped.
func TestTimestampMismatchCompleteFrameRecovered(t *testing.T) {
	d := New()
	if _, err := d.Depacketize([]byte{0x0A, 0x0A}, false, ts); err != nil {
		t.Fatalf("frame A fragment: %v", err)
	}
	got, err := d.Depacketize([]byte{0x0B, 0x0C}, true, ts+1)
	if err != nil {
		t.Fatalf("mismatched complete frame: %v", err)
	}
	if want := []byte{0x0B, 0x0C}; !bytes.Equal(got, want) {
		t.Fatalf("frame = %x, want %x (complete frame lost on mismatch)", got, want)
	}
}

// Reset mid-reassembly drops the partial frame.
func TestResetDropsPartial(t *testing.T) {
	d := New()
	_, _ = d.Depacketize([]byte{0x01, 0x02, 0x03}, false, ts)
	d.Reset()
	tail := []byte{0xEE, 0xFF}
	got, err := d.Depacketize(tail, true, ts+1)
	if err != nil {
		t.Fatalf("after reset: %v", err)
	}
	if !bytes.Equal(got, tail) {
		t.Fatalf("frame = %x, want %x (Reset did not drop the partial frame)", got, tail)
	}
}

// Depacketize must never panic on arbitrary input and must never return a
// non-sentinel error.
func FuzzDepacketize(f *testing.F) {
	f.Add([]byte{0xFF, 0xF8, 0x01}, true, uint32(1))
	f.Add([]byte{}, false, uint32(0))
	f.Add([]byte{0x00}, false, uint32(7))
	f.Fuzz(func(t *testing.T, payload []byte, marker bool, rtpTime uint32) {
		d := New()
		// Feed the same payload a few times with alternating markers to exercise
		// the reassembly state machine.
		for i := 0; i < 4; i++ {
			_, err := d.Depacketize(payload, marker || i == 3, rtpTime)
			if err != nil && !errors.Is(err, ErrEmptyPayload) && !errors.Is(err, ErrFrameOverflow) {
				t.Fatalf("unexpected error value: %v", err)
			}
		}
		// Success-path invariant: a fresh depacketizer fed one non-empty marker
		// packet returns exactly that payload as the completed frame.
		if len(payload) > 0 {
			frame, err := New().Depacketize(payload, true, rtpTime)
			if err != nil {
				t.Fatalf("unfragmented marker packet errored: %v", err)
			}
			if !bytes.Equal(frame, payload) {
				t.Fatalf("unfragmented frame = %x, want payload %x", frame, payload)
			}
		}
	})
}

// The unfragmented delivery path allocates nothing, asserted programmatically so
// a future allocation regression fails the suite (the benchmark only measures).
// Mirrors the AllocsPerRun guards in depacket/g726 and depacket/latm.
func TestUnfragmentedDepacketizeZeroAlloc(t *testing.T) {
	d := New()
	frame := make([]byte, 512)
	frame[0], frame[1] = 0xFF, 0xF8
	got := testing.AllocsPerRun(100, func() {
		if _, err := d.Depacketize(frame, true, ts); err != nil {
			t.Fatalf("Depacketize: %v", err)
		}
	})
	if got != 0 {
		t.Errorf("unfragmented Depacketize allocated %v times, want 0", got)
	}
}

// The unfragmented delivery path allocates nothing.
func BenchmarkDepacketizeUnfragmented(b *testing.B) {
	d := New()
	frame := make([]byte, 512)
	frame[0], frame[1] = 0xFF, 0xF8
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Depacketize(frame, true, ts); err != nil {
			b.Fatal(err)
		}
	}
}
