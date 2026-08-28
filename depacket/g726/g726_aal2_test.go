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

// loadAAL2Payload reads the AAL2 (most-significant-bit-first) conformance
// payload for a rate. It is the same audio, at the same bit rate, as the plain
// rateNbit.payload vector, encoded by a separate tool run into the opposite
// codeword packing; see testdata/README.md.
func loadAAL2Payload(t *testing.T, base string) []byte {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("testdata", base+".aal2payload"))
	if err != nil {
		t.Fatalf("read aal2 payload: %v", err)
	}
	return payload
}

// TestDecodeAAL2MatchesReference is the AAL2 conformance test, mirroring
// TestDecodeMatchesReference: each committed AAL2-packed payload must decode
// byte-for-byte to the SAME reference PCM as its RFC 3551 counterpart. The two
// packings carry an identical codeword sequence through an identical ADPCM state
// machine, so identical output is the defining property of a correct unpacker.
//
// The reference PCM is not regenerated for this test; it is the vector the plain
// RFC 3551 path is already pinned to, so a bug in the MSB-first reader cannot be
// masked by a matching bug in the expectation.
func TestDecodeAAL2MatchesReference(t *testing.T) {
	for _, rc := range rateCases {
		t.Run(rc.name, func(t *testing.T) {
			payload := loadAAL2Payload(t, rc.base)
			_, want := loadVector(t, rc.base)
			d, err := g726.New(rc.rate, audiostream.G726PackingAAL2)
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

// TestAAL2PayloadDiffersFromRFC3551 guards the premise of the conformance test
// above: the two committed vectors must be genuinely different byte streams of
// the same length. If a regeneration ever produced the same bytes for both, the
// AAL2 test would pass while proving nothing about the bit order.
func TestAAL2PayloadDiffersFromRFC3551(t *testing.T) {
	for _, rc := range rateCases {
		t.Run(rc.name, func(t *testing.T) {
			aal2 := loadAAL2Payload(t, rc.base)
			plain, _ := loadVector(t, rc.base)
			if len(aal2) != len(plain) {
				t.Fatalf("packing must not change payload length: aal2 %d, rfc3551 %d", len(aal2), len(plain))
			}
			if bytes.Equal(aal2, plain) {
				t.Fatal("the AAL2 and RFC 3551 vectors are byte-identical; one of them is mis-generated")
			}
		})
	}
}

// TestPackingMismatchProducesDifferentAudio proves the packing selection is
// load-bearing rather than cosmetic: decoding an AAL2 payload with an RFC 3551
// decoder (and the reverse) must NOT reproduce the reference PCM. This is what
// makes refusing to guess the packing worth doing, since the wrong order decodes
// without error into plausible but wrong audio.
func TestPackingMismatchProducesDifferentAudio(t *testing.T) {
	for _, rc := range rateCases {
		t.Run(rc.name, func(t *testing.T) {
			aal2 := loadAAL2Payload(t, rc.base)
			plain, want := loadVector(t, rc.base)

			wrongOnAAL2 := mustDecode(t, rc.rate, audiostream.G726PackingRFC3551, aal2)
			if bytes.Equal(wrongOnAAL2, want) {
				t.Fatal("an AAL2 payload decoded as RFC 3551 reproduced the reference PCM; the packing is not being applied")
			}

			wrongOnPlain := mustDecode(t, rc.rate, audiostream.G726PackingAAL2, plain)
			if bytes.Equal(wrongOnPlain, want) {
				t.Fatal("an RFC 3551 payload decoded as AAL2 reproduced the reference PCM; the packing is not being applied")
			}
		})
	}
}

// mustDecode builds a decoder for one rate and packing, decodes payload with it,
// and fails the test on any error, so callers read as a single expression.
func mustDecode(t *testing.T, rate audiostream.G726BitRate, packing audiostream.G726Packing, payload []byte) []byte {
	t.Helper()
	d, err := g726.New(rate, packing)
	if err != nil {
		t.Fatalf("New(%v, %v): %v", rate, packing, err)
	}
	out, err := d.DecodeAlloc(payload)
	if err != nil {
		t.Fatalf("DecodeAlloc(%v, %v): %v", rate, packing, err)
	}
	return out
}

func TestNewUnknownPacking(t *testing.T) {
	if _, err := g726.New(audiostream.G726Rate32, audiostream.G726Packing(99)); !errors.Is(err, g726.ErrUnknownPacking) {
		t.Fatalf("New(packing 99): got %v, want ErrUnknownPacking", err)
	}
}

// TestAAL2SplitEqualsWhole mirrors TestDecodeSplitEqualsWhole for the AAL2
// packing: the adaptive state must carry across calls identically, so a stream
// split on a whole-codeword boundary decodes to the same PCM as one call.
func TestAAL2SplitEqualsWhole(t *testing.T) {
	for _, rc := range rateCases {
		t.Run(rc.name, func(t *testing.T) {
			payload := loadAAL2Payload(t, rc.base)
			// Split near the midpoint on a whole-codeword boundary, matching the
			// RFC 3551 sibling. A fixed cut would slice out of range rather than
			// skip cleanly if the vectors were ever regenerated shorter, and
			// testdata/README.md documents a regeneration procedure.
			cut := (len(payload) / (2 * rc.bits)) * rc.bits
			if cut == 0 || cut >= len(payload) {
				t.Skipf("payload too short to split at a codeword boundary")
			}
			whole := mustDecode(t, rc.rate, audiostream.G726PackingAAL2, payload)
			d, err := g726.New(rc.rate, audiostream.G726PackingAAL2)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			first, err := d.DecodeAlloc(payload[:cut])
			if err != nil {
				t.Fatalf("first DecodeAlloc: %v", err)
			}
			second, err := d.DecodeAlloc(payload[cut:])
			if err != nil {
				t.Fatalf("second DecodeAlloc: %v", err)
			}
			if !bytes.Equal(append(first, second...), whole) {
				t.Fatal("split AAL2 decode differs from whole decode")
			}
		})
	}
}

// TestAAL2IncompletePayload pins that the RFC 3551 octet-completeness rule is a
// property of the codeword geometry, not of the bit order: an N-bit codeword
// stream fills a whole number of octets at the same lengths under either
// packing, so the AAL2 decoder rejects exactly the same payload lengths.
func TestAAL2IncompletePayload(t *testing.T) {
	for _, tc := range []struct {
		name          string
		rate          audiostream.G726BitRate
		badLen, okLen int
	}{
		{name24kbps, audiostream.G726Rate24, 4, 3},
		{name40kbps, audiostream.G726Rate40, 4, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := g726.New(tc.rate, audiostream.G726PackingAAL2)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, derr := d.DecodeAlloc(make([]byte, tc.badLen)); !errors.Is(derr, g726.ErrIncompletePayload) {
				t.Fatalf("DecodeAlloc(%d bytes): got %v, want ErrIncompletePayload", tc.badLen, derr)
			}
			// A rejected payload must leave the adaptive state untouched, exactly
			// as on the RFC 3551 path: a conformant payload decoded afterwards
			// must match a fresh decoder's output on the same bytes.
			good := make([]byte, tc.okLen)
			gotAfter, derr := d.DecodeAlloc(good)
			if derr != nil {
				t.Fatalf("DecodeAlloc(%d bytes): unexpected %v", tc.okLen, derr)
			}
			if want := mustDecode(t, tc.rate, audiostream.G726PackingAAL2, good); !bytes.Equal(gotAfter, want) {
				t.Error("a rejected payload advanced the adaptive state")
			}
		})
	}
}
