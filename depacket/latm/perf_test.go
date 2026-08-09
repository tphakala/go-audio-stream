package latm

import (
	"bytes"
	"errors"
	"testing"
)

// writeInBandExplicitRateHeader writes useSameStreamMux 0 followed by a
// single-subframe StreamMuxConfig whose ASC uses samplingFrequencyIndex 15
// (the escape code for an explicit 24-bit sampling rate, here 48000 Hz =
// 0x00BB80). The extracted ASC is therefore 5 bytes (17 80 5D C0 10, the
// same value the out-of-band v1ExplicitRate vector documents) instead of
// v4's 2 bytes, so alternating the two across packets exercises the ascBuf
// reuse path both growing and shrinking the retained ASC.
func writeInBandExplicitRateHeader(w *bitWriter) {
	w.write(0, 1)         // useSameStreamMux
	w.write(0, 1)         // audioMuxVersion
	w.write(1, 1)         // allStreamsSameTimeFraming
	w.write(0, 6)         // numSubFrames
	w.write(0, 4)         // numProgram
	w.write(0, 3)         // numLayer
	w.write(2, 5)         // audioObjectType (AAC-LC)
	w.write(15, 4)        // samplingFrequencyIndex (escape to explicit rate)
	w.write(0x00BB80, 24) // explicit samplingFrequency 48000 Hz
	w.write(2, 4)         // channelConfiguration
	w.write(0, 1)         // frameLengthFlag
	w.write(0, 1)         // dependsOnCoreCoder
	w.write(0, 1)         // extensionFlag
	w.write(0, 3)         // frameLengthType
	w.write(0xFF, 8)      // latmBufferFullness
	w.write(0, 1)         // otherDataPresent
	w.write(0, 1)         // crcCheckPresent
}

// buildInBandExplicitRate builds a single-subframe in-band AudioMuxElement
// carrying the explicit-rate StreamMuxConfig (5-byte ASC 17 80 5D C0 10) and
// a two-byte payload DE AD.
func buildInBandExplicitRate() []byte {
	w := &bitWriter{}
	writeInBandExplicitRateHeader(w)
	w.write(2, 8) // PayloadLengthInfo
	w.write(0xDE, 8)
	w.write(0xAD, 8)
	w.byteAlign()
	return w.bytes()
}

// ascV4 is v4's extracted ASC (AAC-LC, 44.1 kHz, stereo).
var ascV4 = []byte{0x12, 0x10}

// ascExplicitRate is buildInBandExplicitRate's extracted ASC (AAC-LC,
// explicit 48 kHz, stereo).
var ascExplicitRate = []byte{0x17, 0x80, 0x5D, 0xC0, 0x10}

