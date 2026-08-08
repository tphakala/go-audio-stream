package latm_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/tphakala/go-audio-stream/depacket/latm"
)

// V1 is the out-of-band StreamMuxConfig test vector (hand-verified,
// docs/plans/2026-07-23-phase2-latm-plan.md, "Hand-verified test vectors",
// vector V1): AAC-LC, 44.1 kHz, stereo, one subframe, frameLengthType 0.
// Decodes to ASC 12 10.
var V1 = []byte{0x40, 0x00, 0x24, 0x20, 0x3F, 0xC0}

// V2 is the out-of-band AudioMuxElement test vector paired with V1
// (docs/plans/2026-07-23-phase2-latm-plan.md, vector V2): MuxSlotLengthBytes
// 03, payload AA BB CC.
var V2 = []byte{0x03, 0xAA, 0xBB, 0xCC}

// buildAMEOutOfBand assembles a byte-aligned out-of-band AudioMuxElement
// (muxConfigPresent == 0) from raw PayloadLengthInfo byte sequences and
// their payload bytes, one pair per subframe. Each entry in lengths is the
// literal byte-summed length encoding (for example {0x03} for a length of
// 3, or {0xFF, 0x03} for the 255-escape sum yielding 258), so a test can
// exercise the escape path directly without hand-counting bytes.
func buildAMEOutOfBand(lengths, payloads [][]byte) []byte {
	var out []byte
	for i := range lengths {
		out = append(out, lengths[i]...)
		out = append(out, payloads[i]...)
	}
	return out
}

// newOutOfBand builds a Depacketizer in out-of-band mode with smc as the
// StreamMuxConfig, failing the test on a New error.
func newOutOfBand(t *testing.T, smc []byte) *latm.Depacketizer {
	t.Helper()
	d, err := latm.New(latm.Config{MuxConfigPresent: false, StreamMuxConfig: smc})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

func TestOutOfBandSingleAU(t *testing.T) {
	t.Parallel()
	d := newOutOfBand(t, V1)

	aus, err := d.Depacketize(V2, true, 1000)
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}
	if len(aus) != 1 {
		t.Fatalf("AU count = %d, want 1", len(aus))
	}
	want := []byte{0xAA, 0xBB, 0xCC}
	if !bytes.Equal(aus[0].Data, want) {
		t.Errorf("Data = % x, want % x", aus[0].Data, want)
	}
	if aus[0].RTPOffset != 0 {
		t.Errorf("RTPOffset = %d, want 0", aus[0].RTPOffset)
	}

	asc := d.AudioSpecificConfig()
	wantASC := []byte{0x12, 0x10}
	if !bytes.Equal(asc, wantASC) {
		t.Errorf("AudioSpecificConfig = % x, want % x", asc, wantASC)
	}
}

func TestOutOfBandLengthEscape(t *testing.T) {
	t.Parallel()
	d := newOutOfBand(t, V1)

	payload := bytes.Repeat([]byte{0x77}, 258)
	pkt := buildAMEOutOfBand([][]byte{{0xFF, 0x03}}, [][]byte{payload})

	aus, err := d.Depacketize(pkt, true, 1000)
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}
	if len(aus) != 1 {
		t.Fatalf("AU count = %d, want 1", len(aus))
	}
	if !bytes.Equal(aus[0].Data, payload) {
		t.Errorf("Data length = %d, want %d", len(aus[0].Data), len(payload))
	}
}

func TestOutOfBandPayloadOverflow(t *testing.T) {
	t.Parallel()
	d := newOutOfBand(t, V1)

	// MuxSlotLengthBytes 10 (0x0A), but only 4 payload bytes follow.
	pkt := buildAMEOutOfBand([][]byte{{0x0A}}, [][]byte{{0x01, 0x02, 0x03, 0x04}})
	_, err := d.Depacketize(pkt, true, 1000)
	if !errors.Is(err, latm.ErrPayloadOverflow) {
		t.Fatalf("err = %v, want ErrPayloadOverflow", err)
	}
}

func TestOutOfBandTruncated(t *testing.T) {
	t.Parallel()

	t.Run("empty payload", func(t *testing.T) {
		t.Parallel()
		d := newOutOfBand(t, V1)
		_, err := d.Depacketize(nil, true, 1000)
		if !errors.Is(err, latm.ErrTruncated) {
			t.Fatalf("err = %v, want ErrTruncated", err)
		}
	})

	t.Run("length byte with no data", func(t *testing.T) {
		t.Parallel()
		d := newOutOfBand(t, V1)
		// Only the length byte (declares 3 bytes of payload); none follow.
		_, err := d.Depacketize([]byte{0x03}, true, 1000)
		if !errors.Is(err, latm.ErrPayloadOverflow) {
			t.Fatalf("err = %v, want ErrPayloadOverflow", err)
		}
	})
}

func TestConfigRejection(t *testing.T) {
	t.Parallel()

	t.Run("nil StreamMuxConfig", func(t *testing.T) {
		t.Parallel()
		_, err := latm.New(latm.Config{MuxConfigPresent: false, StreamMuxConfig: nil})
		if !errors.Is(err, latm.ErrConfigInvalid) {
			t.Fatalf("err = %v, want ErrConfigInvalid", err)
		}
	})

	t.Run("empty StreamMuxConfig", func(t *testing.T) {
		t.Parallel()
		_, err := latm.New(latm.Config{MuxConfigPresent: false, StreamMuxConfig: []byte{}})
		if !errors.Is(err, latm.ErrConfigInvalid) {
			t.Fatalf("err = %v, want ErrConfigInvalid", err)
		}
	})

	t.Run("numProgram nonzero", func(t *testing.T) {
		t.Parallel()
		// V1 with numProgram set to 1 instead of 0 (byte1: 0000 -> 0001 in
		// the high nibble; numLayer and the ASC lead bit are unaffected).
		badV1 := []byte{0x40, 0x10, 0x24, 0x20, 0x3F, 0xC0}
		_, err := latm.New(latm.Config{MuxConfigPresent: false, StreamMuxConfig: badV1})
		if !errors.Is(err, latm.ErrUnsupportedMux) {
			t.Fatalf("err = %v, want ErrUnsupportedMux", err)
		}
	})
}
