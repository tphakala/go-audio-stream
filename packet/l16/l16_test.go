package l16

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// swapBack converts a big-endian L16 payload back to little-endian, the
// operation the ingest side performs.
func swapBack(payload []byte) []byte {
	le := make([]byte, len(payload))
	for i := 0; i+1 < len(payload); i += 2 {
		binary.LittleEndian.PutUint16(le[i:i+2], binary.BigEndian.Uint16(payload[i:i+2]))
	}
	return le
}

func TestSplitRoundTripsThroughByteSwap(t *testing.T) {
	src := []byte{
		0x01, 0x02, 0x03, 0x04, // stereo frame 0
		0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c,
	}
	p := &Packetizer{Channels: 2, MaxBytes: 1024}
	var got []byte
	frames, err := p.Split(src, func(payload []byte) error {
		got = append(got, swapBack(payload)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if frames != 3 {
		t.Errorf("frames = %d, want 3", frames)
	}
	if !bytes.Equal(got, src) {
		t.Errorf("round-trip:\n got %x\nwant %x", got, src)
	}
}

func TestSplitFrameAlignment(t *testing.T) {
	src := make([]byte, 4*300) // 300 stereo frames
	p := &Packetizer{Channels: 2, MaxBytes: 1023}
	count := 0
	_, err := p.Split(src, func(payload []byte) error {
		count++
		if len(payload)%4 != 0 {
			t.Errorf("payload len %d not frame-aligned", len(payload))
		}
		if len(payload) > 1020 { // 1023 rounded down to a multiple of 4
			t.Errorf("payload len %d exceeds frame-aligned cap 1020", len(payload))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Error("no payloads emitted")
	}
}

func TestSplitPartialFrameRejected(t *testing.T) {
	p := &Packetizer{Channels: 2, MaxBytes: 1024}
	if _, err := p.Split([]byte{1, 2, 3}, func([]byte) error { return nil }); !errors.Is(err, ErrPartialFrame) {
		t.Errorf("Split(3 bytes, stereo) err = %v, want ErrPartialFrame", err)
	}
}

func TestSplitBadChannels(t *testing.T) {
	p := &Packetizer{Channels: 0, MaxBytes: 1024}
	if _, err := p.Split([]byte{1, 2}, func([]byte) error { return nil }); !errors.Is(err, ErrBadChannels) {
		t.Errorf("Split with 0 channels err = %v, want ErrBadChannels", err)
	}
}

func TestSplitEmptyInput(t *testing.T) {
	p := &Packetizer{Channels: 1, MaxBytes: 1024}
	calls := 0
	frames, err := p.Split(nil, func([]byte) error { calls++; return nil })
	if err != nil || frames != 0 || calls != 0 {
		t.Errorf("Split(nil) = (%d frames, %v), %d calls; want 0, nil, 0", frames, err, calls)
	}
}

func TestSplitStopsOnCallbackError(t *testing.T) {
	p := &Packetizer{Channels: 1, MaxBytes: 2} // one frame per payload
	src := make([]byte, 2*5)                   // 5 mono frames
	boom := errors.New("boom")
	calls := 0
	frames, err := p.Split(src, func([]byte) error {
		calls++
		if calls == 2 {
			return boom
		}
		return nil
	})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want boom", err)
	}
	if frames != 1 {
		t.Errorf("frames = %d, want 1 (stopped after the second payload's error)", frames)
	}
}

func TestSplitNoAlloc(t *testing.T) {
	p := &Packetizer{Channels: 1, MaxBytes: 15360}
	src := make([]byte, 15360)
	sink := func([]byte) error { return nil }
	allocs := testing.AllocsPerRun(200, func() {
		if _, err := p.Split(src, sink); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Errorf("Split allocs/op = %v, want 0", allocs)
	}
}

// BenchmarkSplit384k20ms is the real remote-mic hot-path shape: one 20 ms
// payload of mono 384 kHz S16 (7680 frames = 15360 bytes).
func BenchmarkSplit384k20ms(b *testing.B) {
	p := &Packetizer{Channels: 1, MaxBytes: 15360}
	src := make([]byte, 15360)
	sink := func([]byte) error { return nil }
	b.ReportAllocs()
	for range b.N {
		if _, err := p.Split(src, sink); err != nil {
			b.Fatal(err)
		}
	}
}
