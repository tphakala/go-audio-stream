// Package mp4 is a dependency-free, bounds-safe ISO Base Media File Format (ISO
// BMFF / ISO/IEC 14496-12) box reader, scoped to what an fMP4 (CMAF) AAC audio
// track needs. It parses an initialization segment (ftyp/moov) into the audio
// track's AudioSpecificConfig, timescale and track_ID, and slices the AAC access
// units out of a media fragment (moof/mdat). It frames, it never decodes.
//
// Every input is treated as untrusted network data: each box size is validated
// against the remaining buffer, the MPEG-4 descriptor length reader is capped,
// the trun sample_count is validated before iterating, offset arithmetic is
// checked, and box descent is bounded, so a malformed or hostile segment yields
// an error rather than a panic, an unbounded loop, or an out-of-range read.
package mp4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Error sentinels the caller matches with errors.Is. ErrNoAudioTrack and ErrNoASC
// describe a structurally valid segment that carries no usable AAC audio (mapped
// by the caller to a malformed-segment cause); ErrUnsupportedSampleEntry
// describes an audio track this package will not play (encrypted, or a non-AAC
// codec); ErrMalformedBox describes a truncated or invalid box structure;
// ErrShortMdat reports that a fragment's declared sample data ran past the buffer,
// a recoverable, counted condition rather than a hard failure.
var (
	// ErrNoAudioTrack reports an initialization segment with no track whose
	// handler is 'soun' with an mp4a sample entry.
	ErrNoAudioTrack = errors.New("mp4: no AAC audio track in initialization segment")
	// ErrNoASC reports an mp4a sample entry from which no AudioSpecificConfig
	// (the esds DecoderSpecificInfo) could be extracted.
	ErrNoASC = errors.New("mp4: no AudioSpecificConfig in sample entry")
	// ErrUnsupportedSampleEntry reports an audio sample entry this package does
	// not support: an encrypted entry (enca and friends) or a non-AAC codec (a
	// non-mp4a entry, or an mp4a whose objectTypeIndication is not AAC).
	ErrUnsupportedSampleEntry = errors.New("mp4: unsupported or encrypted audio sample entry")
	// ErrMalformedBox reports a box whose size is invalid or which is truncated,
	// so the structure could not be parsed.
	ErrMalformedBox = errors.New("mp4: malformed box structure")
	// ErrShortMdat reports that a fragment's declared sample data overruns the
	// buffer. Samples decoded before the overrun are still delivered; the caller
	// treats this as a counted gap, not a fatal error.
	ErrShortMdat = errors.New("mp4: sample data overruns the fragment")
)

// AudioInit is the audio track state resolved from an initialization segment: the
// AudioSpecificConfig (a private copy), the media timescale (ticks per second),
// the track_ID that selects the matching track fragment in each media fragment,
// and the mvex/trex default sample size and duration that a track fragment may
// omit.
type AudioInit struct {
	// ASC is the MPEG-4 AudioSpecificConfig extracted verbatim from the esds
	// DecoderSpecificInfo. It is a private copy, safe to retain.
	ASC []byte
	// Timescale is the media timescale in ticks per second, from mdhd.
	Timescale uint32
	// TrackID is the audio track_ID, from tkhd; it selects the audio traf in a
	// media fragment.
	TrackID uint32
	// DefaultSize is the trex default_sample_size, used when a trun and its tfhd
	// carry no per-sample size. Zero when trex is absent.
	DefaultSize uint32
	// DefaultDur is the trex default_sample_duration, used when a trun and its
	// tfhd carry no per-sample duration. Zero when trex is absent.
	DefaultDur uint32
}

// Sample is one AAC access unit sliced from a media fragment: its raw bytes
// (aliasing the fragment buffer, valid only until the next fragment is parsed)
// and its duration in timescale ticks.
type Sample struct {
	// Data is the raw AAC access unit, aliasing the fragment buffer.
	Data []byte
	// Dur is the sample duration in timescale ticks.
	Dur uint32
}

// errStopBox halts eachBox early without reporting an error.
var errStopBox = errors.New("mp4: stop box iteration")

