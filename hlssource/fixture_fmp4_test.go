package hlssource

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// This file hand-encodes fMP4 (CMAF) fixtures in the style of the MPEG-TS
// fixtures in fixture_test.go. buildInitSegment produces an ftyp+moov whose audio
// track carries the same wantASC (0x1210) AudioSpecificConfig the TS path
// resolves, so an fMP4 AAC track produces the byte-identical CodecAAC. buildFragment
// produces a moof+mdat carrying whole AAC access units.

// u16 and u32 encode a big-endian integer, the ISO BMFF byte order.
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

// box length-prefixes a run of payload parts with the 8-byte ISO BMFF box header
// (size then four-character type).
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

// fullBox is box with the leading FullBox version byte and 24-bit flags.
func fullBox(typ string, version byte, flags uint32, parts ...[]byte) []byte {
	head := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	all := make([][]byte, 0, 1+len(parts))
	all = append(all, head)
	all = append(all, parts...)
	return box(typ, all...)
}

// descriptor encodes one MPEG-4 descriptor with a single-byte expandable length
// (content is always well under 128 bytes in these fixtures).
func descriptor(tag byte, content []byte) []byte {
	return append([]byte{tag, byte(len(content))}, content...)
}

// buildESDS builds an esds box whose DecoderSpecificInfo is asc and whose
// DecoderConfigDescriptor objectTypeIndication is AAC (0x40).
func buildESDS(asc []byte) []byte {
	dsi := descriptor(0x05, asc) // DecoderSpecificInfo
	dcdContent := append([]byte{
		0x40,             // objectTypeIndication: AAC
		0x15,             // streamType (audio) + upstream + reserved
		0x00, 0x00, 0x00, // bufferSizeDB
		0x00, 0x00, 0x00, 0x00, // maxBitrate
		0x00, 0x00, 0x00, 0x00, // avgBitrate
	}, dsi...)
	dcd := descriptor(0x04, dcdContent) // DecoderConfigDescriptor
	esContent := append([]byte{
		0x00, 0x00, // ES_ID
		0x00, // flags: no dependency, no URL, no OCR
	}, dcd...)
	esDesc := descriptor(0x03, esContent) // ES_Descriptor
	return box("esds", append([]byte{0x00, 0x00, 0x00, 0x00}, esDesc...))
}

// buildMP4A builds an mp4a AudioSampleEntry carrying the esds for asc.
func buildMP4A(asc []byte, timescale uint32) []byte {
	ase := make([]byte, 28)
	ase[7] = 1                                          // data_reference_index
	binary.BigEndian.PutUint16(ase[16:], 2)             // channelcount
	binary.BigEndian.PutUint16(ase[18:], 16)            // samplesize
	binary.BigEndian.PutUint32(ase[24:], timescale<<16) // samplerate 16.16 (ignored)
	return box("mp4a", ase, buildESDS(asc))
}

// buildInitSegment builds a whole fMP4 initialization segment (ftyp + moov with
// one 'soun' trak carrying an mp4a/esds sample entry, plus mvex/trex) resolving to
// the given AudioSpecificConfig, timescale and track_ID. The trex defaults are
// zero, so fragments built with buildFragment carry their own per-sample values.
func buildInitSegment(asc []byte, timescale, trackID uint32) []byte {
	return buildInitSegmentDefaults(asc, timescale, trackID, 0, 0)
}

// buildInitSegmentDefaults is buildInitSegment with caller-chosen trex
// default_sample_duration and default_sample_size, so a test can prove a fragment
// that omits per-sample values inherits the trex defaults.
func buildInitSegmentDefaults(asc []byte, timescale, trackID, defDur, defSize uint32) []byte {
	tkhd := fullBox("tkhd", 0, 0x000007,
		u32(0), u32(0), u32(trackID), u32(0), u32(0)) // creation, modification, track_ID, reserved, duration
	mdhd := fullBox("mdhd", 0, 0,
		u32(0), u32(0), u32(timescale), u32(0), u16(0x55c4), u16(0)) // ..., timescale, duration, language, pre_defined
	hdlr := fullBox("hdlr", 0, 0,
		u32(0), []byte("soun"), u32(0), u32(0), u32(0), []byte("a\x00")) // pre_defined, handler_type, reserved, name
	stsd := fullBox("stsd", 0, 0, u32(1), buildMP4A(asc, timescale))
	stbl := box("stbl", stsd)
	minf := box("minf", stbl)
	mdia := box("mdia", mdhd, hdlr, minf)
	trak := box("trak", tkhd, mdia)
	trex := fullBox("trex", 0, 0,
		u32(trackID), u32(1), u32(defDur), u32(defSize), u32(0)) // track_ID, default_sample_description_index, dur, size, flags
	mvex := box("mvex", trex)
	moov := box("moov", trak, mvex)
	ftyp := box("ftyp", []byte("iso5"), u32(0), []byte("iso5"), []byte("cmfc"))
	return append(ftyp, moov...)
}

