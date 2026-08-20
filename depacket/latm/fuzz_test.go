package latm

import "testing"

// truncs returns buf itself followed by its prefixes at length 0, 1, half,
// and len(buf)-1 (skipping any length that is not strictly smaller than
// len(buf)), so a fuzz target seeded from a hand-verified vector also gets
// seeded with truncations of it.
func truncs(buf []byte) [][]byte {
	out := [][]byte{buf}
	for _, n := range []int{0, 1, len(buf) / 2, len(buf) - 1} {
		if n >= 0 && n < len(buf) {
			out = append(out, buf[:n])
		}
	}
	return out
}

// buildStreamMuxConfigCoreCoderDelay is a StreamMuxConfig whose ASC sets
// dependsOnCoreCoder to 1 with coreCoderDelay 0x123, the one parseASC branch
// the hand-verified v1/v3 vectors never exercise. Built field by field with
// the bitWriter from multisubframe_test.go (same package) rather than
// hand-computed hex, mirroring writeV3Header there. Otherwise identical to
// v1: AAC-LC, 44.1 kHz, stereo, one subframe, frameLengthType 0.
func buildStreamMuxConfigCoreCoderDelay() []byte {
	w := &bitWriter{}
	w.write(0, 1)      // audioMuxVersion
	w.write(1, 1)      // allStreamsSameTimeFraming
	w.write(0, 6)      // numSubFrames
	w.write(0, 4)      // numProgram
	w.write(0, 3)      // numLayer
	w.write(2, 5)      // audioObjectType (AAC-LC)
	w.write(4, 4)      // samplingFrequencyIndex (44100 Hz)
	w.write(2, 4)      // channelConfiguration
	w.write(0, 1)      // frameLengthFlag
	w.write(1, 1)      // dependsOnCoreCoder
	w.write(0x123, 14) // coreCoderDelay
	w.write(0, 1)      // extensionFlag
	w.write(0, 3)      // frameLengthType
	w.write(0xFF, 8)   // latmBufferFullness
	w.write(0, 1)      // otherDataPresent
	w.write(0, 1)      // crcCheckPresent
	w.byteAlign()
	return w.bytes()
}

// FuzzParseStreamMuxConfig drives parseStreamMuxConfig directly with
// arbitrary bytes. It must never panic, and on success the returned ASC must
// fit within MaxStreamMuxConfigBytes (parseStreamMuxConfig caps its input to
// that many bytes before parsing) and the retained numSubFrames+1 must fit
// within MaxSubFrames (parseStreamMuxConfigBits rejects a larger value before
// returning).
func FuzzParseStreamMuxConfig(f *testing.F) {
	for _, buf := range truncs(v1) {
		f.Add(buf)
	}
	for _, buf := range truncs(v3) {
		f.Add(buf)
	}
	for _, buf := range truncs(buildStreamMuxConfigCoreCoderDelay()) {
		f.Add(buf)
	}
	f.Fuzz(func(t *testing.T, buf []byte) {
		smc, asc, _, err := parseStreamMuxConfig(buf)
		if err != nil {
			return
		}
		if len(asc) > MaxStreamMuxConfigBytes {
			t.Fatalf("asc length %d exceeds MaxStreamMuxConfigBytes %d", len(asc), MaxStreamMuxConfigBytes)
		}
		if smc.numSubFrames+1 > MaxSubFrames {
			t.Fatalf("numSubFrames+1 %d exceeds MaxSubFrames %d", smc.numSubFrames+1, MaxSubFrames)
		}
	})
}

