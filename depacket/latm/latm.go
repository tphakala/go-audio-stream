package latm

import (
	"errors"
	"fmt"
)

// Caps on a single payload. All input is untrusted; these bounds keep a
// hostile payload from exhausting memory or CPU.
const (
	// MaxSubFrames is the largest number of subframe access units this
	// package extracts from one AudioMuxElement. A StreamMuxConfig declaring
	// more yields ErrUnsupportedMux.
	MaxSubFrames = 64
	// MaxMuxSlotBytes is the largest single access unit, in bytes, the
	// PayloadLengthInfo byte-sum may declare. A larger value yields
	// ErrPayloadOverflow before any buffering.
	MaxMuxSlotBytes = 64 * 1024
	// MaxStreamMuxConfigBytes bounds an out-of-band or in-band
	// StreamMuxConfig parse, so a malformed config cannot loop unbounded.
	MaxStreamMuxConfigBytes = 512
)

// Sentinel errors. New and Depacketize return one of these (never any other
// error value) and never panic.
var (
	// ErrConfigInvalid is returned by New when Config is inconsistent: an
	// out-of-band mode with an empty or unparseable StreamMuxConfig, or a
	// SamplesPerFrame outside 0..8192.
	ErrConfigInvalid = errors.New("latm: invalid config")
	// ErrUnsupportedMux is returned when the StreamMuxConfig uses a shape
	// this package does not support: audioMuxVersion != 0,
	// allStreamsSameTimeFraming != 1, numProgram != 0, numLayer != 0,
	// frameLengthType != 0, or numSubFrames+1 > MaxSubFrames.
	ErrUnsupportedMux = errors.New("latm: unsupported mux configuration")
	// ErrUnsupportedASC is returned when the AudioSpecificConfig uses an
	// audio object type this package's length parse does not cover.
	ErrUnsupportedASC = errors.New("latm: unsupported audio-specific-config")
	// ErrNoConfig is returned when an in-band AudioMuxElement sets
	// useSameStreamMux before any StreamMuxConfig has been received.
	ErrNoConfig = errors.New("latm: useSameStreamMux with no prior config")
	// ErrTruncated is returned when the payload ends before a declared field,
	// length group, or payload byte count is complete.
	ErrTruncated = errors.New("latm: truncated AudioMuxElement")
	// ErrPayloadOverflow is returned when a MuxSlotLengthBytes value exceeds
	// MaxMuxSlotBytes or the payload data present.
	ErrPayloadOverflow = errors.New("latm: payload length exceeds available data")
)

// Config configures a Depacketizer. The caller maps these from its SDP fmtp
// parameters for an MP4A-LATM track (see sdp.LATMParams): MuxConfigPresent
// from cpresent, StreamMuxConfig from the config= hex.
type Config struct {
	// MuxConfigPresent mirrors RFC 3016 cpresent. False means the
	// StreamMuxConfig is out-of-band in StreamMuxConfig; true means each
	// AudioMuxElement carries it in-band (or reuses a retained one).
	MuxConfigPresent bool
	// StreamMuxConfig is the out-of-band StreamMuxConfig bytes (the SDP
	// config= value for a LATM track, which is a StreamMuxConfig, not a bare
	// AudioSpecificConfig). Required when MuxConfigPresent is false; ignored
	// otherwise.
	StreamMuxConfig []byte
	// SamplesPerFrame overrides the per-subframe RTP tick increment. Zero
	// uses the value derived from the ASC frameLengthFlag (1024 or 960).
	SamplesPerFrame int
}

// AU is one depacketized access unit: the AAC bytes and this AU's RTP
// timestamp offset from the packet timestamp, in RTP clock ticks. The first
// subframe has RTPOffset 0; each subsequent subframe adds the AAC frame
// length. In out-of-band mode (Config.MuxConfigPresent == false) Data
// aliases the input payload passed to Depacketize. In in-band mode Data
// aliases the Depacketizer's internal buffer instead: the in-band payload
// bytes are bit-misaligned in the AudioMuxElement, so they are repacked
// MSB-first into that buffer and cannot alias the input directly. Either
// way, Data is valid only until the next Depacketize or Reset call; copy to
// retain.
type AU struct {
	// Data is the raw AAC access-unit bytes, ready for an AAC decoder such as
	// github.com/tphakala/go-aac.
	Data []byte
	// RTPOffset is this AU's RTP timestamp offset from the packet timestamp,
	// in RTP clock ticks.
	RTPOffset uint32
}

// Depacketizer turns MP4A-LATM RTP payloads into AAC access units. It retains
// the StreamMuxConfig (out-of-band from New, or in-band across packets when
// useSameStreamMux is set), so one Depacketizer serves one RTP stream and is
// not safe for concurrent use.
type Depacketizer struct {
	cfg         Config
	smc         streamMuxConfig
	haveSMC     bool
	asc         []byte
	frameLength uint32
	inBandData  []byte // reused scratch for in-band AU bytes repacked MSB-first from the bit-misaligned payload
	aus         []AU   // reused scratch across calls
}

// New validates cfg and returns a Depacketizer. For the out-of-band mode it
// parses Config.StreamMuxConfig once, extracting the ASC and mux parameters,
// and returns ErrConfigInvalid or ErrUnsupportedMux/ErrUnsupportedASC on a bad
// config. For the in-band mode it returns a Depacketizer that learns the
// config from the first packet.
func New(cfg Config) (*Depacketizer, error) {
	if cfg.SamplesPerFrame < 0 || cfg.SamplesPerFrame > 8192 {
		return nil, fmt.Errorf("%w: SamplesPerFrame %d not in 0..8192", ErrConfigInvalid, cfg.SamplesPerFrame)
	}

	d := &Depacketizer{cfg: cfg}
	if cfg.MuxConfigPresent {
		return d, nil
	}

	if len(cfg.StreamMuxConfig) == 0 {
		return nil, fmt.Errorf("%w: out-of-band mode requires a non-empty StreamMuxConfig", ErrConfigInvalid)
	}
	smc, asc, frameLength, err := parseStreamMuxConfig(cfg.StreamMuxConfig)
	if err != nil {
		return nil, err
	}
	d.smc = smc
	d.asc = asc
	d.frameLength = frameLength
	d.haveSMC = true
	return d, nil
}

// AudioSpecificConfig returns the AAC AudioSpecificConfig extracted from the
// StreamMuxConfig (out-of-band at New, or from the first in-band packet), or
// nil before any config has been seen. The returned slice is owned by the
// Depacketizer; copy to retain across calls.
func (d *Depacketizer) AudioSpecificConfig() []byte {
	if !d.haveSMC {
		return nil
	}
	return d.asc
}

// Reset discards any retained in-band StreamMuxConfig learned from the stream,
// so a following packet must re-carry the config. The caller invokes it on an
// SSRC change. It does not clear an out-of-band config supplied at New.
func (d *Depacketizer) Reset() {
	if !d.cfg.MuxConfigPresent {
		return
	}
	d.haveSMC = false
	d.smc = streamMuxConfig{}
	d.asc = nil
	d.frameLength = 0
}