// buildFragment builds a media fragment (moof + mdat) delivering each of samples
// as one AAC access unit with the given per-sample duration. The trun carries
// per-sample sizes and durations and a data_offset resolving (with the
// default-base-is-moof tfhd flag) to the mdat payload.
func buildFragment(trackID uint32, samples [][]byte, dur uint32) []byte {
	tfhd := fullBox("tfhd", 0, 0x020000, u32(trackID)) // default-base-is-moof
	tfdt := fullBox("tfdt", 1, 0, u32(0), u32(0))      // baseMediaDecodeTime 0 (ignored)
	mfhd := fullBox("mfhd", 0, 0, u32(1))

	records := make([]byte, 0, len(samples)*8)
	data := make([]byte, 0, len(samples)*64)
	for _, s := range samples {
		records = append(records, u32(dur)...)
		records = append(records, u32(uint32(len(s)))...)
		data = append(data, s...)
	}
	// trun flags: data-offset (0x01) + sample-duration (0x100) + sample-size (0x200).
	trun := func(dataOffset int32) []byte {
		body := append([]byte{}, u32(uint32(len(samples)))...)
		body = append(body, u32(uint32(dataOffset))...)
		body = append(body, records...)
		return fullBox("trun", 0, 0x000301, body)
	}
	moof := box("moof", mfhd, box("traf", tfhd, tfdt, trun(0)))
	dataOffset := int32(len(moof) + 8) // moof size then the 8-byte mdat header
	moof = box("moof", mfhd, box("traf", tfhd, tfdt, trun(dataOffset)))
	return append(moof, box("mdat", data)...)
}

// buildMultiplexedFragment builds a fragment carrying both a video and an audio
// traf in one moof, with the video sample bytes placed FIRST in the mdat. A
// demuxer that read the mdat sequentially would feed video into the AAC path;
// the correct behavior selects the audio traf by track_ID and honors its
// data_offset, skipping the video bytes.
func buildMultiplexedFragment(videoID uint32, videoData []byte, audioID uint32, audioSamples [][]byte, dur uint32) []byte {
	mfhd := fullBox("mfhd", 0, 0, u32(1))
	vtfhd := fullBox("tfhd", 0, 0x020000, u32(videoID))
	vtrun := func(off int32) []byte {
		body := append([]byte{}, u32(1)...)
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
		body := append([]byte{}, u32(uint32(len(audioSamples)))...)
		body = append(body, u32(uint32(off))...)
		body = append(body, aRecords...)
		return fullBox("trun", 0, 0x000301, body)
	}
	build := func(vOff, aOff int32) []byte {
		vtraf := box("traf", vtfhd, vtrun(vOff))
		atraf := box("traf", atfhd, atrun(aOff))
		return box("moof", mfhd, vtraf, atraf)
	}
	moof := build(0, 0)
	vOff := int32(len(moof) + 8) // + mdat header
	aOff := vOff + int32(len(videoData))
	moof = build(vOff, aOff)
	mdat := box("mdat", append(append([]byte{}, videoData...), aData...))
	return append(moof, mdat...)
}

// fmp4Samples builds n distinct raw AAC access units of payloadLen bytes each,
// each filled with a per-sample marker, returning them in order.
func fmp4Samples(n, payloadLen int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		s := make([]byte, payloadLen)
		for j := range s {
			s[j] = byte(i + 1)
		}
		out[i] = s
	}
	return out
}

// buildFMP4MediaPlaylist emits a media playlist body with an EXT-X-MAP init URI
// followed by the given fMP4 fragment segments.
func buildFMP4MediaPlaylist(targetDur int, mediaSeq uint64, endList bool, initURI string, segs []segSpec) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", targetDur)
	fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", mediaSeq)
	fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"%s\"\n", initURI)
	for _, s := range segs {
		if s.discontinuity {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		if s.gap {
			b.WriteString("#EXT-X-GAP\n")
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n%s\n", s.duration, s.uri)
	}
	if endList {
		b.WriteString("#EXT-X-ENDLIST\n")
	}
	return b.String()
}
