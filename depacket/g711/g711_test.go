package g711_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/depacket/g711"
)

// refMuLawTable and refALawTable are the published bit-exact Sun/CCITT
// G.711 256-entry decode tables, transcribed verbatim as the independent
// source of truth for the package's expansion. They are literal constants,
// NOT a re-run of the exp/mantissa expansion the implementation uses, so
// TestFullTableCrosscheck genuinely compares the code against an outside
// reference rather than against a copy of itself. The values match the
// public-domain tables in github.com/zaf/g711 (ulaw2lpcm / alaw2lpcm) and
// reproduce the ITU anchors guarded by TestMuLawAnchors / TestALawAnchors.
var refMuLawTable = [256]int16{
	-32124, -31100, -30076, -29052, -28028, -27004, -25980, -24956, -23932, -22908, -21884, -20860, -19836, -18812, -17788, -16764,
	-15996, -15484, -14972, -14460, -13948, -13436, -12924, -12412, -11900, -11388, -10876, -10364, -9852, -9340, -8828, -8316,
	-7932, -7676, -7420, -7164, -6908, -6652, -6396, -6140, -5884, -5628, -5372, -5116, -4860, -4604, -4348, -4092,
	-3900, -3772, -3644, -3516, -3388, -3260, -3132, -3004, -2876, -2748, -2620, -2492, -2364, -2236, -2108, -1980,
	-1884, -1820, -1756, -1692, -1628, -1564, -1500, -1436, -1372, -1308, -1244, -1180, -1116, -1052, -988, -924,
	-876, -844, -812, -780, -748, -716, -684, -652, -620, -588, -556, -524, -492, -460, -428, -396,
	-372, -356, -340, -324, -308, -292, -276, -260, -244, -228, -212, -196, -180, -164, -148, -132,
	-120, -112, -104, -96, -88, -80, -72, -64, -56, -48, -40, -32, -24, -16, -8, 0,
	32124, 31100, 30076, 29052, 28028, 27004, 25980, 24956, 23932, 22908, 21884, 20860, 19836, 18812, 17788, 16764,
	15996, 15484, 14972, 14460, 13948, 13436, 12924, 12412, 11900, 11388, 10876, 10364, 9852, 9340, 8828, 8316,
	7932, 7676, 7420, 7164, 6908, 6652, 6396, 6140, 5884, 5628, 5372, 5116, 4860, 4604, 4348, 4092,
	3900, 3772, 3644, 3516, 3388, 3260, 3132, 3004, 2876, 2748, 2620, 2492, 2364, 2236, 2108, 1980,
	1884, 1820, 1756, 1692, 1628, 1564, 1500, 1436, 1372, 1308, 1244, 1180, 1116, 1052, 988, 924,
	876, 844, 812, 780, 748, 716, 684, 652, 620, 588, 556, 524, 492, 460, 428, 396,
	372, 356, 340, 324, 308, 292, 276, 260, 244, 228, 212, 196, 180, 164, 148, 132,
	120, 112, 104, 96, 88, 80, 72, 64, 56, 48, 40, 32, 24, 16, 8, 0,
}

var refALawTable = [256]int16{
	-5504, -5248, -6016, -5760, -4480, -4224, -4992, -4736, -7552, -7296, -8064, -7808, -6528, -6272, -7040, -6784,
	-2752, -2624, -3008, -2880, -2240, -2112, -2496, -2368, -3776, -3648, -4032, -3904, -3264, -3136, -3520, -3392,
	-22016, -20992, -24064, -23040, -17920, -16896, -19968, -18944, -30208, -29184, -32256, -31232, -26112, -25088, -28160, -27136,
	-11008, -10496, -12032, -11520, -8960, -8448, -9984, -9472, -15104, -14592, -16128, -15616, -13056, -12544, -14080, -13568,
	-344, -328, -376, -360, -280, -264, -312, -296, -472, -456, -504, -488, -408, -392, -440, -424,
	-88, -72, -120, -104, -24, -8, -56, -40, -216, -200, -248, -232, -152, -136, -184, -168,
	-1376, -1312, -1504, -1440, -1120, -1056, -1248, -1184, -1888, -1824, -2016, -1952, -1632, -1568, -1760, -1696,
	-688, -656, -752, -720, -560, -528, -624, -592, -944, -912, -1008, -976, -816, -784, -880, -848,
	5504, 5248, 6016, 5760, 4480, 4224, 4992, 4736, 7552, 7296, 8064, 7808, 6528, 6272, 7040, 6784,
	2752, 2624, 3008, 2880, 2240, 2112, 2496, 2368, 3776, 3648, 4032, 3904, 3264, 3136, 3520, 3392,
	22016, 20992, 24064, 23040, 17920, 16896, 19968, 18944, 30208, 29184, 32256, 31232, 26112, 25088, 28160, 27136,
	11008, 10496, 12032, 11520, 8960, 8448, 9984, 9472, 15104, 14592, 16128, 15616, 13056, 12544, 14080, 13568,
	344, 328, 376, 360, 280, 264, 312, 296, 472, 456, 504, 488, 408, 392, 440, 424,
	88, 72, 120, 104, 24, 8, 56, 40, 216, 200, 248, 232, 152, 136, 184, 168,
	1376, 1312, 1504, 1440, 1120, 1056, 1248, 1184, 1888, 1824, 2016, 1952, 1632, 1568, 1760, 1696,
	688, 656, 752, 720, 560, 528, 624, 592, 944, 912, 1008, 976, 816, 784, 880, 848,
}