// boxSpan describes one box located during iteration: its four-character type and
// the byte ranges (relative to the buffer walked) of the whole box and of its
// payload.
type boxSpan struct {
	typ     string
	start   int // offset of the box header within the walked buffer
	bodyOff int // offset of the payload within the walked buffer
	bodyEnd int // end (exclusive) of the payload within the walked buffer
}

// eachBox iterates the top-level boxes in data, calling fn for each with offsets
// relative to the start of data. Returning errStopBox from fn halts iteration
// without error; any other error from fn is returned. A box whose declared size
// is too small, overruns the buffer, or carries an out-of-range 64-bit largesize
// returns ErrMalformedBox. The offset advances by the box length (always at least
// 8) each step, so iteration always terminates.
func eachBox(data []byte, fn func(b boxSpan) error) error {
	off := 0
	for off+8 <= len(data) {
		size := int64(binary.BigEndian.Uint32(data[off : off+4]))
		typ := string(data[off+4 : off+8])
		headerLen := int64(8)
		var boxLen int64
		switch size {
		case 0:
			// size 0 means the box extends to the end of the enclosing buffer.
			boxLen = int64(len(data) - off)
		case 1:
			// size 1 means a 64-bit largesize follows the type.
			if off+16 > len(data) {
				return ErrMalformedBox
			}
			large := binary.BigEndian.Uint64(data[off+8 : off+16])
			if large < 16 || large > uint64(len(data)-off) {
				return ErrMalformedBox
			}
			boxLen = int64(large)
			headerLen = 16
		default:
			if size < 8 || size > int64(len(data)-off) {
				return ErrMalformedBox
			}
			boxLen = size
		}
		b := boxSpan{
			typ:     typ,
			start:   off,
			bodyOff: off + int(headerLen),
			bodyEnd: off + int(boxLen),
		}
		if err := fn(b); err != nil {
			if errors.Is(err, errStopBox) {
				return nil
			}
			return err
		}
		off += int(boxLen)
	}
	return nil
}

// firstChild returns the payload of the first child box of data with the given
// type. found is false when no such box exists; err is non-nil only when a box
// structure is malformed.
func firstChild(data []byte, typ string) (body []byte, found bool, err error) {
	e := eachBox(data, func(b boxSpan) error {
		if b.typ == typ {
			body = data[b.bodyOff:b.bodyEnd]
			found = true
			return errStopBox
		}
		return nil
	})
	if e != nil {
		return nil, false, e
	}
	return body, found, nil
}

// ParseInit parses an fMP4 initialization segment and resolves the AAC audio
// track. It walks moov, selects the trak whose mdia/hdlr handler_type is 'soun'
// with an mp4a sample entry, and reads the track_ID (tkhd), timescale (mdhd) and
// AudioSpecificConfig (stsd>mp4a>esds), plus the mvex/trex defaults. It returns
// ErrNoAudioTrack when no audio track is present, ErrNoASC when the audio sample
// entry carries no AudioSpecificConfig, ErrUnsupportedSampleEntry when the audio
// track is encrypted or not AAC, and ErrMalformedBox for a broken structure.
func ParseInit(data []byte) (AudioInit, error) {
	moov, ok, err := firstChild(data, "moov")
	if err != nil {
		return AudioInit{}, err
	}
	if !ok {
		return AudioInit{}, fmt.Errorf("%w: no moov box", ErrMalformedBox)
	}
	trex, err := parseTrex(moov)
	if err != nil {
		return AudioInit{}, err
	}
	var (
		ai      AudioInit
		found   bool
		trakErr error
	)
	err = eachBox(moov, func(b boxSpan) error {
		if b.typ != "trak" {
			return nil
		}
		info, isAudio, terr := parseTrak(moov[b.bodyOff:b.bodyEnd])
		if terr != nil {
			trakErr = terr
			return errStopBox
		}
		if isAudio {
			ai = info
			found = true
			return errStopBox
		}
		return nil
	})
	if err != nil {
		return AudioInit{}, err
	}
	if trakErr != nil {
		return AudioInit{}, trakErr
	}
	if !found {
		return AudioInit{}, ErrNoAudioTrack
	}
	// A zero or out-of-range timescale cannot yield real sample durations (every
	// PTS would be 0, and on a 32-bit build a value above MaxInt32 would narrow
	// negative in the ticks-to-duration conversion), so reject it here rather
	// than deliver a stream whose media clock never advances.
	if ai.Timescale == 0 || ai.Timescale > math.MaxInt32 {
		return AudioInit{}, fmt.Errorf("%w: invalid media timescale %d", ErrMalformedBox, ai.Timescale)
	}
	if d, ok := trex[ai.TrackID]; ok {
		ai.DefaultDur = d.dur
		ai.DefaultSize = d.size
	}
	return ai, nil
}

