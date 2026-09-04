package latm

import (
	"bytes"
	"testing"
)

// secondOOBElement is a second out-of-band AudioMuxElement for the v3 config
// (numSubFrames 1, so two subframes): PayloadLengthInfo 02 / payload 66 77, then
// 03 / payload 88 99 AA.
var secondOOBElement = []byte{0x02, 0x66, 0x77, 0x03, 0x88, 0x99, 0xAA}

func auOffsets(aus []AU) []uint32 {
	out := make([]uint32, len(aus))
	for i := range aus {
		out[i] = aus[i].RTPOffset
	}
	return out
}

func wantAUs(t *testing.T, aus []AU, data [][]byte, offsets []uint32) {
	t.Helper()
	if len(aus) != len(data) {
		t.Fatalf("AU count = %d, want %d", len(aus), len(data))
	}
	for i := range aus {
		if !bytes.Equal(aus[i].Data, data[i]) {
			t.Errorf("aus[%d].Data = % x, want % x", i, aus[i].Data, data[i])
		}
		if aus[i].RTPOffset != offsets[i] {
			t.Errorf("aus[%d].RTPOffset = %d, want %d", i, aus[i].RTPOffset, offsets[i])
		}
	}
}

func TestOutOfBandTwoElements(t *testing.T) {
	t.Parallel()
	d, err := New(Config{MuxConfigPresent: false, StreamMuxConfig: v3})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	payload := append(bytes.Clone(v3Payload), secondOOBElement...)
	aus, err := d.Depacketize(payload, true, 0)
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}
	// RTPOffset accumulates across elements: element 1 spans 2*1024 ticks, so
	// element 2's subframes continue at 2048 and 3072.
	wantAUs(t, aus,
		[][]byte{{0x11, 0x22}, {0x33, 0x44, 0x55}, {0x66, 0x77}, {0x88, 0x99, 0xAA}},
		[]uint32{0, 1024, 2048, 3072})
}

func TestOutOfBandThreeElements(t *testing.T) {
	t.Parallel()
	d, err := New(Config{MuxConfigPresent: false, StreamMuxConfig: v3})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	third := []byte{0x01, 0xCC, 0x02, 0xDD, 0xEE}
	payload := append(append(bytes.Clone(v3Payload), secondOOBElement...), third...)
	aus, err := d.Depacketize(payload, true, 0)
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}
	if got := auOffsets(aus); len(got) != 6 ||
		got[0] != 0 || got[1] != 1024 || got[2] != 2048 || got[3] != 3072 || got[4] != 4096 || got[5] != 5120 {
		t.Fatalf("offsets = %v, want [0 1024 2048 3072 4096 5120]", got)
	}
	if !bytes.Equal(aus[4].Data, []byte{0xCC}) || !bytes.Equal(aus[5].Data, []byte{0xDD, 0xEE}) {
		t.Errorf("element-3 AUs = % x / % x, want CC / DD EE", aus[4].Data, aus[5].Data)
	}
}

func TestOutOfBandTrailingPaddingDropped(t *testing.T) {
	t.Parallel()
	d, err := New(Config{MuxConfigPresent: false, StreamMuxConfig: v3})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	payload := append(bytes.Clone(v3Payload), 0x00, 0x00, 0x00)
	aus, err := d.Depacketize(payload, true, 0)
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}
	// A wholly-zero remainder is padding, not a second element.
	wantAUs(t, aus, [][]byte{{0x11, 0x22}, {0x33, 0x44, 0x55}}, []uint32{0, 1024})
}

func TestOutOfBandZeroLengthElementIsParsedNotPadding(t *testing.T) {
	t.Parallel()
	d, err := New(Config{MuxConfigPresent: false, StreamMuxConfig: v3})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// A second element whose first subframe is zero-length but whose remainder is
	// not all-zero must be parsed, locking the padding discriminator's boundary:
	// subframe0 len 0 (empty AU), subframe1 len 2 (AA BB).
	payload := append(bytes.Clone(v3Payload), 0x00, 0x02, 0xAA, 0xBB)
	aus, err := d.Depacketize(payload, true, 0)
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}
	wantAUs(t, aus,
		[][]byte{{0x11, 0x22}, {0x33, 0x44, 0x55}, {}, {0xAA, 0xBB}},
		[]uint32{0, 1024, 2048, 3072})
}

func TestOutOfBandTrailingMalformedDeliversLeading(t *testing.T) {
	t.Parallel()
	d, err := New(Config{MuxConfigPresent: false, StreamMuxConfig: v3})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// A second element declaring a 5-byte first subframe with only 2 bytes present
	// overflows; the complete leading element is still delivered (nil error).
	payload := append(bytes.Clone(v3Payload), 0x05, 0x11, 0x22)
	aus, err := d.Depacketize(payload, true, 0)
	if err != nil {
		t.Fatalf("Depacketize: want nil (deliver leading), got %v", err)
	}
	wantAUs(t, aus, [][]byte{{0x11, 0x22}, {0x33, 0x44, 0x55}}, []uint32{0, 1024})
}

