package latm

import (
	"bytes"
	"errors"
	"testing"
)

// v3 is the out-of-band StreamMuxConfig test vector (hand-verified,
// docs/plans/2026-07-23-phase2-latm-plan.md, vector V3): AAC-LC, 44.1 kHz,
// stereo, numSubFrames 1 (two access units per AudioMuxElement),
// frameLengthType 0. Same ASC as v1 (12 10), frameLength 1024.
var v3 = []byte{0x41, 0x00, 0x24, 0x20, 0x3F, 0xC0}

// v3Payload is the out-of-band AudioMuxElement paired with v3 (vector V3):
// two subframes, PayloadLengthInfo 02 / payload 11 22, PayloadLengthInfo 03
// / payload 33 44 55.
var v3Payload = []byte{0x02, 0x11, 0x22, 0x03, 0x33, 0x44, 0x55}

// v3PayloadTruncated is v3Payload with the last byte dropped: the second
// subframe declares a 3-byte payload (PayloadLengthInfo 03) but only 2
// bytes (33 44) follow.
var v3PayloadTruncated = []byte{0x02, 0x11, 0x22, 0x03, 0x33, 0x44}

func TestOutOfBandTwoSubframes(t *testing.T) {
	t.Parallel()
	d, err := New(Config{MuxConfigPresent: false, StreamMuxConfig: v3})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	aus, err := d.Depacketize(v3Payload, true, 0)
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}
	if len(aus) != 2 {
		t.Fatalf("AU count = %d, want 2", len(aus))
	}
	wantAU0 := []byte{0x11, 0x22}
	wantAU1 := []byte{0x33, 0x44, 0x55}
	if !bytes.Equal(aus[0].Data, wantAU0) {
		t.Errorf("aus[0].Data = % x, want % x", aus[0].Data, wantAU0)
	}
	if aus[0].RTPOffset != 0 {
		t.Errorf("aus[0].RTPOffset = %d, want 0", aus[0].RTPOffset)
	}
	if !bytes.Equal(aus[1].Data, wantAU1) {
		t.Errorf("aus[1].Data = % x, want % x", aus[1].Data, wantAU1)
	}
	if aus[1].RTPOffset != 1024 {
		t.Errorf("aus[1].RTPOffset = %d, want 1024", aus[1].RTPOffset)
	}
}

func TestSamplesPerFrameOverride(t *testing.T) {
	t.Parallel()
	d, err := New(Config{MuxConfigPresent: false, StreamMuxConfig: v3, SamplesPerFrame: 960})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	aus, err := d.Depacketize(v3Payload, true, 0)
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}
	if len(aus) != 2 {
		t.Fatalf("AU count = %d, want 2", len(aus))
	}
	if aus[0].RTPOffset != 0 {
		t.Errorf("aus[0].RTPOffset = %d, want 0", aus[0].RTPOffset)
	}
	if aus[1].RTPOffset != 960 {
		t.Errorf("aus[1].RTPOffset = %d, want 960", aus[1].RTPOffset)
	}
}

// bitWriter packs bits MSB-first into a byte slice, the write-side mirror of
// bitReader. Test-only helper for constructing in-band AudioMuxElement
// bitstreams field by field, so a multi-subframe test vector can be built
// from named field values instead of hand-computed hex.
type bitWriter struct {
	buf  []byte
	nbit int // bits already written into the last byte of buf, 0..7
}

// write appends the low n bits of v (0 <= n <= 32), MSB first.
func (w *bitWriter) write(v uint64, n int) {
	for i := n - 1; i >= 0; i-- {
		if w.nbit == 0 {
			w.buf = append(w.buf, 0)
		}
		if (v>>uint(i))&1 == 1 {
			w.buf[len(w.buf)-1] |= 1 << uint(7-w.nbit)
		}
		w.nbit = (w.nbit + 1) % 8
	}
}

// byteAlign marks the writer aligned to the next byte boundary. A partial
// last byte's unwritten bits are already zero (a freshly appended byte
// starts zero-valued), so this only resets the bit counter; the next write
// starts a fresh byte instead of continuing to fill the current one.
func (w *bitWriter) byteAlign() {
	w.nbit = 0
}

// bytes returns the packed byte slice built so far.
func (w *bitWriter) bytes() []byte {
	return w.buf
}