// trexDefaults holds a trex box's default sample duration and size for one track.
type trexDefaults struct {
	dur  uint32
	size uint32
}

// parseTrex collects the mvex>trex defaults keyed by track_ID. A missing mvex or
// trex is not an error (defaults simply stay zero and per-sample values, when
// present, are used).
func parseTrex(moov []byte) (map[uint32]trexDefaults, error) {
	out := map[uint32]trexDefaults{}
	mvex, ok, err := firstChild(moov, "mvex")
	if err != nil {
		return nil, err
	}
	if !ok {
		return out, nil
	}
	err = eachBox(mvex, func(b boxSpan) error {
		if b.typ != "trex" {
			return nil
		}
		body := mvex[b.bodyOff:b.bodyEnd]
		// FullBox(4) + track_ID(4) + default_sample_description_index(4)
		// + default_sample_duration(4) + default_sample_size(4) + flags(4).
		if len(body) < 20 {
			return nil
		}
		id := binary.BigEndian.Uint32(body[4:8])
		out[id] = trexDefaults{
			dur:  binary.BigEndian.Uint32(body[12:16]),
			size: binary.BigEndian.Uint32(body[16:20]),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// parseTrak inspects one trak. isAudio is true only when the track handler is
// 'soun' and its sample entry is an mp4a with a usable AudioSpecificConfig. A
// 'soun' track that is encrypted or not AAC returns ErrUnsupportedSampleEntry; a
// 'soun' track missing its AudioSpecificConfig returns ErrNoASC. A non-audio
// track returns isAudio false with no error, so the walk continues.
func parseTrak(trak []byte) (AudioInit, bool, error) {
	var (
		trackID   uint32
		haveTrack bool
		mdia      []byte
	)
	err := eachBox(trak, func(b boxSpan) error {
		switch b.typ {
		case "tkhd":
			if id, ok := parseTKHD(trak[b.bodyOff:b.bodyEnd]); ok {
				trackID = id
				haveTrack = true
			}
		case "mdia":
			mdia = trak[b.bodyOff:b.bodyEnd]
		}
		return nil
	})
	if err != nil {
		return AudioInit{}, false, err
	}
	if mdia == nil {
		return AudioInit{}, false, nil
	}
	handler, timescale, stsd, err := parseMdia(mdia)
	if err != nil {
		return AudioInit{}, false, err
	}
	if handler != "soun" {
		return AudioInit{}, false, nil
	}
	if !haveTrack {
		return AudioInit{}, false, fmt.Errorf("%w: audio track has no tkhd track_ID", ErrMalformedBox)
	}
	asc, err := parseSTSDForASC(stsd)
	if err != nil {
		return AudioInit{}, false, err
	}
	return AudioInit{ASC: asc, Timescale: timescale, TrackID: trackID}, true, nil
}

// parseMdia reads the handler_type (hdlr), timescale (mdhd) and stsd payload from
// an mdia box. tkhd and mdia are siblings inside trak and mdhd/hdlr/minf are
// siblings inside mdia, so no ordering is assumed: each is located by type.
func parseMdia(mdia []byte) (handler string, timescale uint32, stsd []byte, err error) {
	if mdhd, ok, e := firstChild(mdia, "mdhd"); e != nil {
		return "", 0, nil, e
	} else if ok {
		timescale, _ = parseMDHD(mdhd)
	}
	if hdlr, ok, e := firstChild(mdia, "hdlr"); e != nil {
		return "", 0, nil, e
	} else if ok {
		handler = parseHDLR(hdlr)
	}
	minf, ok, e := firstChild(mdia, "minf")
	if e != nil {
		return "", 0, nil, e
	}
	if !ok {
		return handler, timescale, nil, nil
	}
	stbl, ok, e := firstChild(minf, "stbl")
	if e != nil {
		return "", 0, nil, e
	}
	if !ok {
		return handler, timescale, nil, nil
	}
	stsd, _, e = firstChild(stbl, "stsd")
	if e != nil {
		return "", 0, nil, e
	}
	return handler, timescale, stsd, nil
}

// parseTKHD reads the track_ID from a tkhd box, honoring the version 0 (32-bit
// times) and version 1 (64-bit times) layouts.
func parseTKHD(body []byte) (uint32, bool) {
	if len(body) < 4 {
		return 0, false
	}
	// FullBox(4) then, per version: creation, modification, track_ID.
	var off int
	if body[0] == 1 {
		off = 4 + 8 + 8
	} else {
		off = 4 + 4 + 4
	}
	if off+4 > len(body) {
		return 0, false
	}
	return binary.BigEndian.Uint32(body[off : off+4]), true
}

// parseMDHD reads the timescale from an mdhd box, honoring the version 0 and
// version 1 layouts.
func parseMDHD(body []byte) (uint32, bool) {
	if len(body) < 4 {
		return 0, false
	}
	// FullBox(4) then, per version: creation, modification, timescale.
	var off int
	if body[0] == 1 {
		off = 4 + 8 + 8
	} else {
		off = 4 + 4 + 4
	}
	if off+4 > len(body) {
		return 0, false
	}
	return binary.BigEndian.Uint32(body[off : off+4]), true
}

// parseHDLR reads the four-character handler_type from an hdlr box.
func parseHDLR(body []byte) string {
	// FullBox(4) + pre_defined(4) + handler_type(4).
	if len(body) < 12 {
		return ""
	}
	return string(body[8:12])
}

// parseSTSDForASC finds the mp4a sample entry in a stsd box and extracts its
// AudioSpecificConfig. An encrypted entry (enca and friends) or a non-mp4a entry
// is ErrUnsupportedSampleEntry; an mp4a with no esds/DecoderSpecificInfo is
// ErrNoASC.
func parseSTSDForASC(stsd []byte) ([]byte, error) {
	if stsd == nil {
		return nil, ErrNoASC
	}
	// FullBox(4) + entry_count(4), then the sample entry boxes.
	if len(stsd) < 8 {
		return nil, fmt.Errorf("%w: short stsd", ErrMalformedBox)
	}
	entries := stsd[8:]
	var (
		asc          []byte
		entryErr     error
		sawEncrypted bool
		firstType    string
	)
	err := eachBox(entries, func(b boxSpan) error {
		if firstType == "" {
			firstType = b.typ
		}
		switch b.typ {
		case "mp4a":
			a, e := parseMP4A(entries[b.bodyOff:b.bodyEnd])
			if e != nil {
				entryErr = e
				return errStopBox
			}
			asc = a
			return errStopBox
		case "enca", "encv", "encs", "drms", "drmi":
			sawEncrypted = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if entryErr != nil {
		return nil, entryErr
	}
	if asc != nil {
		return asc, nil
	}
	if sawEncrypted {
		return nil, fmt.Errorf("%w: encrypted sample entry", ErrUnsupportedSampleEntry)
	}
	if firstType != "" {
		return nil, fmt.Errorf("%w: sample entry %q is not mp4a", ErrUnsupportedSampleEntry, firstType)
	}
	return nil, ErrNoASC
}

// audioSampleEntryLen is the fixed prefix of an mp4a (AudioSampleEntry) box before
// its child boxes: SampleEntry (6 reserved + 2 data_reference_index) plus
// AudioSampleEntry (8 reserved + 2 channelcount + 2 samplesize + 2 pre_defined +
// 2 reserved + 4 samplerate). Version 0 is assumed, the CMAF form.
const audioSampleEntryLen = 28

// parseMP4A skips the AudioSampleEntry header and extracts the AudioSpecificConfig
// from the child esds box.
func parseMP4A(body []byte) ([]byte, error) {
	if len(body) < audioSampleEntryLen {
		return nil, fmt.Errorf("%w: short mp4a sample entry", ErrMalformedBox)
	}
	esds, ok, err := firstChild(body[audioSampleEntryLen:], "esds")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNoASC
	}
	return parseESDS(esds)
}

// MPEG-4 descriptor tags used within an esds box.
const (
	tagESDescriptor      = 0x03
	tagDecoderConfig     = 0x04
	tagDecoderSpecific   = 0x05
	objectTypeAAC        = 0x40 // Audio ISO/IEC 14496-3 (AAC)
	decoderConfigFixed   = 13   // objectTypeIndication + streamType + bufferSizeDB + max/avg bitrate
	esDescriptorMinFixed = 3    // ES_ID(2) + flags(1)
)

// parseESDS walks the esds descriptor tree (ES_Descriptor > DecoderConfigDescriptor
// > DecoderSpecificInfo) and returns the DecoderSpecificInfo bytes as the
// AudioSpecificConfig, as a private copy. It requires the DecoderConfigDescriptor
// objectTypeIndication to be AAC (0x40); any other value is
// ErrUnsupportedSampleEntry. A missing descriptor is ErrNoASC.
func parseESDS(esds []byte) ([]byte, error) {
	if len(esds) < 4 {
		return nil, fmt.Errorf("%w: short esds", ErrMalformedBox)
	}
	// Skip the FullBox version+flags.
	tag, es, _, err := readDescriptor(esds[4:])
	if err != nil || tag != tagESDescriptor {
		return nil, ErrNoASC
	}
	if len(es) < esDescriptorMinFixed {
		return nil, ErrNoASC
	}
	flags := es[2]
	off := esDescriptorMinFixed
	if flags&0x80 != 0 { // streamDependenceFlag: dependsOn_ES_ID (2)
		off += 2
	}
	if flags&0x40 != 0 { // URL_Flag: URLlength (1) + URLstring
		if off >= len(es) {
			return nil, ErrNoASC
		}
		off += 1 + int(es[off])
	}
	if flags&0x20 != 0 { // OCRstreamFlag: OCR_ES_Id (2)
		off += 2
	}
	if off > len(es) {
		return nil, ErrNoASC
	}
	tag, dcd, _, err := readDescriptor(es[off:])
	if err != nil || tag != tagDecoderConfig {
		return nil, ErrNoASC
	}
	if len(dcd) < decoderConfigFixed {
		return nil, ErrNoASC
	}
	if oti := dcd[0]; oti != objectTypeAAC {
		return nil, fmt.Errorf("%w: objectTypeIndication %#x is not AAC", ErrUnsupportedSampleEntry, oti)
	}
	tag, dsi, _, err := readDescriptor(dcd[decoderConfigFixed:])
	if err != nil || tag != tagDecoderSpecific {
		return nil, ErrNoASC
	}
	if len(dsi) == 0 {
		return nil, ErrNoASC
	}
	return append([]byte(nil), dsi...), nil
}

// readDescriptor reads one MPEG-4 descriptor from the front of b: its tag byte and
// its expandable length (at most four continuation bytes, so the reader is bounded
// against a hostile length). It returns the descriptor content and the bytes after
// it. A truncated descriptor is ErrMalformedBox.
func readDescriptor(b []byte) (tag byte, content, rest []byte, err error) {
	if len(b) < 2 {
		return 0, nil, nil, ErrMalformedBox
	}
	tag = b[0]
	i := 1
	size := 0
	for n := 0; n < 4; n++ {
		if i >= len(b) {
			return 0, nil, nil, ErrMalformedBox
		}
		c := b[i]
		i++
		size = size<<7 | int(c&0x7F)
		if c&0x80 == 0 {
			break
		}
	}
	if size < 0 || i+size > len(b) {
		return 0, nil, nil, ErrMalformedBox
	}
	return tag, b[i : i+size], b[i+size:], nil
}

// ParseFragment slices the AAC access units out of one media fragment and delivers
// each to onSample in order. It locates the moof, selects the traf whose tfhd
// track_ID equals init.TrackID (so a multiplexed audio+video fragment feeds only
// the audio samples), and computes each sample's absolute position in the whole
// fragment buffer from the moof start, the tfhd base_data_offset (defaulting to
// the moof start when the default-base-is-moof flag is set) and the trun
// data_offset. A sample whose data overruns the buffer stops delivery and returns
// ErrShortMdat; a structurally unusable fragment returns ErrMalformedBox. An error
// returned by onSample is propagated verbatim.
func ParseFragment(init AudioInit, frag []byte, onSample func(Sample) error) error {
	var (
		moofStart int
		moof      []byte
		haveMoof  bool
	)
	err := eachBox(frag, func(b boxSpan) error {
		if b.typ == "moof" {
			moofStart = b.start
			moof = frag[b.bodyOff:b.bodyEnd]
			haveMoof = true
			return errStopBox
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !haveMoof {
		return fmt.Errorf("%w: no moof box", ErrMalformedBox)
	}
	traf, ok, err := findAudioTraf(moof, init.TrackID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: no track fragment for track %d", ErrMalformedBox, init.TrackID)
	}
	// Bound the total samples delivered across the whole fragment, not just per
	// trun. Every delivered sample occupies at least one byte of the fragment,
	// so a fragment can never legitimately deliver more samples than it has
	// bytes. Without this, several defaults-only truns that each re-seat
	// data_offset to the same bytes could re-deliver them and drive a quadratic
	// number of onSample calls on a small hostile fragment.
	budget := len(frag)
	guarded := func(s Sample) error {
		if budget <= 0 {
			return fmt.Errorf("%w: fragment delivers more samples than its bytes allow", ErrMalformedBox)
		}
		budget--
		return onSample(s)
	}
	return parseTraf(traf, moofStart, frag, init, guarded)
}

// findAudioTraf returns the traf payload whose tfhd track_ID matches trackID.
func findAudioTraf(moof []byte, trackID uint32) (traf []byte, found bool, err error) {
	var inner error
	e := eachBox(moof, func(b boxSpan) error {
		if b.typ != "traf" {
			return nil
		}
		body := moof[b.bodyOff:b.bodyEnd]
		tfhd, ok, terr := firstChild(body, "tfhd")
		if terr != nil {
			inner = terr
			return errStopBox
		}
		if !ok {
			return nil
		}
		id, ok := tfhdTrackID(tfhd)
		if ok && id == trackID {
			traf = body
			found = true
			return errStopBox
		}
		return nil
	})
	if e != nil {
		return nil, false, e
	}
	if inner != nil {
		return nil, false, inner
	}
	return traf, found, nil
}

// tfhd flag bits (ISO/IEC 14496-12).
const (
	tfhdBaseDataOffset    = 0x000001
	tfhdSampleDescIndex   = 0x000002
	tfhdDefaultDuration   = 0x000008
	tfhdDefaultSize       = 0x000010
	tfhdDefaultFlags      = 0x000020
	tfhdDefaultBaseIsMoof = 0x020000
)

// tfhdTrackID reads the track_ID from a tfhd box (FullBox then track_ID).
func tfhdTrackID(tfhd []byte) (uint32, bool) {
	if len(tfhd) < 8 {
		return 0, false
	}
	return binary.BigEndian.Uint32(tfhd[4:8]), true
}

// parseTraf reads the tfhd defaults and walks every trun in the track fragment,
// delivering samples. It threads a running absolute data pointer across truns so a
// trun without its own data_offset continues where the previous one ended.
func parseTraf(traf []byte, moofStart int, frag []byte, init AudioInit, onSample func(Sample) error) error {
	tfhd, ok, err := firstChild(traf, "tfhd")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: track fragment has no tfhd", ErrMalformedBox)
	}
	base, defDur, defSize, baseExplicit, err := parseTFHD(tfhd, moofStart)
	if err != nil {
		return err
	}
	if defDur == 0 {
		defDur = init.DefaultDur
	}
	if defSize == 0 {
		defSize = init.DefaultSize
	}

	dataPtr := base
	// located tracks whether the sample data position is reliably known. It is
	// true once an explicit tfhd base_data_offset or a trun data_offset has fixed
	// it; a trun that omits its data_offset before that point cannot be placed
	// (the default base is the moof start, which is not the media data) and is
	// rejected in parseTrun.
	located := baseExplicit
	short := false
	var inner error
	e := eachBox(traf, func(b boxSpan) error {
		if b.typ != "trun" {
			return nil
		}
		next, hadOffset, wasShort, terr := parseTrun(traf[b.bodyOff:b.bodyEnd], base, dataPtr, located, defDur, defSize, frag, onSample)
		if terr != nil {
			inner = terr
			return errStopBox
		}
		if hadOffset {
			located = true
		}
		dataPtr = next
		if wasShort {
			short = true
			return errStopBox
		}
		return nil
	})
	if e != nil {
		return e
	}
	if inner != nil {
		return inner
	}
	if short {
		return ErrShortMdat
	}
	return nil
}

// parseTFHD reads the base_data_offset and default sample duration/size from a
// tfhd. base is the moof start when the default-base-is-moof flag is set (the CMAF
// case) or when no base is signalled, and the explicit base_data_offset otherwise.
func parseTFHD(tfhd []byte, moofStart int) (base int, defDur, defSize uint32, baseExplicit bool, err error) {
	if len(tfhd) < 8 {
		return 0, 0, 0, false, fmt.Errorf("%w: short tfhd", ErrMalformedBox)
	}
	flags := uint32(tfhd[1])<<16 | uint32(tfhd[2])<<8 | uint32(tfhd[3])
	off := 8 // FullBox(4) + track_ID(4)
	base = moofStart
	if flags&tfhdBaseDataOffset != 0 {
		if off+8 > len(tfhd) {
			return 0, 0, 0, false, fmt.Errorf("%w: tfhd base_data_offset truncated", ErrMalformedBox)
		}
		v := binary.BigEndian.Uint64(tfhd[off : off+8])
		if v > math.MaxInt32 {
			return 0, 0, 0, false, fmt.Errorf("%w: tfhd base_data_offset out of range", ErrMalformedBox)
		}
		base = int(v)
		baseExplicit = true
		off += 8
	}
	if flags&tfhdSampleDescIndex != 0 {
		if off+4 > len(tfhd) {
			return 0, 0, 0, false, fmt.Errorf("%w: tfhd sample_description_index truncated", ErrMalformedBox)
		}
		off += 4
	}
	if flags&tfhdDefaultDuration != 0 {
		if off+4 > len(tfhd) {
			return 0, 0, 0, false, fmt.Errorf("%w: tfhd default_sample_duration truncated", ErrMalformedBox)
		}
		defDur = binary.BigEndian.Uint32(tfhd[off : off+4])
		off += 4
	}
	if flags&tfhdDefaultSize != 0 {
		if off+4 > len(tfhd) {
			return 0, 0, 0, false, fmt.Errorf("%w: tfhd default_sample_size truncated", ErrMalformedBox)
		}
		defSize = binary.BigEndian.Uint32(tfhd[off : off+4])
	}
	// default_sample_flags (tfhdDefaultFlags), if present, follows here but is not
	// needed for audio slicing, so off is not advanced past default_sample_size.
	return base, defDur, defSize, baseExplicit, nil
}

// trun flag bits (ISO/IEC 14496-12).
const (
	trunDataOffset       = 0x000001
	trunFirstSampleFlags = 0x000004
	trunSampleDuration   = 0x000100
	trunSampleSize       = 0x000200
	trunSampleFlags      = 0x000400
	trunSampleCTO        = 0x000800
)

// parseTrun walks one trun, delivering each sample. base is the tfhd base offset
// and running is the absolute data pointer carried in from a previous trun; the
// trun data_offset (when present) reseats the pointer to base+data_offset. The
// sample_count is validated against the remaining trun buffer before iterating.
// next is the data pointer after the last delivered sample; wasShort is true when
// a sample's data overran the fragment buffer, which stops delivery for a counted
// gap rather than a panic.
func parseTrun(trun []byte, base, running int, located bool, defDur, defSize uint32, frag []byte, onSample func(Sample) error) (next int, hadOffset, wasShort bool, err error) {
	if len(trun) < 8 {
		return running, false, false, fmt.Errorf("%w: short trun", ErrMalformedBox)
	}
	flags := uint32(trun[1])<<16 | uint32(trun[2])<<8 | uint32(trun[3])
	sampleCount := binary.BigEndian.Uint32(trun[4:8])
	off := 8
	dataPtr := running
	hadOffset = flags&trunDataOffset != 0
	if hadOffset {
		if off+4 > len(trun) {
			return running, false, false, fmt.Errorf("%w: trun data_offset truncated", ErrMalformedBox)
		}
		dataOffset := int32(binary.BigEndian.Uint32(trun[off : off+4]))
		off += 4
		dataPtr = base + int(dataOffset)
	} else if !located {
		// This trun carries no explicit data_offset and no base has been
		// established (no tfhd base_data_offset and no preceding trun offset in
		// this traf), so the sample data cannot be located: base defaults to the
		// moof start, which addresses the moof box, not the media in mdat. Fail
		// closed rather than slice the moof header (or, in a multiplexed segment,
		// another track's bytes) as AAC.
		return running, false, false, fmt.Errorf("%w: trun has no data_offset and no base was established", ErrMalformedBox)
	}
	if flags&trunFirstSampleFlags != 0 {
		if off+4 > len(trun) {
			return running, hadOffset, false, fmt.Errorf("%w: trun first_sample_flags truncated", ErrMalformedBox)
		}
		off += 4
	}
	perSample := 0
	if flags&trunSampleDuration != 0 {
		perSample += 4
	}
	if flags&trunSampleSize != 0 {
		perSample += 4
	}
	if flags&trunSampleFlags != 0 {
		perSample += 4
	}
	if flags&trunSampleCTO != 0 {
		perSample += 4
	}
	// Validate sample_count against the buffer before iterating, so a hostile
	// count cannot drive an out-of-range read or an unbounded loop.
	remaining := len(trun) - off
	switch {
	case perSample > 0:
		if uint64(sampleCount) > uint64(remaining/perSample) {
			return running, hadOffset, false, fmt.Errorf("%w: trun sample_count exceeds the box", ErrMalformedBox)
		}
	default:
		// No per-sample records: every sample uses the defaults, so the count is
		// bounded only by how much sample data the fragment can hold.
		if uint64(sampleCount) > uint64(len(frag)) {
			return running, hadOffset, false, fmt.Errorf("%w: trun sample_count exceeds the fragment", ErrMalformedBox)
		}
	}
	for s := uint32(0); s < sampleCount; s++ {
		dur := defDur
		size := defSize
		if flags&trunSampleDuration != 0 {
			dur = binary.BigEndian.Uint32(trun[off : off+4])
			off += 4
		}
		if flags&trunSampleSize != 0 {
			size = binary.BigEndian.Uint32(trun[off : off+4])
			off += 4
		}
		if flags&trunSampleFlags != 0 {
			off += 4
		}
		if flags&trunSampleCTO != 0 {
			off += 4
		}
		if size == 0 || dataPtr < 0 {
			return dataPtr, hadOffset, true, nil
		}
		end := dataPtr + int(size)
		if end < dataPtr || end > len(frag) {
			return dataPtr, hadOffset, true, nil
		}
		if err := onSample(Sample{Data: frag[dataPtr:end], Dur: dur}); err != nil {
			return dataPtr, hadOffset, false, err
		}
		dataPtr = end
	}
	return dataPtr, hadOffset, false, nil
}