func TestMuLawAnchors(t *testing.T) {
	t.Parallel()
	cases := map[byte]int16{0xFF: 0, 0x7F: 0, 0x00: -32124, 0x80: 32124}
	for b, want := range cases {
		if got := refMuLawTable[b]; got != want {
			t.Fatalf("reference table disagrees: refMuLawTable[%#02x] = %d, want %d", b, got, want)
		}
		out, err := g711.DepacketizeAlloc([]byte{b}, audiostream.MuLaw)
		if err != nil {
			t.Fatalf("DepacketizeAlloc(mu-law) = %v", err)
		}
		if got := int16(binary.LittleEndian.Uint16(out)); got != want {
			t.Errorf("mu-law %#02x -> %d, want %d", b, got, want)
		}
	}
}

func TestALawAnchors(t *testing.T) {
	t.Parallel()
	cases := map[byte]int16{0xD5: 8, 0x55: -8, 0xAA: 32256, 0x2A: -32256}
	for b, want := range cases {
		if got := refALawTable[b]; got != want {
			t.Fatalf("reference table disagrees: refALawTable[%#02x] = %d, want %d", b, got, want)
		}
		out, err := g711.DepacketizeAlloc([]byte{b}, audiostream.ALaw)
		if err != nil {
			t.Fatalf("DepacketizeAlloc(a-law) = %v", err)
		}
		if got := int16(binary.LittleEndian.Uint16(out)); got != want {
			t.Errorf("a-law %#02x -> %d, want %d", b, got, want)
		}
	}
}

func TestFullTableCrosscheck(t *testing.T) {
	t.Parallel()
	for i := range 256 {
		b := byte(i)
		muOut, _ := g711.DepacketizeAlloc([]byte{b}, audiostream.MuLaw)
		if got := int16(binary.LittleEndian.Uint16(muOut)); got != refMuLawTable[b] {
			t.Errorf("mu-law %#02x = %d, want %d", b, got, refMuLawTable[b])
		}
		aOut, _ := g711.DepacketizeAlloc([]byte{b}, audiostream.ALaw)
		if got := int16(binary.LittleEndian.Uint16(aOut)); got != refALawTable[b] {
			t.Errorf("a-law %#02x = %d, want %d", b, got, refALawTable[b])
		}
	}
}

func TestDepacketizeIntoBuffer(t *testing.T) {
	t.Parallel()
	payload := []byte{0xFF, 0x00, 0x80}
	dst := make([]byte, 2*len(payload))
	n, err := g711.Depacketize(dst, payload, audiostream.MuLaw)
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}
	if n != 6 {
		t.Fatalf("n = %d, want 6", n)
	}
	want := []int16{refMuLawTable[0xFF], refMuLawTable[0x00], refMuLawTable[0x80]}
	for i, w := range want {
		if got := int16(binary.LittleEndian.Uint16(dst[2*i:])); got != w {
			t.Errorf("sample %d = %d, want %d", i, got, w)
		}
	}
}

func TestDepacketizeShortBuffer(t *testing.T) {
	t.Parallel()
	dst := make([]byte, 2) // too small for 2 samples
	n, err := g711.Depacketize(dst, []byte{0x01, 0x02}, audiostream.ALaw)
	if !errors.Is(err, g711.ErrShortBuffer) {
		t.Fatalf("err = %v, want ErrShortBuffer", err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0", n)
	}
}

func TestDepacketizeEmpty(t *testing.T) {
	t.Parallel()
	n, err := g711.Depacketize(nil, nil, audiostream.MuLaw)
	if err != nil || n != 0 {
		t.Errorf("empty: n=%d err=%v, want 0/nil", n, err)
	}
	if out, err := g711.DepacketizeAlloc(nil, audiostream.ALaw); err != nil || len(out) != 0 {
		t.Errorf("DepacketizeAlloc(nil) len = %d, want 0", len(out))
	}
}

func TestDepacketizeUnknownLaw(t *testing.T) {
	t.Parallel()
	// Decoding with the wrong table yields plausible but wrong audio,
	// which is harder to diagnose than a refusal, so a law that is
	// neither of the two defined ones is rejected rather than defaulted.
	dst := make([]byte, 8)
	n, err := g711.Depacketize(dst, []byte{0x01, 0x02}, audiostream.Law(99))
	if !errors.Is(err, g711.ErrUnknownLaw) {
		t.Errorf("Depacketize(law 99) err = %v, want ErrUnknownLaw", err)
	}
	if n != 0 {
		t.Errorf("Depacketize(law 99) n = %d, want 0", n)
	}
	if out, err := g711.DepacketizeAlloc([]byte{0x01}, audiostream.Law(99)); !errors.Is(err, g711.ErrUnknownLaw) || out != nil {
		t.Errorf("DepacketizeAlloc(law 99) = (%v, %v), want (nil, ErrUnknownLaw)", out, err)
	}
}

func TestDepacketizeShortBufferLeavesDstUntouched(t *testing.T) {
	t.Parallel()
	// The contract says a short destination writes nothing, so a partial
	// write would leave a caller reusing the buffer with a mix of new and
	// stale samples.
	dst := []byte{0xAA, 0xBB, 0xCC, 0xDD}
	before := bytes.Clone(dst)

	n, err := g711.Depacketize(dst, []byte{0x01, 0x02, 0x03}, audiostream.MuLaw)
	if !errors.Is(err, g711.ErrShortBuffer) {
		t.Fatalf("err = %v, want ErrShortBuffer", err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0", n)
	}
	if !bytes.Equal(dst, before) {
		t.Errorf("dst = % x, want it untouched % x", dst, before)
	}
}