// writeV3Header writes useSameStreamMux 0 followed by a StreamMuxConfig
// whose fields match v3 (41 00 24 20 3F C0) exactly: audioMuxVersion 0,
// allStreamsSameTimeFraming 1, numSubFrames 1, numProgram 0, numLayer 0,
// audioObjectType 2 (AAC-LC), samplingFrequencyIndex 4 (44100 Hz),
// channelConfiguration 2, frameLengthFlag 0, dependsOnCoreCoder 0,
// extensionFlag 0, frameLengthType 0, latmBufferFullness 0xFF,
// otherDataPresent 0, crcCheckPresent 0.
func writeV3Header(w *bitWriter) {
	w.write(0, 1)    // useSameStreamMux
	w.write(0, 1)    // audioMuxVersion
	w.write(1, 1)    // allStreamsSameTimeFraming
	w.write(1, 6)    // numSubFrames
	w.write(0, 4)    // numProgram
	w.write(0, 3)    // numLayer
	w.write(2, 5)    // audioObjectType
	w.write(4, 4)    // samplingFrequencyIndex
	w.write(2, 4)    // channelConfiguration
	w.write(0, 1)    // frameLengthFlag
	w.write(0, 1)    // dependsOnCoreCoder
	w.write(0, 1)    // extensionFlag
	w.write(0, 3)    // frameLengthType
	w.write(0xFF, 8) // latmBufferFullness
	w.write(0, 1)    // otherDataPresent
	w.write(0, 1)    // crcCheckPresent
}

// buildInBandTwoSubframes packs an in-band AudioMuxElement equivalent to
// v3/v3Payload: the v3 StreamMuxConfig fields, then PayloadLengthInfo 02 /
// payload 11 22, then PayloadLengthInfo 03 / payload 33 44 55.
func buildInBandTwoSubframes() []byte {
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
	return w.bytes()
}

// buildInBandTwoSubframesTruncated is buildInBandTwoSubframes with the last
// payload byte (0x55) dropped: the second subframe declares a 3-byte
// payload but only 2 bytes follow.
func buildInBandTwoSubframesTruncated() []byte {
	w := &bitWriter{}
	writeV3Header(w)
	w.write(2, 8)
	w.write(0x11, 8)
	w.write(0x22, 8)
	w.write(3, 8)
	w.write(0x33, 8)
	w.write(0x44, 8)
	return w.bytes()
}

