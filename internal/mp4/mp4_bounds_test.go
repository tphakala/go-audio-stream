package mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// This file pins the safety-critical validation branches of the box reader that
// the deterministic-fixture tests in mp4_test.go do not yet exercise. Every input
// here is a hand-built malformed or edge-case segment: the assertions match exact
// error sentinels (errors.Is) or exact parsed values, so a test fails if the
// corresponding guard is weakened or removed, not merely if the parser panics.

// u64 encodes a big-endian 64-bit value, the companion of u16/u32 from mp4_test.go.
func u64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

// trunBox builds a trun box payload with an explicit flags word, sample_count and
// (when the data-offset flag is set) data_offset, followed by raw per-sample record
// bytes. It lets a test drive parseTrun's flag-dependent layout directly.
func trunBox(flags, sampleCount uint32, dataOffset int32, records []byte) []byte {
	body := bytes.Clone(u32(sampleCount))
	if flags&trunDataOffset != 0 {
		body = append(body, u32(uint32(dataOffset))...)
	}
	body = append(body, records...)
	return fullBox("trun", 0, flags, body)
}

// tfhdMoofBase builds a tfhd whose only flag is default-base-is-moof, so the base
// defaults to the moof start and no explicit base_data_offset is present.
func tfhdMoofBase(trackID uint32) []byte {
	return fullBox("tfhd", 0, tfhdDefaultBaseIsMoof, u32(trackID))
}

// tfhdExplicitBase builds a tfhd that carries an explicit 64-bit base_data_offset.
func tfhdExplicitBase(trackID uint32, base uint64) []byte {
	return fullBox("tfhd", 0, tfhdBaseDataOffset, u32(trackID), u64(base))
}

// TestParseFragmentSampleBudget pins the per-fragment delivered-sample cap. A
// traf can pack many defaults-only truns that each re-seat data_offset to the
// same mdat bytes; without a whole-fragment budget that re-delivers those bytes
// a quadratic number of times. Since every delivered sample consumes at least
// one fragment byte, ParseFragment must stop with ErrMalformedBox once the
// samples delivered exceed the fragment's byte count.
func TestParseFragmentSampleBudget(t *testing.T) {
	const (
		nTruns         = 40
		samplesPerTrun = 100
		mdatLen        = 100
	)
	// default-base-is-moof (base is the moof start) plus default_sample_size 1,
	// so each sample is one byte and no per-sample records are present.
	tfhd := fullBox("tfhd", 0, tfhdDefaultBaseIsMoof|tfhdDefaultSize, u32(1), u32(1))
	truns := func(off int32) []byte {
		out := make([]byte, 0, nTruns*24)
		for range nTruns {
			out = append(out, trunBox(trunDataOffset, samplesPerTrun, off, nil)...)
		}
		return out
	}
	// Two-pass: the data_offset must point at the mdat payload, whose position
	// depends on the moof length, which is independent of the offset value.
	moof0 := box("moof", box("traf", tfhd, truns(0)))
	dataOff := int32(len(moof0) + 8) // mdat payload follows the moof box and the mdat header
	frag := append(box("moof", box("traf", tfhd, truns(dataOff))), box("mdat", make([]byte, mdatLen))...)

	delivered := 0
	err := ParseFragment(AudioInit{TrackID: 1}, frag, func(Sample) error {
		delivered++
		return nil
	})
	if !errors.Is(err, ErrMalformedBox) {
		t.Fatalf("ParseFragment = %v, want ErrMalformedBox from the per-fragment sample budget", err)
	}
	if delivered > len(frag) {
		t.Fatalf("delivered %d samples, exceeds the fragment's %d-byte budget", delivered, len(frag))
	}
}

// assembleAudioFrag builds a fragment with a single audio traf from prebuilt tfhd
// and trun boxes plus an mdat payload. The moof is placed first, so moofStart is 0.
func assembleAudioFrag(tfhd, trun, mdat []byte) []byte {
	mfhd := fullBox("mfhd", 0, 0, u32(1))
	moof := box("moof", mfhd, box("traf", tfhd, trun))
	return append(moof, box("mdat", mdat)...)
}

