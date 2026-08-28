package g726_test

import (
	"errors"
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
// arbitrary payloads at any rate and EITHER codeword packing, and that its byte
// count is consistent with the codeword width. The packing is derived from a
// spare bit of the same selector byte, so the AAL2 (MSB-first) reader, the one
// piece of new bit manipulation, shares the corpus with the RFC 3551 one.
func FuzzDecode(f *testing.F) {
	// aal2Sel sets the packing bit the fuzz body reads, so a seed exercises the
	// MSB-first reader. Seeding BOTH packings matters beyond an actual fuzzing
	// run: `go test` executes only the seed corpus, so a corpus whose selectors
	// all leave bit 7 clear would never reach the AAL2 reader in CI.
	const aal2Sel = 0x80
	f.Add(uint8(2), []byte{0x00})
	f.Add(uint8(4), []byte{0x12, 0x34, 0x56, 0x78})
	f.Add(uint8(5), []byte{0xff, 0xff, 0xff, 0xff, 0xff})
	f.Add(uint8(aal2Sel|2), []byte{0x00})
	f.Add(uint8(aal2Sel|4), []byte{0x12, 0x34, 0x56, 0x78})
	f.Add(uint8(aal2Sel|5), []byte{0xff, 0xff, 0xff, 0xff, 0xff})
	// The conformance vectors are listed in fuzzRates order, so the index IS the
	// selector. Deriving it from the filename instead ("rate2bit" -> 2) would be
	// off by two under the selector's modulo: rate2bit.payload would be decoded at
	// 4 bits per codeword and rate5bit.payload at 3, where its length is not a
	// whole number of codewords and it is rejected before reaching a reader at all.
	for i, base := range []string{"rate2bit", "rate3bit", "rate4bit", "rate5bit"} {
		for _, v := range []struct {
			ext     string
			packing uint8
		}{
			{".payload", 0},
			{".aal2payload", aal2Sel},
		} {
			p, err := os.ReadFile(filepath.Join("testdata", base+v.ext))
			if err != nil {
				continue
			}
			// The vector at its own rate: real codewords, correctly aligned.
			f.Add(v.packing|uint8(i), p)
			// And deliberately at a different rate. Real audio decoded at the
			// wrong codeword width is still valid input to a decoder that must
			// never panic on it, and it reaches quantizer and predictor branches
			// the aligned vectors do not: either a length that is not a whole
			// number of codewords (the rejection path) or a misaligned codeword
			// stream driving the adaptation differently.
			f.Add(v.packing|uint8((i+2)%len(fuzzRates)), p)
		}
	}

	f.Fuzz(func(t *testing.T, sel uint8, payload []byte) {
		rate := fuzzRates[int(sel)%len(fuzzRates)]
		packing := audiostream.G726PackingRFC3551
		if sel&0x80 != 0 {
			packing = audiostream.G726PackingAAL2
		}
		d, err := g726.New(rate, packing)
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		// A payload that is not a whole number of codeword groups is malformed
		// (ErrIncompletePayload) and decodes to nothing; that is a valid outcome,
		// not a crash, so there is nothing further to assert for it.
		out, err := d.DecodeAlloc(payload)
		if errors.Is(err, g726.ErrIncompletePayload) {
			return
		}
		if err != nil {
			t.Fatalf("DecodeAlloc: %v", err)
		}
		if len(out)%2 != 0 {
			t.Fatalf("odd output length %d", len(out))
		}

		// Oversized destination guarded by canaries: Decode must not touch the
		// bytes past what it reports writing.
		d2, _ := g726.New(rate, packing)
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
