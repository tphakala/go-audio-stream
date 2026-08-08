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

// FuzzDepacketizeOutOfBand drives the out-of-band Depacketize path with a
// fixed valid StreamMuxConfig (v1) and arbitrary AudioMuxElement bytes and
// marker bit. It must never panic.
func FuzzDepacketizeOutOfBand(f *testing.F) {
	d, err := New(Config{MuxConfigPresent: false, StreamMuxConfig: v1})
	if err != nil {
		f.Fatalf("New: %v", err)
	}

	// v2 is the out-of-band AudioMuxElement paired with v1 (see V2 in
	// latm_test.go): MuxSlotLengthBytes 03, payload AA BB CC.
	v2 := []byte{0x03, 0xAA, 0xBB, 0xCC}
	for _, buf := range truncs(v2) {
		f.Add(buf, true)
	}
	f.Fuzz(func(t *testing.T, payload []byte, marker bool) {
		_, _ = d.Depacketize(payload, marker, 0)
	})
}

// FuzzDepacketizeInBand drives the in-band Depacketize path across a
// two-call sequence with arbitrary payloads and marker bits, exercising the
// retained-config path and useSameStreamMux. It must never panic on either
// call, including the second call against whatever state the first call left
// behind. d.Reset() at the top of each fuzz execution keeps iterations
// independent of one another, mirroring depacket/aac's FuzzDepacketize.
func FuzzDepacketizeInBand(f *testing.F) {
	d, err := New(Config{MuxConfigPresent: true})
	if err != nil {
		f.Fatalf("New: %v", err)
	}

	f.Add(v4, true, v5, true)
	f.Add(v5, true, v4, true) // v5 first: useSameStreamMux with no prior config (ErrNoConfig).

	f.Fuzz(func(t *testing.T, p1 []byte, m1 bool, p2 []byte, m2 bool) {
		d.Reset()
		_, _ = d.Depacketize(p1, m1, 0)
		_, _ = d.Depacketize(p2, m2, 0)
	})
}
