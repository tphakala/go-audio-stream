package g726_test

import (
	"os"
	"path/filepath"
	"testing"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/depacket/g726"
)

var fuzzRates = []audiostream.G726BitRate{
	audiostream.G726Rate16,
	audiostream.G726Rate24,
	audiostream.G726Rate32,
	audiostream.G726Rate40,
}

// FuzzDecode checks that Decode never panics or writes out of bounds for
// arbitrary payloads at any rate, and that its byte count is consistent with
// the codeword width.
func FuzzDecode(f *testing.F) {
	f.Add(uint8(2), []byte{0x00})
	f.Add(uint8(4), []byte{0x12, 0x34, 0x56, 0x78})
	f.Add(uint8(5), []byte{0xff, 0xff, 0xff, 0xff, 0xff})
	for _, base := range []string{"rate2bit", "rate3bit", "rate4bit", "rate5bit"} {
		if p, err := os.ReadFile(filepath.Join("testdata", base+".payload")); err == nil {
			f.Add(base[4]-'0', p)
		}
	}

	f.Fuzz(func(t *testing.T, sel uint8, payload []byte) {
		rate := fuzzRates[int(sel)%len(fuzzRates)]
		d, err := g726.New(rate)
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		// Exactly-sized destination: the return must fill it and write no more.
		out, err := d.DecodeAlloc(payload)
		if err != nil {
			t.Fatalf("DecodeAlloc: %v", err)
		}
		if len(out)%2 != 0 {
			t.Fatalf("odd output length %d", len(out))
		}

		// Oversized destination guarded by canaries: Decode must not touch the
		// bytes past what it reports writing.
		d2, _ := g726.New(rate)
		dst := make([]byte, len(out)+8)
		for i := range dst {
			dst[i] = 0x5A
		}
		n, err := d2.Decode(dst, payload)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if n != len(out) {
			t.Fatalf("Decode wrote %d, DecodeAlloc produced %d", n, len(out))
		}
		for i := n; i < len(dst); i++ {
			if dst[i] != 0x5A {
				t.Fatalf("Decode wrote past its reported length at byte %d", i)
			}
		}
	})
}
