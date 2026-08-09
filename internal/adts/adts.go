// Package adts parses MPEG-2/4 AAC ADTS frame headers. It frames, it never
// decodes: Parse reads the 7-byte fixed header (reporting a 9-byte header length
// when the CRC-protected variant is signaled, though it does not read the CRC) and reports
// the frame's byte length, header length, and the fields needed to synthesize
// an AudioSpecificConfig, so a streaming framer can cut an ADTS byte stream into
// whole coded frames and hand each raw access unit to a decoder. It has no
// dependencies beyond the standard library.
package adts

import "errors"

// Header lengths in bytes: MinHeaderLen without the optional CRC,
// CRCHeaderLen with it (protection_absent == 0).
const (
	MinHeaderLen = 7
	CRCHeaderLen = 9
)

// SamplesPerFrame is the number of samples per channel an ADTS AAC frame decodes
// to. ADTS always carries 1024-sample frames; the 480/512-sample AAC-LD/ELD
// object types are not transported in ADTS, so a framer can treat it as fixed.
const SamplesPerFrame = 1024

// ErrInvalidHeader reports a byte sequence that is not a valid, supported ADTS
// header: too short to hold the fixed header, the syncword absent, the layer
// field not 00, the sampling-frequency index reserved (13, 14) or the escape
// value (15) whose explicit frequency a 2-byte ASC cannot express, the channel
// configuration 0 (its layout is carried by an in-band program_config_element a
// 2-byte ASC cannot express), or the frame length shorter than its own header.
var ErrInvalidHeader = errors.New("adts: invalid frame header")

// sampleRateHz maps the 4-bit sampling_frequency_index to a rate in Hz. Indices
// 13 and 14 are reserved and 15 is the escape value; all three are 0 here, so a
// single "== 0" check in Parse rejects them.
var sampleRateHz = [16]int{
	96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050,
	16000, 12000, 11025, 8000, 7350, 0, 0, 0,
}

// Header is a parsed ADTS frame header.
type Header struct {
	// AudioObjectType is the MPEG-4 AAC object type (the 2-bit profile field
	// plus one): 1 Main, 2 LC, 3 SSR, 4 LTP. It is the top field of the ASC.
	AudioObjectType int
	// SampleRateIndex is the 4-bit sampling_frequency_index (0..12).
	SampleRateIndex int
	// SampleRate is the sampling rate in Hz for SampleRateIndex.
	SampleRate int
	// ChannelConfig is the 3-bit channel_configuration (1..7).
	ChannelConfig int
	// Channels is the channel count ChannelConfig denotes (config 7 is 7.1, i.e.
	// 8 channels; 1..6 map to themselves).
	Channels int
	// HeaderLen is the header size in bytes: MinHeaderLen, or CRCHeaderLen when
	// the CRC is present. The raw access unit begins at this offset.
	HeaderLen int
	// FrameLen is the whole frame length in bytes, header (and CRC) included. A
	// framer advances by it to reach the next frame; the access unit is
	// frame[HeaderLen:FrameLen].
	FrameLen int
	// NumRawBlocks is number_of_raw_data_blocks_in_frame: the frame carries
	// NumRawBlocks+1 raw data blocks. Only 0 (a single block) is deliverable as
	// one access unit; the fixed header does not carry the per-block boundaries a
	// multi-block split would need.
	NumRawBlocks int
	// MPEG2 reports the ID bit: true for MPEG-2 AAC, false for MPEG-4. It does
	// not change the object-type mapping or the synthesized ASC.
	MPEG2 bool
}

// channelCount maps the 3-bit channel_configuration to a channel count. Config 0
// (in-band PCE) is rejected by Parse before this is called; config 7 is 7.1
// (8 channels); 1..6 map to themselves.
func channelCount(cfg int) int {
	if cfg == 7 {
		return 8
	}
	return cfg
}

// Parse decodes the ADTS header at the front of b and returns its geometry, or
// ErrInvalidHeader when b is not a valid, supported header. b shorter than
// MinHeaderLen is ErrInvalidHeader, so a framer buffers more bytes and retries.
// It performs no I/O and allocates nothing.
func Parse(b []byte) (Header, error) {
	if len(b) < MinHeaderLen {
		return Header{}, ErrInvalidHeader
	}
	// syncword: 12 bits, all ones (b[0] and the top nibble of b[1]).
	if b[0] != 0xFF || b[1]&0xF0 != 0xF0 {
		return Header{}, ErrInvalidHeader
	}
	// layer (b[1] bits 2..1) must be 00 for ADTS.
	if b[1]&0x06 != 0 {
		return Header{}, ErrInvalidHeader
	}
	mpeg2 := b[1]&0x08 != 0 // ID bit: 1 = MPEG-2, 0 = MPEG-4.
	protectionAbsent := b[1]&0x01 != 0

	profile := int(b[2]>>6) & 0x03
	srIdx := int(b[2]>>2) & 0x0F
	chanCfg := (int(b[2]&0x01) << 2) | (int(b[3]>>6) & 0x03)

	rate := sampleRateHz[srIdx]
	if rate == 0 {
		return Header{}, ErrInvalidHeader // reserved (13, 14) or escape (15)
	}
	if chanCfg < 1 || chanCfg > 7 {
		return Header{}, ErrInvalidHeader // 0 is PCE-defined; the field maxes at 7
	}

	frameLen := (int(b[3]&0x03) << 11) | (int(b[4]) << 3) | (int(b[5]) >> 5)
	headerLen := MinHeaderLen
	if !protectionAbsent {
		headerLen = CRCHeaderLen
	}
	if frameLen < headerLen {
		return Header{}, ErrInvalidHeader
	}

	return Header{
		AudioObjectType: profile + 1,
		SampleRateIndex: srIdx,
		SampleRate:      rate,
		ChannelConfig:   chanCfg,
		Channels:        channelCount(chanCfg),
		HeaderLen:       headerLen,
		FrameLen:        frameLen,
		NumRawBlocks:    int(b[6] & 0x03),
		MPEG2:           mpeg2,
	}, nil
}

// AudioSpecificConfig synthesizes the 2-byte MPEG-4 AudioSpecificConfig this
// header describes: 5 bits audioObjectType, 4 bits samplingFrequencyIndex, 4
// bits channelConfiguration, then a 3-bit GASpecificConfig of zeros
// (frameLengthFlag, dependsOnCoreCoder, extensionFlag all 0). It is what a
// decoder such as go-aac needs, and it makes an ADTS-framed AAC track carry the
// same CodecAAC.AudioSpecificConfig an RTSP AAC track resolves from its SDP, so
// both decode through one path. All three fields are already range-checked by
// Parse, so they fit their bit widths.
func (h Header) AudioSpecificConfig() []byte {
	asc := uint16(h.AudioObjectType)<<11 | uint16(h.SampleRateIndex)<<7 | uint16(h.ChannelConfig)<<3
	return []byte{byte(asc >> 8), byte(asc)}
}