// TestEachBoxSizeZeroExtendsToEnd pins the size==0 branch of eachBox: a 32-bit box
// size of 0 means the box runs to the end of the enclosing buffer, so exactly one
// box is reported whose payload ends at the buffer end and iteration terminates.
func TestEachBoxSizeZeroExtendsToEnd(t *testing.T) {
	payload := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	data := append(append(u32(0), []byte("free")...), payload...)
	var got []boxSpan
	if err := eachBox(data, func(b boxSpan) error {
		got = append(got, b)
		return nil
	}); err != nil {
		t.Fatalf("eachBox: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d boxes, want 1", len(got))
	}
	if got[0].typ != "free" {
		t.Errorf("typ = %q, want %q", got[0].typ, "free")
	}
	if got[0].bodyOff != 8 {
		t.Errorf("bodyOff = %d, want 8", got[0].bodyOff)
	}
	if got[0].bodyEnd != len(data) {
		t.Errorf("bodyEnd = %d, want %d (buffer end)", got[0].bodyEnd, len(data))
	}
}

// TestEachBoxLargesizeValid pins the size==1 (64-bit largesize) happy path: a valid
// largesize box parses, its 16-byte header is skipped, and its payload spans to the
// declared end.
func TestEachBoxLargesizeValid(t *testing.T) {
	payload := []byte{0x01, 0x02, 0x03, 0x04, 0x05}
	largesize := uint64(16 + len(payload))
	data := append(append(append(u32(1), []byte("mdat")...), u64(largesize)...), payload...)
	var got []boxSpan
	if err := eachBox(data, func(b boxSpan) error {
		got = append(got, b)
		return nil
	}); err != nil {
		t.Fatalf("eachBox: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d boxes, want 1", len(got))
	}
	if got[0].typ != "mdat" {
		t.Errorf("typ = %q, want %q", got[0].typ, "mdat")
	}
	if got[0].bodyOff != 16 {
		t.Errorf("bodyOff = %d, want 16 (past the 64-bit header)", got[0].bodyOff)
	}
	if got[0].bodyEnd != len(data) {
		t.Errorf("bodyEnd = %d, want %d", got[0].bodyEnd, len(data))
	}
}

// TestEachBoxMalformedSizes pins every malformed size-field branch of eachBox: an
// ordinary size below the 8-byte minimum or past the buffer end, a 64-bit largesize
// below its 16-byte minimum or past the buffer end, and a largesize header that is
// itself truncated. Each must return ErrMalformedBox.
func TestEachBoxMalformedSizes(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"ordinary size below minimum", append(u32(4), []byte("free")...)},
		{"ordinary size past buffer", append(u32(100), []byte("free")...)},
		{"largesize below minimum", append(append(u32(1), []byte("free")...), u64(15)...)},
		{"largesize past buffer", append(append(u32(1), []byte("free")...), u64(1000)...)},
		{"largesize header truncated", append(append(u32(1), []byte("free")...), u32(0)...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := eachBox(tc.data, func(boxSpan) error { return nil })
			if !errors.Is(err, ErrMalformedBox) {
				t.Errorf("eachBox = %v, want ErrMalformedBox", err)
			}
		})
	}
}

// TestParseFragmentSampleCountGuard pins the trun sample_count security guard, in
// both of its forms, exercised through ParseFragment. A trun that claims 2^31
// samples must be rejected with ErrMalformedBox before any iteration, whether it
// carries per-sample records (bounded by remaining/perSample) or relies purely on
// defaults (bounded by the fragment length). A missing guard would drive an
// out-of-range read or an unbounded loop.
func TestParseFragmentSampleCountGuard(t *testing.T) {
	const trackID = 5
	huge := uint32(1 << 31)

	t.Run("per-sample records", func(t *testing.T) {
		// data-offset + per-sample duration + size flags, but no record bytes, so
		// the sampleCount > remaining/perSample branch fires.
		trun := trunBox(trunDataOffset|trunSampleDuration|trunSampleSize, huge, 0, nil)
		frag := assembleAudioFrag(tfhdMoofBase(trackID), trun, []byte{1, 2, 3, 4})
		err := ParseFragment(AudioInit{TrackID: trackID}, frag, func(Sample) error { return nil })
		if !errors.Is(err, ErrMalformedBox) {
			t.Errorf("ParseFragment = %v, want ErrMalformedBox", err)
		}
	})

	t.Run("defaults only", func(t *testing.T) {
		// data-offset flag only (so the data position is located) and no per-sample
		// flags, so the sampleCount > len(frag) branch fires.
		trun := trunBox(trunDataOffset, huge, 0, nil)
		frag := assembleAudioFrag(tfhdMoofBase(trackID), trun, []byte{1, 2, 3, 4})
		err := ParseFragment(AudioInit{TrackID: trackID, DefaultDur: 1024, DefaultSize: 16}, frag, func(Sample) error { return nil })
		if !errors.Is(err, ErrMalformedBox) {
			t.Errorf("ParseFragment = %v, want ErrMalformedBox", err)
		}
	})
}

