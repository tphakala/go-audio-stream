// Package mp3 parses MPEG-1/2/2.5 Audio Layer I/II/III frame headers. It frames,
// it never decodes: Parse reads the 4-byte header and reports the frame's byte
// length and geometry so a streaming framer can cut the byte stream into whole
// coded frames. It has no dependencies beyond the standard library.
package mp3

import "errors"

// HeaderLen is the size in bytes of an MPEG audio frame header.
const HeaderLen = 4

// ErrInvalidHeader reports a 4-byte value that is not a valid, supported MPEG
// audio frame header: the sync word is absent, a reserved version/layer/
// emphasis is set, the bitrate index is free-format (0) or bad (15), or the
// sampling-rate index is reserved (3). Free-format is intentionally rejected:
// its frame length is not carried in the header (it must be inferred from the
// distance to the next sync), and no mainstream encoder emits it.
var ErrInvalidHeader = errors.New("mp3: invalid frame header")

// Raw 2-bit version-ID field values (header bits 20-19).
const (
	version25       = 0 // MPEG Version 2.5 (unofficial; bit 20 is 0)
	versionReserved = 1
	version2        = 2 // MPEG Version 2
	version1        = 3 // MPEG Version 1
)

// Raw 2-bit layer field values (header bits 18-17).
const (
	layerReserved = 0
	layerIII      = 1
	layerII       = 2
	layerI        = 3
)

// channelModeMono is the raw channel-mode value for single-channel audio
// (header bits 7-6 == 11); every other value is two-channel.
const channelModeMono = 3

// emphasisReserved is the reserved emphasis value (header bits 1-0 == 10).
const emphasisReserved = 2

// syncMask isolates the 11-bit frame sync (bits 31-21). A robust parser checks
// only these 11 bits, not 12: bit 20 is 0 for MPEG 2.5, so a 12-bit sync check
// would silently drop every MPEG 2.5 frame.
const syncMask = 0xFFE00000

// bitrateKbps maps [version][layer][index] to a bitrate in kbps. Reserved
// version and layer rows, and the free-format (0) and bad (15) indices, are 0,
// so a single "== 0" check rejects them all. MPEG 2.5 shares MPEG 2's table.
var bitrateKbps = [4][4][16]int{
	version25: {
		layerIII: {0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},
		layerII:  {0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},
		layerI:   {0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256, 0},
	},
	version2: {
		layerIII: {0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},
		layerII:  {0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},
		layerI:   {0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256, 0},
	},
	version1: {
		layerIII: {0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0},
		layerII:  {0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 0},
		layerI:   {0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448, 0},
	},
}

// sampleRateHz maps [version][index] to a sampling rate in Hz. The reserved
// index (3) and the reserved version row are 0.
var sampleRateHz = [4][4]int{
	version25: {11025, 12000, 8000, 0},
	version2:  {22050, 24000, 16000, 0},
	version1:  {44100, 48000, 32000, 0},
}

// Header is a parsed MPEG audio frame header.
type Header struct {
	// Version is the raw 2-bit version-ID field (0 = MPEG 2.5, 2 = MPEG 2,
	// 3 = MPEG 1); the reserved value 1 never appears in a parsed Header.
	Version int
	// Layer is the raw 2-bit layer field (1 = Layer III, 2 = Layer II,
	// 3 = Layer I); the reserved value 0 never appears in a parsed Header.
	Layer int
	// ChannelMode is the raw 2-bit channel-mode field (3 = mono, else two
	// channels).
	ChannelMode int
	// SampleRate is the audio sampling rate in Hz.
	SampleRate int
	// Bitrate is the audio bitrate in bits per second.
	Bitrate int
	// Channels is 1 for mono and 2 otherwise. It comes from the channel-mode
	// field, not from decoding, so a joint-stereo frame reports 2.
	Channels int
	// SamplesPerFrame is the number of PCM samples per channel this frame
	// decodes to, fixed by version and layer. It lets a framer derive the
	// frame's duration without decoding.
	SamplesPerFrame int
	// FrameLen is the total frame length in bytes, header included, padding
	// included. It is what a framer advances by to reach the next frame.
	FrameLen int
	// Padding reports whether the padding bit was set (one extra slot).
	Padding bool
}

// Parse decodes a 4-byte MPEG audio frame header packed big-endian into h and
// returns its geometry, or ErrInvalidHeader when h is not a valid, supported
// header. It performs no I/O and allocates nothing.
func Parse(h uint32) (Header, error) {
	if h&syncMask != syncMask {
		return Header{}, ErrInvalidHeader
	}
	version := int((h >> 19) & 0x3)
	layer := int((h >> 17) & 0x3)
	bitrateIdx := int((h >> 12) & 0xF)
	srIdx := int((h >> 10) & 0x3)
	padding := int((h >> 9) & 0x1)
	channelMode := int((h >> 6) & 0x3)
	emphasis := int(h & 0x3)

	if version == versionReserved || layer == layerReserved || emphasis == emphasisReserved {
		return Header{}, ErrInvalidHeader
	}
	// Reserved rows and the free-format/bad indices are 0 in both tables, so
	// these two checks also cover version==reserved and layer==reserved.
	kbps := bitrateKbps[version][layer][bitrateIdx]
	if kbps == 0 {
		return Header{}, ErrInvalidHeader
	}
	rate := sampleRateHz[version][srIdx]
	if rate == 0 {
		return Header{}, ErrInvalidHeader
	}

	bps := kbps * 1000
	frameLen := frameLenBytes(version, layer, bps, rate, padding)
	if frameLen < HeaderLen {
		return Header{}, ErrInvalidHeader
	}
	channels := 2
	if channelMode == channelModeMono {
		channels = 1
	}
	return Header{
		Version:         version,
		Layer:           layer,
		ChannelMode:     channelMode,
		SampleRate:      rate,
		Bitrate:         bps,
		Channels:        channels,
		SamplesPerFrame: samplesPerFrame(version, layer),
		FrameLen:        frameLen,
		Padding:         padding == 1,
	}, nil
}

// frameLenBytes returns the whole frame length in bytes. Layer I counts in
// 4-byte slots; Layers II and III count in 1-byte slots. MPEG-1 Layer III packs
// twice the samples per frame of MPEG-2/2.5 Layer III, so its slot count uses
// 144 rather than 72. layer is never layerReserved here (Parse rejected it).
func frameLenBytes(version, layer, bps, rate, padding int) int {
	switch layer {
	case layerI:
		return (12*bps/rate + padding) * 4
	case layerII:
		return 144*bps/rate + padding
	default: // layerIII
		if version == version1 {
			return 144*bps/rate + padding
		}
		return 72*bps/rate + padding
	}
}

// samplesPerFrame returns the PCM samples per channel a frame decodes to. It is
// fixed by version and layer: Layer I is 384, Layer II is 1152, and Layer III
// is 1152 for MPEG-1 but 576 for MPEG-2/2.5.
func samplesPerFrame(version, layer int) int {
	switch layer {
	case layerI:
		return 384
	case layerII:
		return 1152
	default: // layerIII
		if version == version1 {
			return 1152
		}
		return 576
	}
}