// TestInBandResendConfigReuseSameASC drives the useSameStreamMux 0 path
// (encoder resends the full StreamMuxConfig every packet) repeatedly with
// the same vector and asserts every call yields the same AU and ASC. This
// is the path the ascBuf reuse targets: the retained ASC is re-extracted
// into the shared scratch buffer on every packet, so a reuse that failed to
// re-zero or mis-sized the buffer would corrupt a later result.
func TestInBandResendConfigReuseSameASC(t *testing.T) {
	t.Parallel()
	d, err := New(Config{MuxConfigPresent: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	wantAU := []byte{0xAA, 0xBB, 0xCC}
	for i := range 8 {
		aus, err := d.Depacketize(v4, true, 0)
		if err != nil {
			t.Fatalf("Depacketize #%d: %v", i, err)
		}
		if len(aus) != 1 || !bytes.Equal(aus[0].Data, wantAU) {
			t.Fatalf("#%d AU = % x, want % x", i, aus, wantAU)
		}
		if asc := d.AudioSpecificConfig(); !bytes.Equal(asc, ascV4) {
			t.Fatalf("#%d ASC = % x, want % x", i, asc, ascV4)
		}
	}
}

// TestInBandResendConfigASCSizeChange drives resend-config packets whose ASC
// changes size across calls, so the reused ascBuf must reslice and re-zero
// correctly. The step order is load-bearing: because of the double buffer's
// swap the dst passed on packet k is the buffer last written on packet k-2, so a
// strict 2,5,2,5 alternation would only ever reuse a backing array to reproduce
// the identical ASC it already held and would never exercise the re-zero. The
// v4, explicit, explicit, v4 order instead reuses a 5-byte backing (last held
// 17 80 5D C0 10) for the final 2-byte ASC: without the re-zero in
// extractBitsInto that call would return 17 90 (the stale bytes OR-ed with
// 12 10) rather than 12 10.
func TestInBandResendConfigASCSizeChange(t *testing.T) {
	t.Parallel()
	d, err := New(Config{MuxConfigPresent: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	explicit := buildInBandExplicitRate()
	explicitAU := []byte{0xDE, 0xAD}
	v4AU := []byte{0xAA, 0xBB, 0xCC}

	steps := []struct {
		name    string
		payload []byte
		wantAU  []byte
		wantASC []byte
	}{
		{"v4-first", v4, v4AU, ascV4},
		{"grow-to-explicit", explicit, explicitAU, ascExplicitRate},
		{"explicit-again", explicit, explicitAU, ascExplicitRate},
		{"shrink-reusing-5byte-buffer", v4, v4AU, ascV4},
	}
	for _, s := range steps {
		aus, err := d.Depacketize(s.payload, true, 0)
		if err != nil {
			t.Fatalf("%s: Depacketize: %v", s.name, err)
		}
		if len(aus) != 1 || !bytes.Equal(aus[0].Data, s.wantAU) {
			t.Fatalf("%s: AU = % x, want % x", s.name, aus, s.wantAU)
		}
		if asc := d.AudioSpecificConfig(); !bytes.Equal(asc, s.wantASC) {
			t.Fatalf("%s: ASC = % x, want % x", s.name, asc, s.wantASC)
		}
	}
}

// TestInBandResendConfigZeroAlloc asserts the useSameStreamMux 0 steady state
// is allocation-free. The reused scratch buffers warm at different rates: aus
// and inBandData size on the first packet, but the ASC double buffer needs two
// (each of the first two resend-config packets allocates one half), so the
// steady state begins at the third packet. The explicit warmup below reaches it
// before AllocsPerRun measures. This test deliberately does not call t.Parallel:
// testing.AllocsPerRun reads process-wide malloc counters, so concurrent
// allocation from a sibling test would perturb the measurement.
func TestInBandResendConfigZeroAlloc(t *testing.T) {
	d, err := New(Config{MuxConfigPresent: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Warm both halves of the ASC double buffer plus aus/inBandData, so the
	// measurement below reflects only the steady state.
	for range 3 {
		if _, err := d.Depacketize(v4, true, 0); err != nil {
			t.Fatalf("warmup Depacketize: %v", err)
		}
	}
	if got := testing.AllocsPerRun(100, func() {
		if _, err := d.Depacketize(v4, true, 0); err != nil {
			t.Fatalf("Depacketize: %v", err)
		}
	}); got != 0 {
		t.Errorf("allocs/op = %v, want 0", got)
	}
}

// BenchmarkDepacketizeInBandResendConfig measures the useSameStreamMux 0
// path, where every AudioMuxElement re-carries the full StreamMuxConfig and
// the ASC is re-extracted each packet.
func BenchmarkDepacketizeInBandResendConfig(b *testing.B) {
	d, err := New(Config{MuxConfigPresent: true})
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(v4)))
	for b.Loop() {
		if _, err := d.Depacketize(v4, true, 0); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDepacketizeInBandSameMux measures the useSameStreamMux 1 steady
// state (the config is retained, not re-parsed) as a control: it already
// touches no ASC extraction, so it isolates the resend path's extra cost.
func BenchmarkDepacketizeInBandSameMux(b *testing.B) {
	d, err := New(Config{MuxConfigPresent: true})
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	if _, err := d.Depacketize(v4, true, 0); err != nil {
		b.Fatalf("Depacketize(v4): %v", err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(v5)))
	for b.Loop() {
		if _, err := d.Depacketize(v5, true, 0); err != nil {
			b.Fatal(err)
		}
	}
}

// buildInBandResendBadFrameLengthType builds a resend-config (useSameStreamMux
// 0) in-band AudioMuxElement whose AudioSpecificConfig parses cleanly (so
// parseASC packs the scratch buffer in place, extracting 12 10) but whose
// frameLengthType is 1, which parseStreamMuxConfigBits rejects with
// ErrUnsupportedMux immediately after the ASC. It is the mid-parse failure the
// ascBuf double buffer exists to contain: the in-place scratch write must not
// reach the retained config.
func buildInBandResendBadFrameLengthType() []byte {
	w := &bitWriter{}
	w.write(0, 1) // useSameStreamMux
	w.write(0, 1) // audioMuxVersion
	w.write(1, 1) // allStreamsSameTimeFraming
	w.write(0, 6) // numSubFrames
	w.write(0, 4) // numProgram
	w.write(0, 3) // numLayer
	w.write(2, 5) // audioObjectType (AAC-LC): a valid ASC extracting to 12 10
	w.write(4, 4) // samplingFrequencyIndex (44100)
	w.write(2, 4) // channelConfiguration
	w.write(0, 1) // frameLengthFlag
	w.write(0, 1) // dependsOnCoreCoder
	w.write(0, 1) // extensionFlag
	w.write(1, 3) // frameLengthType = 1 -> ErrUnsupportedMux, AFTER the ASC extract
	w.byteAlign()
	return w.bytes()
}

// TestInBandResendConfigMidParseFailurePreservesConfig is the regression test
// for the double buffer's reason to exist: a resend-config packet that fails
// AFTER parseASC has already written the scratch buffer must leave the retained
// ASC byte-for-byte intact. The two explicit-rate packets first are
// load-bearing: they leave d.ascBuf a live 5-byte buffer, so the failing
// packet's parseASC mutates that buffer in place rather than a fresh
// allocation. The failing packet carries a DIFFERENT (2-byte) ASC, so a
// single-buffer design that aliased the scratch onto d.asc would visibly
// corrupt the retained 5-byte ASC here.
func TestInBandResendConfigMidParseFailurePreservesConfig(t *testing.T) {
	t.Parallel()
	d, err := New(Config{MuxConfigPresent: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	explicit := buildInBandExplicitRate()
	if _, err := d.Depacketize(explicit, true, 0); err != nil {
		t.Fatalf("Depacketize(explicit #1): %v", err)
	}
	if _, err := d.Depacketize(explicit, true, 0); err != nil {
		t.Fatalf("Depacketize(explicit #2): %v", err)
	}
	if asc := d.AudioSpecificConfig(); !bytes.Equal(asc, ascExplicitRate) {
		t.Fatalf("retained ASC before failure = % x, want % x", asc, ascExplicitRate)
	}

	// The failing packet: a valid 2-byte ASC, then frameLengthType 1.
	if _, err := d.Depacketize(buildInBandResendBadFrameLengthType(), true, 0); !errors.Is(err, ErrUnsupportedMux) {
		t.Fatalf("err = %v, want ErrUnsupportedMux", err)
	}
	if asc := d.AudioSpecificConfig(); !bytes.Equal(asc, ascExplicitRate) {
		t.Fatalf("retained ASC corrupted by failed parse = % x, want % x", asc, ascExplicitRate)
	}

	// Positive control: the retained config still decodes a same-mux packet.
	aus, err := d.Depacketize(v5, true, 0)
	if err != nil {
		t.Fatalf("Depacketize(v5) after failed resend: %v", err)
	}
	if len(aus) != 1 || !bytes.Equal(aus[0].Data, []byte{0xAA, 0xBB, 0xCC}) {
		t.Fatalf("same-mux AU after failed resend = % x, want AA BB CC", aus)
	}
}
