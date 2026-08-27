package g726_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/depacket/g726"
)

// rateCase pairs a bit rate with its codeword width and the testdata basename.
type rateCase struct {
	name string
	rate audiostream.G726BitRate
	bits int
	base string
}

var rateCases = []rateCase{
	{"16kbps", audiostream.G726Rate16, 2, "rate2bit"},
	{"24kbps", audiostream.G726Rate24, 3, "rate3bit"},
	{"32kbps", audiostream.G726Rate32, 4, "rate4bit"},
	{"40kbps", audiostream.G726Rate40, 5, "rate5bit"},
}

func loadVector(t *testing.T, base string) (payload, pcm []byte) {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("testdata", base+".payload"))
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	pcm, err = os.ReadFile(filepath.Join("testdata", base+".pcm"))
	if err != nil {
		t.Fatalf("read pcm: %v", err)
	}
	return payload, pcm
}

// TestDecodeMatchesReference is the conformance test: each committed G.726
// payload must decode byte-for-byte to the committed reference PCM.
func TestDecodeMatchesReference(t *testing.T) {
	for _, rc := range rateCases {
		t.Run(rc.name, func(t *testing.T) {
			payload, want := loadVector(t, rc.base)
			d, err := g726.New(rc.rate)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			got, err := d.DecodeAlloc(payload)
			if err != nil {
				t.Fatalf("DecodeAlloc: %v", err)
			}
			if len(got) != len(want) {
				t.Fatalf("length mismatch: got %d bytes, want %d", len(got), len(want))
			}
			if !bytes.Equal(got, want) {
				// Report the first differing sample to make failures debuggable.
				for i := 0; i+1 < len(got); i += 2 {
					if got[i] != want[i] || got[i+1] != want[i+1] {
						t.Fatalf("sample %d differs: got % x, want % x", i/2, got[i:i+2], want[i:i+2])
					}
				}
				t.Fatal("byte mismatch with equal lengths")
			}
		})
	}
}

// TestDecodeSplitEqualsWhole guards the stateful design: decoding a stream in
// one call must equal decoding it split across two calls on the same Decoder.
func TestDecodeSplitEqualsWhole(t *testing.T) {
	for _, rc := range rateCases {
		t.Run(rc.name, func(t *testing.T) {
			payload, _ := loadVector(t, rc.base)
			// Split on a byte boundary that is also a whole-codeword boundary
			// so no partial codeword is dropped at the seam. For 2/4-bit rates
			// every byte is codeword-aligned; for 3/5-bit rates use a multiple
			// of the rate in bytes (bits bytes hold 8 codewords).
			split := (len(payload) / (2 * rc.bits)) * rc.bits
			if split == 0 || split >= len(payload) {
				t.Skipf("payload too short to split at a codeword boundary")
			}

			whole, err := g726.New(rc.rate)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			want, err := whole.DecodeAlloc(payload)
			if err != nil {
				t.Fatalf("DecodeAlloc whole: %v", err)
			}

			split1, err := g726.New(rc.rate)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			a, err := split1.DecodeAlloc(payload[:split])
			if err != nil {
				t.Fatalf("DecodeAlloc part 1: %v", err)
			}
			b, err := split1.DecodeAlloc(payload[split:])
			if err != nil {
				t.Fatalf("DecodeAlloc part 2: %v", err)
			}
			got := make([]byte, 0, len(a)+len(b))
			got = append(got, a...)
			got = append(got, b...)
			if !bytes.Equal(got, want) {
				t.Fatalf("split decode differs from whole decode (%d vs %d bytes)", len(got), len(want))
			}
		})
	}
}

// TestResetRestoresBaseline verifies Reset returns a used decoder to the fresh
// state: decoding after Reset equals decoding from a new decoder.
func TestResetRestoresBaseline(t *testing.T) {
	rc := rateCases[2] // 32 kbps
	payload, _ := loadVector(t, rc.base)

	fresh, _ := g726.New(rc.rate)
	want, err := fresh.DecodeAlloc(payload)
	if err != nil {
		t.Fatalf("DecodeAlloc: %v", err)
	}

	used, _ := g726.New(rc.rate)
	if _, err := used.DecodeAlloc(payload); err != nil {
		t.Fatalf("prime decode: %v", err)
	}
	used.Reset()
	got, err := used.DecodeAlloc(payload)
	if err != nil {
		t.Fatalf("post-reset decode: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("decode after Reset differs from a fresh decoder")
	}
}

func TestNewUnknownBitRate(t *testing.T) {
	if _, err := g726.New(audiostream.G726BitRate(99)); !errors.Is(err, g726.ErrUnknownBitRate) {
		t.Fatalf("New(99): got %v, want ErrUnknownBitRate", err)
	}
}

func TestDecodeEmpty(t *testing.T) {
	d, _ := g726.New(audiostream.G726Rate32)
	n, err := d.Decode(nil, nil)
	if n != 0 || err != nil {
		t.Fatalf("Decode(nil,nil): got (%d,%v), want (0,nil)", n, err)
	}
}

func TestDecodeShortBufferUntouched(t *testing.T) {
	d, _ := g726.New(audiostream.G726Rate32)
	payload := []byte{0x00, 0x11, 0x22, 0x33} // 4 bytes -> 8 samples -> 16 bytes needed
	dst := make([]byte, 4)
	sentinel := []byte{0xAA, 0xBB, 0xCC, 0xDD}
	copy(dst, sentinel)
	n, err := d.Decode(dst, payload)
	if n != 0 || !errors.Is(err, g726.ErrShortBuffer) {
		t.Fatalf("Decode short: got (%d,%v), want (0,ErrShortBuffer)", n, err)
	}
	if !bytes.Equal(dst, sentinel) {
		t.Fatalf("dst was modified on short buffer: %x", dst)
	}
}

func TestDecodeOutputLength(t *testing.T) {
	for _, rc := range rateCases {
		t.Run(rc.name, func(t *testing.T) {
			d, _ := g726.New(rc.rate)
			// bits bytes of payload hold exactly 8 codewords.
			payload := make([]byte, rc.bits*3)
			out, err := d.DecodeAlloc(payload)
			if err != nil {
				t.Fatalf("DecodeAlloc: %v", err)
			}
			wantSamples := (len(payload) * 8) / rc.bits
			if len(out) != 2*wantSamples {
				t.Fatalf("output length: got %d, want %d", len(out), 2*wantSamples)
			}
		})
	}
}