func TestInBandTwoSubframes(t *testing.T) {
	t.Parallel()
	d, err := New(Config{MuxConfigPresent: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	aus, err := d.Depacketize(buildInBandTwoSubframes(), true, 0)
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}
	if len(aus) != 2 {
		t.Fatalf("AU count = %d, want 2", len(aus))
	}
	wantAU0 := []byte{0x11, 0x22}
	wantAU1 := []byte{0x33, 0x44, 0x55}
	if !bytes.Equal(aus[0].Data, wantAU0) {
		t.Errorf("aus[0].Data = % x, want % x", aus[0].Data, wantAU0)
	}
	if aus[0].RTPOffset != 0 {
		t.Errorf("aus[0].RTPOffset = %d, want 0", aus[0].RTPOffset)
	}
	if !bytes.Equal(aus[1].Data, wantAU1) {
		t.Errorf("aus[1].Data = % x, want % x", aus[1].Data, wantAU1)
	}
	if aus[1].RTPOffset != 1024 {
		t.Errorf("aus[1].RTPOffset = %d, want 1024", aus[1].RTPOffset)
	}

	// Distinct-region correctness: aus[0].Data and aus[1].Data are both
	// repacked into the shared, reused d.inBandData buffer. If the second
	// subframe were packed over the top of the first instead of into its
	// own region, mutating aus[1].Data would corrupt aus[0].Data. Prove
	// they occupy disjoint memory.
	for i := range aus[1].Data {
		aus[1].Data[i] = 0xFF
	}
	if !bytes.Equal(aus[0].Data, wantAU0) {
		t.Errorf("aus[0].Data changed after mutating aus[1].Data: got % x, want % x (buffer regions alias)", aus[0].Data, wantAU0)
	}
}

func TestSubframeCountCap(t *testing.T) {
	t.Parallel()

	// numSubFrames is a plain 6-bit StreamMuxConfig field (0..63), so
	// numSubFrames+1 tops out at 64, exactly MaxSubFrames: like the
	// parse-time guard in parseStreamMuxConfigBits (see the "structural
	// no-op" comment there), this cap is unreachable through any real
	// bitstream, out-of-band or in-band. Both subtests force the retained
	// smc past the cap directly (this file is package latm) to exercise
	// the defensive guard Depacketize/depacketizeInBand apply before
	// allocating any per-subframe AU, since New's public parse path can
	// never drive numSubFrames there.

	t.Run("out-of-band", func(t *testing.T) {
		t.Parallel()
		d, err := New(Config{MuxConfigPresent: false, StreamMuxConfig: v3})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		d.smc.numSubFrames = MaxSubFrames
		_, err = d.Depacketize(v3Payload, true, 0)
		if !errors.Is(err, ErrUnsupportedMux) {
			t.Fatalf("err = %v, want ErrUnsupportedMux", err)
		}
	})

	t.Run("in-band", func(t *testing.T) {
		t.Parallel()
		d, err := New(Config{MuxConfigPresent: true})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := d.Depacketize(buildInBandTwoSubframes(), true, 0); err != nil {
			t.Fatalf("Depacketize (learn config): %v", err)
		}
		d.smc.numSubFrames = MaxSubFrames

		// useSameStreamMux 1, reusing the now-poisoned retained config; the
		// guard must fire before the loop reads any subframe data.
		_, err = d.Depacketize([]byte{0x80}, true, 0)
		if !errors.Is(err, ErrUnsupportedMux) {
			t.Fatalf("err = %v, want ErrUnsupportedMux", err)
		}
	})
}

// writeV1HeaderInBand writes useSameStreamMux 0 followed by a StreamMuxConfig
// whose fields match v1 (a single subframe): the same layout writeV3Header
// packs but with numSubFrames 0, so exactly one PayloadLengthInfo/payload pair
// follows.
func writeV1HeaderInBand(w *bitWriter) {
	w.write(0, 1)    // useSameStreamMux
	w.write(0, 1)    // audioMuxVersion
	w.write(1, 1)    // allStreamsSameTimeFraming
	w.write(0, 6)    // numSubFrames
	w.write(0, 4)    // numProgram
	w.write(0, 3)    // numLayer
	w.write(2, 5)    // audioObjectType
	w.write(4, 4)    // samplingFrequencyIndex
	w.write(2, 4)    // channelConfiguration
	w.write(0, 1)    // frameLengthFlag
	w.write(0, 1)    // dependsOnCoreCoder
	w.write(0, 1)    // extensionFlag
	w.write(0, 3)    // frameLengthType
	w.write(0xFF, 8) // latmBufferFullness
	w.write(0, 1)    // otherDataPresent
	w.write(0, 1)    // crcCheckPresent
}

// buildInBandLengthEscape packs a single-subframe in-band AudioMuxElement whose
// PayloadLengthInfo uses the 0xFF continuation byte-sum to declare payloadLen
// payload bytes: floor(payloadLen/255) bytes of 0xFF, then payloadLen mod 255
// as the terminating byte, then payloadLen payload bytes 00 01 02 ... A
// payloadLen >= 255 forces at least one 0xFF continuation.
func buildInBandLengthEscape(payloadLen int) []byte {
	w := &bitWriter{}
	writeV1HeaderInBand(w)
	for rem := payloadLen; ; {
		if rem >= 255 {
			w.write(0xFF, 8)
			rem -= 255
			continue
		}
		w.write(uint64(rem), 8)
		break
	}
	for i := range payloadLen {
		w.write(uint64(i&0xFF), 8)
	}
	w.byteAlign()
	return w.bytes()
}

// buildInBandTruncatedLength packs a single-subframe in-band AudioMuxElement
// whose PayloadLengthInfo ends on a 0xFF continuation byte with nothing after
// it, so the byte-sum read runs off the end of the element mid-length.
func buildInBandTruncatedLength() []byte {
	w := &bitWriter{}
	writeV1HeaderInBand(w)
	w.write(0xFF, 8) // continuation byte, but the element ends here.
	w.byteAlign()
	return w.bytes()
}

func TestInBandLengthEscape(t *testing.T) {
	t.Parallel()
	const payloadLen = 300 // > 255, so PayloadLengthInfo needs a 0xFF continuation.
	d, err := New(Config{MuxConfigPresent: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	aus, err := d.Depacketize(buildInBandLengthEscape(payloadLen), true, 0)
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}
	if len(aus) != 1 {
		t.Fatalf("AU count = %d, want 1", len(aus))
	}
	if len(aus[0].Data) != payloadLen {
		t.Fatalf("AU length = %d, want %d (0xFF length-escape byte-sum)", len(aus[0].Data), payloadLen)
	}
	for i := range aus[0].Data {
		if want := byte(i & 0xFF); aus[0].Data[i] != want {
			t.Fatalf("AU byte %d = %#x, want %#x", i, aus[0].Data[i], want)
		}
	}
}

func TestInBandLengthEscapeTruncated(t *testing.T) {
	t.Parallel()
	d, err := New(Config{MuxConfigPresent: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = d.Depacketize(buildInBandTruncatedLength(), true, 0)
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
}

func TestMultiSubframeTruncated(t *testing.T) {
	t.Parallel()

	t.Run("out-of-band", func(t *testing.T) {
		t.Parallel()
		d, err := New(Config{MuxConfigPresent: false, StreamMuxConfig: v3})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		_, err = d.Depacketize(v3PayloadTruncated, true, 0)
		if !errors.Is(err, ErrPayloadOverflow) {
			t.Fatalf("err = %v, want ErrPayloadOverflow", err)
		}
	})

	t.Run("in-band", func(t *testing.T) {
		t.Parallel()
		d, err := New(Config{MuxConfigPresent: true})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		_, err = d.Depacketize(buildInBandTwoSubframesTruncated(), true, 0)
		if !errors.Is(err, ErrPayloadOverflow) {
			t.Fatalf("err = %v, want ErrPayloadOverflow", err)
		}
	})
}
