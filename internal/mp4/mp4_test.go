package mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// wantASC is the AAC-LC / 44100 Hz / stereo AudioSpecificConfig, matching the
// hlssource TS fixtures so an fMP4 AAC track resolves the byte-identical config.
var wantASC = []byte{0x12, 0x10}

func u16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func box(typ string, parts ...[]byte) []byte {
	var body []byte
	for _, p := range parts {
		body = append(body, p...)
	}
	out := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(out, uint32(8+len(body)))
	copy(out[4:8], typ)
	return append(out, body...)
}

func fullBox(typ string, version byte, flags uint32, parts ...[]byte) []byte {
	head := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	all := make([][]byte, 0, 1+len(parts))
	all = append(all, head)
	all = append(all, parts...)
	return box(typ, all...)
}

func descriptor(tag byte, content []byte) []byte {
	return append([]byte{tag, byte(len(content))}, content...)
}

func esdsBox(asc []byte, objectType byte) []byte {
	dsi := descriptor(0x05, asc)
	dcd := descriptor(0x04, append([]byte{objectType, 0x15, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, dsi...))
	es := descriptor(0x03, append([]byte{0x00, 0x00, 0x00}, dcd...))
	return box("esds", append([]byte{0, 0, 0, 0}, es...))
}

func mp4aBox(asc []byte, objectType byte) []byte {
	ase := make([]byte, 28)
	ase[7] = 1
	binary.BigEndian.PutUint16(ase[16:], 2)
	binary.BigEndian.PutUint16(ase[18:], 16)
	binary.BigEndian.PutUint32(ase[24:], 44100<<16)
	return box("mp4a", ase, esdsBox(asc, objectType))
}

// initOpts configures a hand-built initialization segment for the tests.
type initOpts struct {
	asc         []byte
	timescale   uint32
	trackID     uint32
	handler     string // default "soun"
	objectType  byte   // default 0x40 (AAC)
	sampleEntry []byte // override the whole stsd sample entry (e.g. an enca box)
	noASC       bool   // build an mp4a without an esds
	defDur      uint32
	defSize     uint32
	noAudio     bool // build only a video track
}

func buildInit(o *initOpts) []byte {
	if o.handler == "" {
		o.handler = "soun"
	}
	if o.objectType == 0 {
		o.objectType = 0x40
	}
	tkhd := fullBox("tkhd", 0, 0x7, u32(0), u32(0), u32(o.trackID), u32(0), u32(0))
	mdhd := fullBox("mdhd", 0, 0, u32(0), u32(0), u32(o.timescale), u32(0), u16(0x55c4), u16(0))
	hdlr := fullBox("hdlr", 0, 0, u32(0), []byte(o.handler), u32(0), u32(0), u32(0), []byte("x\x00"))

	var entry []byte
	switch {
	case o.sampleEntry != nil:
		entry = o.sampleEntry
	case o.noASC:
		ase := make([]byte, 28)
		entry = box("mp4a", ase) // mp4a without an esds child
	default:
		entry = mp4aBox(o.asc, o.objectType)
	}
	stsd := fullBox("stsd", 0, 0, u32(1), entry)
	mdia := box("mdia", mdhd, hdlr, box("minf", box("stbl", stsd)))
	trak := box("trak", tkhd, mdia)

	moovParts := [][]byte{}
	if !o.noAudio {
		moovParts = append(moovParts, trak)
	} else {
		// A video-only trak: handler 'vide', an avc1 sample entry.
		vhdlr := fullBox("hdlr", 0, 0, u32(0), []byte("vide"), u32(0), u32(0), u32(0), []byte("v\x00"))
		vmdhd := fullBox("mdhd", 0, 0, u32(0), u32(0), u32(o.timescale), u32(0), u16(0x55c4), u16(0))
		vtkhd := fullBox("tkhd", 0, 0x7, u32(0), u32(0), u32(o.trackID), u32(0), u32(0))
		vstsd := fullBox("stsd", 0, 0, u32(1), box("avc1", make([]byte, 78)))
		vtrak := box("trak", vtkhd, box("mdia", vmdhd, vhdlr, box("minf", box("stbl", vstsd))))
		moovParts = append(moovParts, vtrak)
	}
	trex := fullBox("trex", 0, 0, u32(o.trackID), u32(1), u32(o.defDur), u32(o.defSize), u32(0))
	moovParts = append(moovParts, box("mvex", trex))
	moov := box("moov", moovParts...)
	ftyp := box("ftyp", []byte("iso5"), u32(0), []byte("iso5"))
	return append(ftyp, moov...)
}

// fragOpts configures a hand-built media fragment.
type fragOpts struct {
	trackID   uint32
	samples   [][]byte
	dur       uint32
	perSample bool // when false, omit per-sample size/dur and rely on tfhd/trex defaults
}

func buildFrag(o fragOpts) []byte {
	mfhd := fullBox("mfhd", 0, 0, u32(1))
	tfhd := fullBox("tfhd", 0, 0x020000, u32(o.trackID)) // default-base-is-moof
	var data []byte
	for _, s := range o.samples {
		data = append(data, s...)
	}
	trunFlags := uint32(0x000001) // data-offset
	if o.perSample {
		trunFlags |= 0x000100 | 0x000200 // sample duration + size
	}
	trun := func(off int32) []byte {
		body := bytes.Clone(u32(uint32(len(o.samples))))
		body = append(body, u32(uint32(off))...)
		if o.perSample {
			for _, s := range o.samples {
				body = append(body, u32(o.dur)...)
				body = append(body, u32(uint32(len(s)))...)
			}
		}
		return fullBox("trun", 0, trunFlags, body)
	}
	moof := box("moof", mfhd, box("traf", tfhd, trun(0)))
	off := int32(len(moof) + 8)
	moof = box("moof", mfhd, box("traf", tfhd, trun(off)))
	return append(moof, box("mdat", data)...)
}

func TestParseInitResolvesTrackAndASC(t *testing.T) {
	init := buildInit(&initOpts{asc: wantASC, timescale: 44100, trackID: 7})
	ai, err := ParseInit(init)
	if err != nil {
		t.Fatalf("ParseInit: %v", err)
	}
	if !bytes.Equal(ai.ASC, wantASC) {
		t.Errorf("ASC = %x, want %x", ai.ASC, wantASC)
	}
	if ai.Timescale != 44100 {
		t.Errorf("Timescale = %d, want 44100", ai.Timescale)
	}
	if ai.TrackID != 7 {
		t.Errorf("TrackID = %d, want 7", ai.TrackID)
	}
}

func TestParseInitTrexDefaults(t *testing.T) {
	init := buildInit(&initOpts{asc: wantASC, timescale: 48000, trackID: 1, defDur: 1024, defSize: 300})
	ai, err := ParseInit(init)
	if err != nil {
		t.Fatalf("ParseInit: %v", err)
	}
	if ai.DefaultDur != 1024 || ai.DefaultSize != 300 {
		t.Errorf("trex defaults = (%d,%d), want (1024,300)", ai.DefaultDur, ai.DefaultSize)
	}
}

func TestParseInitErrorTaxonomy(t *testing.T) {
	cases := []struct {
		name string
		init []byte
		want error
	}{
		{"no audio track", buildInit(&initOpts{timescale: 44100, trackID: 2, noAudio: true}), ErrNoAudioTrack},
		{"no ASC", buildInit(&initOpts{timescale: 44100, trackID: 1, noASC: true}), ErrNoASC},
		{
			"encrypted sample entry",
			buildInit(&initOpts{timescale: 44100, trackID: 1, sampleEntry: box("enca", make([]byte, 28))}),
			ErrUnsupportedSampleEntry,
		},
		{
			"non-AAC objectType",
			buildInit(&initOpts{asc: wantASC, timescale: 44100, trackID: 1, objectType: 0x69}),
			ErrUnsupportedSampleEntry,
		},
		{"no moov", box("ftyp", []byte("iso5")), ErrMalformedBox},
		{"empty", []byte{}, ErrMalformedBox},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseInit(tc.init); !errors.Is(err, tc.want) {
				t.Errorf("ParseInit = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestParseFragmentDeliversSamples(t *testing.T) {
	samples := [][]byte{
		bytes.Repeat([]byte{0xA1}, 30),
		bytes.Repeat([]byte{0xB2}, 40),
		bytes.Repeat([]byte{0xC3}, 25),
	}
	frag := buildFrag(fragOpts{trackID: 7, samples: samples, dur: 1024, perSample: true})
	init := AudioInit{TrackID: 7, Timescale: 44100}
	var got [][]byte
	var durs []uint32
	if err := ParseFragment(init, frag, func(s Sample) error {
		got = append(got, bytes.Clone(s.Data))
		durs = append(durs, s.Dur)
		return nil
	}); err != nil {
		t.Fatalf("ParseFragment: %v", err)
	}
	if len(got) != len(samples) {
		t.Fatalf("delivered %d samples, want %d", len(got), len(samples))
	}
	for i := range samples {
		if !bytes.Equal(got[i], samples[i]) {
			t.Errorf("sample %d mismatch", i)
		}
		if durs[i] != 1024 {
			t.Errorf("sample %d dur = %d, want 1024", i, durs[i])
		}
	}
}

func TestParseFragmentUsesTrexDefaults(t *testing.T) {
	// A fragment with no per-sample size/duration inherits them from the init
	// (trex) defaults threaded through AudioInit.
	sample := bytes.Repeat([]byte{0x5A}, 50)
	frag := buildFrag(fragOpts{trackID: 1, samples: [][]byte{sample}, perSample: false})
	init := AudioInit{TrackID: 1, Timescale: 48000, DefaultDur: 1024, DefaultSize: 50}
	var got []Sample
	if err := ParseFragment(init, frag, func(s Sample) error {
		got = append(got, Sample{Data: bytes.Clone(s.Data), Dur: s.Dur})
		return nil
	}); err != nil {
		t.Fatalf("ParseFragment: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("delivered %d samples, want 1", len(got))
	}
	if !bytes.Equal(got[0].Data, sample) {
		t.Error("default-size sample data mismatch")
	}
	if got[0].Dur != 1024 {
		t.Errorf("default dur = %d, want 1024", got[0].Dur)
	}
}

func TestParseFragmentMultiplexedPicksAudioTrack(t *testing.T) {
	// One moof with a video traf (track 1) and an audio traf (track 2), video
	// bytes placed first in the mdat. Reading mdat sequentially would return the
	// video bytes; selecting by track_ID and honoring data_offset returns audio.
	videoData := bytes.Repeat([]byte{0xFF}, 100)
	audio := [][]byte{bytes.Repeat([]byte{0x11}, 20), bytes.Repeat([]byte{0x22}, 30)}
	frag := buildMuxFrag(1, videoData, 2, audio, 1024)
	init := AudioInit{TrackID: 2, Timescale: 44100}
	var got [][]byte
	if err := ParseFragment(init, frag, func(s Sample) error {
		got = append(got, bytes.Clone(s.Data))
		return nil
	}); err != nil {
		t.Fatalf("ParseFragment: %v", err)
	}
	if len(got) != len(audio) {
		t.Fatalf("delivered %d samples, want %d", len(got), len(audio))
	}
	for i := range audio {
		if !bytes.Equal(got[i], audio[i]) {
			t.Errorf("multiplexed audio sample %d mismatch: got %x", i, got[i][:1])
		}
	}
}

func TestParseFragmentShortMdat(t *testing.T) {
	// A trun claiming a sample larger than the mdat holds must stop with
	// ErrShortMdat, never panic.
	frag := buildOversizeFrag(3)
	init := AudioInit{TrackID: 3, Timescale: 44100}
	err := ParseFragment(init, frag, func(Sample) error { return nil })
	if !errors.Is(err, ErrShortMdat) {
		t.Errorf("oversize sample = %v, want ErrShortMdat", err)
	}
}

func TestParseFragmentNoMatchingTraf(t *testing.T) {
	frag := buildFrag(fragOpts{trackID: 9, samples: [][]byte{{1, 2, 3}}, dur: 1024, perSample: true})
	init := AudioInit{TrackID: 7, Timescale: 44100}
	if err := ParseFragment(init, frag, func(Sample) error { return nil }); !errors.Is(err, ErrMalformedBox) {
		t.Errorf("track mismatch = %v, want ErrMalformedBox", err)
	}
}

// buildMuxFrag builds a moof with a video and an audio traf, video bytes first in
// the mdat, exercising track selection and absolute sample-offset computation.
func buildMuxFrag(videoID uint32, videoData []byte, audioID uint32, audioSamples [][]byte, dur uint32) []byte {
	mfhd := fullBox("mfhd", 0, 0, u32(1))
	vtfhd := fullBox("tfhd", 0, 0x020000, u32(videoID))
	vtrun := func(off int32) []byte {
		body := bytes.Clone(u32(1))
		body = append(body, u32(uint32(off))...)
		body = append(body, u32(dur)...)
		body = append(body, u32(uint32(len(videoData)))...)
		return fullBox("trun", 0, 0x000301, body)
	}
	atfhd := fullBox("tfhd", 0, 0x020000, u32(audioID))
	aRecords := make([]byte, 0, len(audioSamples)*8)
	aData := make([]byte, 0, len(audioSamples)*64)
	for _, s := range audioSamples {
		aRecords = append(aRecords, u32(dur)...)
		aRecords = append(aRecords, u32(uint32(len(s)))...)
		aData = append(aData, s...)
	}
	atrun := func(off int32) []byte {
		body := bytes.Clone(u32(uint32(len(audioSamples))))
		body = append(body, u32(uint32(off))...)
		body = append(body, aRecords...)
		return fullBox("trun", 0, 0x000301, body)
	}
	build := func(vOff, aOff int32) []byte {
		return box("moof", mfhd, box("traf", vtfhd, vtrun(vOff)), box("traf", atfhd, atrun(aOff)))
	}
	moof := build(0, 0)
	vOff := int32(len(moof) + 8)
	aOff := vOff + int32(len(videoData))
	moof = build(vOff, aOff)
	return append(moof, box("mdat", append(bytes.Clone(videoData), aData...))...)
}

// buildOversizeFrag builds a fragment whose single trun sample declares a size
// far larger than the mdat carries.
func buildOversizeFrag(trackID uint32) []byte {
	mfhd := fullBox("mfhd", 0, 0, u32(1))
	tfhd := fullBox("tfhd", 0, 0x020000, u32(trackID))
	trun := func(off int32) []byte {
		body := bytes.Clone(u32(1))              // sample_count
		body = append(body, u32(uint32(off))...) // data_offset
		body = append(body, u32(1024)...)        // duration
		body = append(body, u32(1_000_000)...)   // size, far past the mdat
		return fullBox("trun", 0, 0x000301, body)
	}
	moof := box("moof", mfhd, box("traf", tfhd, trun(0)))
	off := int32(len(moof) + 8)
	moof = box("moof", mfhd, box("traf", tfhd, trun(off)))
	return append(moof, box("mdat", []byte{1, 2, 3, 4})...)
}