func TestOutOfBandElementCap(t *testing.T) {
	t.Parallel()
	// v3 has numSubFrames 1, so each minimal element {0x01,0xAA, 0x01,0xBB} yields
	// two access units. Both cases return nil error: at the cap every element is
	// delivered; over the cap the leading MaxMuxElements are delivered and the rest
	// dropped (the consumer treats a non-nil error as no usable access units, so
	// the cap must not use one).
	cases := []struct {
		name     string
		elements int
		wantAUs  int
	}{
		{"exactly at cap", MaxMuxElements, MaxMuxElements * 2},
		{"over cap drops the excess", MaxMuxElements + 1, MaxMuxElements * 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, err := New(Config{MuxConfigPresent: false, StreamMuxConfig: v3})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			payload := make([]byte, 0, 4*tc.elements)
			for range tc.elements {
				payload = append(payload, 0x01, 0xAA, 0x01, 0xBB)
			}
			aus, err := d.Depacketize(payload, true, 0)
			if err != nil {
				t.Fatalf("Depacketize: want nil, got %v", err)
			}
			if len(aus) != tc.wantAUs {
				t.Fatalf("AU count = %d, want %d", len(aus), tc.wantAUs)
			}
		})
	}
}

// buildInBandTwoElementsSameMux packs two in-band AudioMuxElements into one
// payload: element 1 sends the v3 config, element 2 reuses it (useSameStreamMux
// 1), the common multi-element shape. Each element is byte-aligned at its end.
func buildInBandTwoElementsSameMux() []byte {
	w := &bitWriter{}
	writeV3Header(w)
	w.write(2, 8)
	w.write(0x11, 8)
	w.write(0x22, 8)
	w.write(3, 8)
	w.write(0x33, 8)
	w.write(0x44, 8)
	w.write(0x55, 8)
	w.byteAlign()
	// Element 2: reuse the retained config.
	w.write(1, 1) // useSameStreamMux
	w.write(2, 8)
	w.write(0x66, 8)
	w.write(0x77, 8)
	w.write(3, 8)
	w.write(0x88, 8)
	w.write(0x99, 8)
	w.write(0xAA, 8)
	w.byteAlign()
	return w.bytes()
}

func TestInBandTwoElementsSameMux(t *testing.T) {
	t.Parallel()
	d, err := New(Config{MuxConfigPresent: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	aus, err := d.Depacketize(buildInBandTwoElementsSameMux(), true, 0)
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}
	wantAUs(t, aus,
		[][]byte{{0x11, 0x22}, {0x33, 0x44, 0x55}, {0x66, 0x77}, {0x88, 0x99, 0xAA}},
		[]uint32{0, 1024, 2048, 3072})

	// Disjoint-region correctness across the element boundary: mutating the last
	// AU (element 2) must not corrupt the first AU (element 1), proving every
	// element appends into its own region of the shared d.inBandData buffer.
	for i := range aus[3].Data {
		aus[3].Data[i] = 0xFF
	}
	if !bytes.Equal(aus[0].Data, []byte{0x11, 0x22}) {
		t.Errorf("aus[0].Data corrupted by mutating aus[3].Data: % x", aus[0].Data)
	}
}

func TestInBandTwoElementsResendConfig(t *testing.T) {
	t.Parallel()
	d, err := New(Config{MuxConfigPresent: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Two elements that each send a full config (useSameStreamMux 0). The
	// double in-band config parse in one call must stay memory-safe (the ascBuf
	// double-buffer swaps into the other backing array each time).
	payload := append(bytes.Clone(buildInBandTwoSubframes()), buildInBandTwoSubframes()...)
	aus, err := d.Depacketize(payload, true, 0)
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}
	wantAUs(t, aus,
		[][]byte{{0x11, 0x22}, {0x33, 0x44, 0x55}, {0x11, 0x22}, {0x33, 0x44, 0x55}},
		[]uint32{0, 1024, 2048, 3072})
}

func TestInBandTrailingPaddingDropped(t *testing.T) {
	t.Parallel()
	d, err := New(Config{MuxConfigPresent: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	payload := append(bytes.Clone(buildInBandTwoSubframes()), 0x00, 0x00)
	aus, err := d.Depacketize(payload, true, 0)
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}
	wantAUs(t, aus, [][]byte{{0x11, 0x22}, {0x33, 0x44, 0x55}}, []uint32{0, 1024})
}

func TestInBandTrailingMalformedDeliversLeading(t *testing.T) {
	t.Parallel()
	d, err := New(Config{MuxConfigPresent: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// A valid element 1 (reuse config in element 2 would need a retained config;
	// element 1 supplies it) followed by a truncated element 2.
	payload := append(bytes.Clone(buildInBandTwoSubframes()), buildInBandTwoSubframesTruncated()...)
	aus, err := d.Depacketize(payload, true, 0)
	if err != nil {
		t.Fatalf("Depacketize: want nil (deliver leading), got %v", err)
	}
	wantAUs(t, aus, [][]byte{{0x11, 0x22}, {0x33, 0x44, 0x55}}, []uint32{0, 1024})
}
