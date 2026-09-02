package flac

import (
	"bytes"
	"errors"
	"testing"
)

// An unfragmented frame (marker set, no reassembly in progress) is returned as
// the payload itself, aliased, with no allocation.
func TestUnfragmentedFrameAliasesPayload(t *testing.T) {
	d := New()
	frame := []byte{0xFF, 0xF8, 0x69, 0x18, 0x00, 0x01, 0x02, 0x03}
	got, err := d.Depacketize(frame, true)
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
// only when the marker arrives.
func TestFragmentedReassembly(t *testing.T) {
	d := New()
	p0 := []byte{0xFF, 0xF8, 0x01, 0x02}
	p1 := []byte{0x03, 0x04, 0x05}
	p2 := []byte{0x06, 0x07}

	if got, err := d.Depacketize(p0, false); got != nil || err != nil {
		t.Fatalf("first fragment: got %x, %v; want nil, nil (buffering)", got, err)
	}
	if got, err := d.Depacketize(p1, false); got != nil || err != nil {
		t.Fatalf("middle fragment: got %x, %v; want nil, nil (buffering)", got, err)
	}
	got, err := d.Depacketize(p2, true)
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
	_, _ = d.Depacketize([]byte{0x01, 0x02}, false)
	if _, err := d.Depacketize([]byte{0x03}, true); err != nil {
		t.Fatalf("complete first frame: %v", err)
	}
	next := []byte{0xAA, 0xBB}
	got, err := d.Depacketize(next, true)
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
	if _, err := d.Depacketize(nil, true); !errors.Is(err, ErrEmptyPayload) {
		t.Fatalf("empty payload err = %v, want ErrEmptyPayload", err)
	}

	_, _ = d.Depacketize([]byte{0x01, 0x02}, false) // buffering
	if _, err := d.Depacketize(nil, false); !errors.Is(err, ErrEmptyPayload) {
		t.Fatalf("empty mid-reassembly err = %v, want ErrEmptyPayload", err)
	}
	// The partial frame must have been dropped: a fresh unfragmented packet is
	// delivered whole.
	next := []byte{0x09, 0x08}
	got, err := d.Depacketize(next, true)
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
	if _, err := d.Depacketize(make([]byte, MaxFrameSize), false); err != nil {
		t.Fatalf("cap-sized first fragment: %v", err)
	}
	if _, err := d.Depacketize([]byte{0x00}, false); !errors.Is(err, ErrFrameOverflow) {
		t.Fatalf("overflow err = %v, want ErrFrameOverflow", err)
	}
	// Reassembly was reset, so a normal frame works again.
	if _, err := d.Depacketize([]byte{0x01}, true); err != nil {
		t.Fatalf("after overflow reset: %v", err)
	}
}

// A single payload larger than the cap cannot even start reassembly.
func TestOversizeFirstFragment(t *testing.T) {
	d := New()
	if _, err := d.Depacketize(make([]byte, MaxFrameSize+1), false); !errors.Is(err, ErrFrameOverflow) {
		t.Fatalf("oversize first fragment err = %v, want ErrFrameOverflow", err)
	}
}

// Reset mid-reassembly drops the partial frame.
func TestResetDropsPartial(t *testing.T) {
	d := New()
	_, _ = d.Depacketize([]byte{0x01, 0x02, 0x03}, false)
	d.Reset()
	tail := []byte{0xEE, 0xFF}
	got, err := d.Depacketize(tail, true)
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
	f.Add([]byte{0xFF, 0xF8, 0x01}, true)
	f.Add([]byte{}, false)
	f.Add([]byte{0x00}, false)
	f.Fuzz(func(t *testing.T, payload []byte, marker bool) {
		d := New()
		// Feed the same payload a few times with alternating markers to exercise
		// the reassembly state machine.
		for i := 0; i < 4; i++ {
			_, err := d.Depacketize(payload, marker || i == 3)
			if err != nil && !errors.Is(err, ErrEmptyPayload) && !errors.Is(err, ErrFrameOverflow) {
				t.Fatalf("unexpected error value: %v", err)
			}
		}
		// Success-path invariant: a fresh depacketizer fed one non-empty marker
		// packet returns exactly that payload as the completed frame.
		if len(payload) > 0 {
			frame, err := New().Depacketize(payload, true)
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
		if _, err := d.Depacketize(frame, true); err != nil {
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
		if _, err := d.Depacketize(frame, true); err != nil {
			b.Fatal(err)
		}
	}
}