// TestParseFragmentImplicitOffsetGuard pins the implicit-offset guard: a trun that
// omits the data_offset flag while no base was ever established (default-base-is-moof
// with no explicit base_data_offset, no preceding trun offset) cannot locate its
// sample data, since the default base is the moof start rather than the media. It
// must return ErrMalformedBox instead of slicing the moof header as AAC.
func TestParseFragmentImplicitOffsetGuard(t *testing.T) {
	const trackID = 6
	records := append(u32(1024), u32(16)...) // one sample: duration, size
	trun := trunBox(trunSampleDuration|trunSampleSize, 1, 0, records)
	frag := assembleAudioFrag(tfhdMoofBase(trackID), trun, bytes.Repeat([]byte{0xAB}, 16))
	err := ParseFragment(AudioInit{TrackID: trackID}, frag, func(Sample) error { return nil })
	if !errors.Is(err, ErrMalformedBox) {
		t.Errorf("ParseFragment = %v, want ErrMalformedBox", err)
	}
}

// TestParseFragmentImplicitOffsetWithExplicitBase is the precision counterpart to
// the implicit-offset guard: a trun that omits the data_offset flag is still
// accepted when the tfhd supplied an explicit base_data_offset, and it must slice
// its samples starting at that base. This proves the guard rejects only the
// unlocatable case, not every offset-less trun.
func TestParseFragmentImplicitOffsetWithExplicitBase(t *testing.T) {
	const trackID = 6
	sampleBytes := bytes.Repeat([]byte{0xAB}, 16)
	records := append(u32(1024), u32(uint32(len(sampleBytes)))...) // duration, size
	trun := trunBox(trunSampleDuration|trunSampleSize, 1, 0, records)

	mfhd := fullBox("mfhd", 0, 0, u32(1))
	buildMoof := func(base uint64) []byte {
		return box("moof", mfhd, box("traf", tfhdExplicitBase(trackID, base), trun))
	}
	// The tfhd length is constant regardless of the base value, so measure the moof
	// once, then set the base to the start of the mdat payload and rebuild.
	moof := buildMoof(0)
	base := uint64(len(moof) + 8) // +8 skips the mdat box header
	moof = buildMoof(base)
	frag := append(bytes.Clone(moof), box("mdat", sampleBytes)...)

	var got [][]byte
	var durs []uint32
	if err := ParseFragment(AudioInit{TrackID: trackID}, frag, func(s Sample) error {
		got = append(got, bytes.Clone(s.Data))
		durs = append(durs, s.Dur)
		return nil
	}); err != nil {
		t.Fatalf("ParseFragment: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("delivered %d samples, want 1", len(got))
	}
	if !bytes.Equal(got[0], sampleBytes) {
		t.Errorf("sample = %x, want %x", got[0], sampleBytes)
	}
	if durs[0] != 1024 {
		t.Errorf("dur = %d, want 1024", durs[0])
	}
}

// TestParseFragmentOnSampleError pins error propagation: an error returned by the
// onSample callback must surface verbatim from ParseFragment (errors.Is), not be
// swallowed or converted.
func TestParseFragmentOnSampleError(t *testing.T) {
	sentinel := errors.New("caller aborted")
	samples := [][]byte{bytes.Repeat([]byte{0x1}, 10), bytes.Repeat([]byte{0x2}, 10)}
	frag := buildFrag(fragOpts{trackID: 4, samples: samples, dur: 1024, perSample: true})
	err := ParseFragment(AudioInit{TrackID: 4, Timescale: 44100}, frag, func(Sample) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("ParseFragment = %v, want the callback sentinel", err)
	}
}

// tfhdPayload builds a tfhd box payload (starting at the FullBox version/flags, as
// parseTFHD receives it) with the given flags, a fixed track_ID, and raw optional
// field bytes appended after track_ID.
func tfhdPayload(flags uint32, optional ...[]byte) []byte {
	b := []byte{0, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	b = append(b, u32(1)...) // track_ID
	for _, o := range optional {
		b = append(b, o...)
	}
	return b
}

// TestParseTFHDOptionalFields pins parseTFHD's flag-driven optional-field parsing:
// a present base_data_offset is honored, one above MaxInt32 is rejected, the
// default duration/size are read (past a present sample_description_index), and a
// field whose flag is set but whose bytes are missing returns ErrMalformedBox.
func TestParseTFHDOptionalFields(t *testing.T) {
	t.Run("explicit base honored", func(t *testing.T) {
		body := tfhdPayload(tfhdBaseDataOffset, u64(1000))
		base, _, _, explicit, err := parseTFHD(body, 42)
		if err != nil {
			t.Fatalf("parseTFHD: %v", err)
		}
		if base != 1000 {
			t.Errorf("base = %d, want 1000", base)
		}
		if !explicit {
			t.Error("baseExplicit = false, want true")
		}
	})

	t.Run("no base defaults to moof start", func(t *testing.T) {
		body := tfhdPayload(tfhdDefaultBaseIsMoof)
		base, _, _, explicit, err := parseTFHD(body, 77)
		if err != nil {
			t.Fatalf("parseTFHD: %v", err)
		}
		if base != 77 {
			t.Errorf("base = %d, want 77 (moof start)", base)
		}
		if explicit {
			t.Error("baseExplicit = true, want false")
		}
	})

	t.Run("base above MaxInt32 rejected", func(t *testing.T) {
		body := tfhdPayload(tfhdBaseDataOffset, u64(1<<31)) // MaxInt32 + 1
		if _, _, _, _, err := parseTFHD(body, 0); !errors.Is(err, ErrMalformedBox) {
			t.Errorf("parseTFHD = %v, want ErrMalformedBox", err)
		}
	})

	t.Run("defaults read past sample_description_index", func(t *testing.T) {
		flags := uint32(tfhdSampleDescIndex | tfhdDefaultDuration | tfhdDefaultSize)
		body := tfhdPayload(flags, u32(1), u32(1024), u32(300))
		_, defDur, defSize, _, err := parseTFHD(body, 0)
		if err != nil {
			t.Fatalf("parseTFHD: %v", err)
		}
		if defDur != 1024 || defSize != 300 {
			t.Errorf("defaults = (%d,%d), want (1024,300)", defDur, defSize)
		}
	})

	truncations := []struct {
		name  string
		flags uint32
	}{
		{"base_data_offset", tfhdBaseDataOffset},
		{"sample_description_index", tfhdSampleDescIndex},
		{"default_sample_duration", tfhdDefaultDuration},
		{"default_sample_size", tfhdDefaultSize},
	}
	for _, tc := range truncations {
		t.Run("truncated "+tc.name, func(t *testing.T) {
			// Flag set, but no optional bytes follow track_ID.
			body := tfhdPayload(tc.flags)
			if _, _, _, _, err := parseTFHD(body, 0); !errors.Is(err, ErrMalformedBox) {
				t.Errorf("parseTFHD = %v, want ErrMalformedBox", err)
			}
		})
	}
}

// TestParseInitTimescaleValidation pins the media-timescale validation in ParseInit:
// a zero timescale (every PTS would be zero) and a timescale above MaxInt32 (which
// would narrow negative on a 32-bit build) are both rejected with ErrMalformedBox,
// while a normal timescale parses.
func TestParseInitTimescaleValidation(t *testing.T) {
	t.Run("zero timescale rejected", func(t *testing.T) {
		init := buildInit(&initOpts{asc: wantASC, timescale: 0, trackID: 1})
		if _, err := ParseInit(init); !errors.Is(err, ErrMalformedBox) {
			t.Errorf("ParseInit = %v, want ErrMalformedBox", err)
		}
	})

	t.Run("timescale above MaxInt32 rejected", func(t *testing.T) {
		init := buildInit(&initOpts{asc: wantASC, timescale: 0xFFFFFFFF, trackID: 1})
		if _, err := ParseInit(init); !errors.Is(err, ErrMalformedBox) {
			t.Errorf("ParseInit = %v, want ErrMalformedBox", err)
		}
	})

	t.Run("normal timescale parses", func(t *testing.T) {
		init := buildInit(&initOpts{asc: wantASC, timescale: 44100, trackID: 1})
		ai, err := ParseInit(init)
		if err != nil {
			t.Fatalf("ParseInit: %v", err)
		}
		if ai.Timescale != 44100 {
			t.Errorf("Timescale = %d, want 44100", ai.Timescale)
		}
	})
}

// esdsWithESFlags builds an esds box whose ES_Descriptor carries the given flags
// byte and raw optional-field bytes (streamDependence, URL, OCR) before the
// DecoderConfigDescriptor, so a test can confirm the ASC is still reached past them.
func esdsWithESFlags(asc []byte, esFlags byte, optional []byte) []byte {
	dsi := descriptor(tagDecoderSpecific, asc)
	dcd := descriptor(tagDecoderConfig, append([]byte{objectTypeAAC, 0x15, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, dsi...))
	esContent := append([]byte{0x00, 0x00, esFlags}, optional...)
	esContent = append(esContent, dcd...)
	es := descriptor(tagESDescriptor, esContent)
	return box("esds", append([]byte{0, 0, 0, 0}, es...))
}

// TestParseESDSOptionalDescriptorFlags pins parseESDS's ES_Descriptor optional-field
// skipping: with the streamDependence (0x80), URL (0x40) or OCRstream (0x20) flags
// set, the correct number of bytes must be skipped so the DecoderConfigDescriptor
// and its AudioSpecificConfig are still found. A wrong skip size would miss the ASC.
func TestParseESDSOptionalDescriptorFlags(t *testing.T) {
	cases := []struct {
		name     string
		flags    byte
		optional []byte
	}{
		{"streamDependence", 0x80, []byte{0x00, 0x11}},                      // dependsOn_ES_ID (2)
		{"URL", 0x40, []byte{0x03, 'a', 'b', 'c'}},                          // URLlength (1) + 3 bytes
		{"OCRstream", 0x20, []byte{0x00, 0x22}},                             // OCR_ES_Id (2)
		{"all three", 0xE0, []byte{0x00, 0x11, 0x02, 'x', 'y', 0x00, 0x22}}, // streamDep(2) + URL(1+2) + OCR(2)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			esds := esdsWithESFlags(wantASC, tc.flags, tc.optional)
			asc, err := parseESDS(esds[8:]) // strip the box header; parseESDS takes the payload
			if err != nil {
				t.Fatalf("parseESDS: %v", err)
			}
			if !bytes.Equal(asc, wantASC) {
				t.Errorf("ASC = %x, want %x", asc, wantASC)
			}
		})
	}
}

// buildInitV1 builds an initialization segment using version-1 (64-bit time) tkhd
// and mdhd boxes, mirroring buildInit's version-0 audio path otherwise.
func buildInitV1(asc []byte, timescale, trackID uint32) []byte {
	tkhd := fullBox("tkhd", 1, 0x7, u64(0), u64(0), u32(trackID), u32(0), u64(0))
	mdhd := fullBox("mdhd", 1, 0, u64(0), u64(0), u32(timescale), u32(0), u16(0x55c4), u16(0))
	hdlr := fullBox("hdlr", 0, 0, u32(0), []byte("soun"), u32(0), u32(0), u32(0), []byte("x\x00"))
	stsd := fullBox("stsd", 0, 0, u32(1), mp4aBox(asc, 0x40))
	mdia := box("mdia", mdhd, hdlr, box("minf", box("stbl", stsd)))
	trak := box("trak", tkhd, mdia)
	moov := box("moov", trak)
	ftyp := box("ftyp", []byte("iso5"), u32(0), []byte("iso5"))
	return append(ftyp, moov...)
}

// TestParseInitVersion1Times pins the version-1 tkhd/mdhd layouts: with 64-bit
// creation/modification times the track_ID and timescale sit at different offsets,
// and ParseInit must still resolve both correctly.
func TestParseInitVersion1Times(t *testing.T) {
	init := buildInitV1(wantASC, 48000, 9)
	ai, err := ParseInit(init)
	if err != nil {
		t.Fatalf("ParseInit: %v", err)
	}
	if ai.TrackID != 9 {
		t.Errorf("TrackID = %d, want 9", ai.TrackID)
	}
	if ai.Timescale != 48000 {
		t.Errorf("Timescale = %d, want 48000", ai.Timescale)
	}
	if !bytes.Equal(ai.ASC, wantASC) {
		t.Errorf("ASC = %x, want %x", ai.ASC, wantASC)
	}
}