// assertAUInvariants checks the invariants every success-path Depacketize
// return must satisfy, so the fuzz targets assert more than no-panic: the AU
// count fits MaxMuxElements*MaxSubFrames (a payload may carry several
// AudioMuxElements, each up to MaxSubFrames subframes), each AU fits
// MaxMuxSlotBytes, the first RTPOffset is 0, and offsets are non-decreasing.
// Offsets accumulate across subframes and elements (each subframe advances by the
// element's per-frame tick count, each element continues from the previous one's
// span), so they rise monotonically but not necessarily by a single constant step
// once elements with differing frame lengths appear in one payload.
func assertAUInvariants(t *testing.T, aus []AU) {
	t.Helper()
	if len(aus) > MaxMuxElements*MaxSubFrames {
		t.Fatalf("AU count %d exceeds MaxMuxElements*MaxSubFrames %d", len(aus), MaxMuxElements*MaxSubFrames)
	}
	if len(aus) == 0 {
		return
	}
	if aus[0].RTPOffset != 0 {
		t.Fatalf("aus[0].RTPOffset = %d, want 0", aus[0].RTPOffset)
	}
	for i := range aus {
		if len(aus[i].Data) > MaxMuxSlotBytes {
			t.Fatalf("aus[%d].Data length %d exceeds MaxMuxSlotBytes %d", i, len(aus[i].Data), MaxMuxSlotBytes)
		}
		if i > 0 && aus[i].RTPOffset < aus[i-1].RTPOffset {
			t.Fatalf("aus[%d].RTPOffset = %d < aus[%d].RTPOffset = %d (offsets must be non-decreasing)", i, aus[i].RTPOffset, i-1, aus[i-1].RTPOffset)
		}
	}
}

// FuzzDepacketizeOutOfBand drives the out-of-band Depacketize path with a
// fixed valid StreamMuxConfig (v1) and arbitrary AudioMuxElement bytes and
// marker bit. It must never panic, and on the success path the returned AUs
// must satisfy assertAUInvariants.
func FuzzDepacketizeOutOfBand(f *testing.F) {
	// v2 is the out-of-band AudioMuxElement paired with v1 (see V2 in
	// latm_test.go): MuxSlotLengthBytes 03, payload AA BB CC.
	v2 := []byte{0x03, 0xAA, 0xBB, 0xCC}
	for _, buf := range truncs(v2) {
		f.Add(buf, true)
	}
	// Two concatenated AudioMuxElements: a multi-element payload seed.
	f.Add(append(append([]byte{}, v2...), v2...), true)
	f.Fuzz(func(t *testing.T, payload []byte, marker bool) {
		// A fresh depacketizer per input so each execution owns its buffers and
		// cannot be influenced by state a prior input left behind.
		d, err := New(Config{MuxConfigPresent: false, StreamMuxConfig: v1})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		aus, err := d.Depacketize(payload, marker, 0)
		if err != nil {
			return
		}
		assertAUInvariants(t, aus)
	})
}

// FuzzDepacketizeInBand drives the in-band Depacketize path across a
// two-call sequence with arbitrary payloads and marker bits, exercising the
// retained-config path and useSameStreamMux. It must never panic on either
// call, including the second call against whatever state the first call left
// behind, and each call's success-path AUs must satisfy assertAUInvariants.
// d.Reset() at the top of each fuzz execution keeps iterations independent of
// one another, mirroring depacket/aac's FuzzDepacketize.
func FuzzDepacketizeInBand(f *testing.F) {
	d, err := New(Config{MuxConfigPresent: true})
	if err != nil {
		f.Fatalf("New: %v", err)
	}

	f.Add(v4, true, v5, true)
	f.Add(v5, true, v4, true)                              // v5 first: useSameStreamMux with no prior config (ErrNoConfig).
	f.Add(buildInBandTwoElementsSameMux(), true, v4, true) // a two-element in-band payload seed.

	f.Fuzz(func(t *testing.T, p1 []byte, m1 bool, p2 []byte, m2 bool) {
		d.Reset()
		if aus, err := d.Depacketize(p1, m1, 0); err == nil {
			assertAUInvariants(t, aus)
		}
		if aus, err := d.Depacketize(p2, m2, 0); err == nil {
			assertAUInvariants(t, aus)
		}
	})
}
