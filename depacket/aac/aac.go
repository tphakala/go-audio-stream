package aac

import (
	"errors"
	"fmt"
)

// Caps on a single packet. All payload is untrusted; these bounds keep a
// hostile packet from exhausting memory or CPU.
const (
	// MaxAUsPerPacket is the largest number of access-unit headers this
	// package parses from one RTP packet. A packet declaring more yields
	// ErrTruncatedHeader.
	MaxAUsPerPacket = 512
	// MaxFragmentSize is the largest access unit, in bytes, that the
	// fragment reassembler will buffer across packets. A declared or
	// accumulated size beyond this yields ErrFragmentOverflow.
	MaxFragmentSize = 64 * 1024
)

// Sentinel errors. Depacketize and New return one of these (never any
// other error value) and never panic.
var (
	// ErrConfigInvalid is returned by New when a Config field is zero,
	// negative, or beyond its supported range.
	ErrConfigInvalid = errors.New("aac: invalid config")
	// ErrTruncatedHeader is returned when the AU-headers section is
	// shorter than the declared AU-headers-length, the length is zero,
	// the declared bit count does not divide into whole AU-headers, or
	// the packet declares more than MaxAUsPerPacket headers.
	ErrTruncatedHeader = errors.New("aac: truncated or inconsistent AU headers")
	// ErrAUSizeOverflow is returned when an AU-size field points beyond
	// the access-unit data present in a packet that claims to be
	// complete (marker set, or a multi-AU packet), or when fragment
	// reassembly ends with a byte count that does not match the size the
	// first fragment declared: a fragment marked final that is short, or
	// the declared size exhausted with no final marker.
	ErrAUSizeOverflow = errors.New("aac: AU size exceeds available payload")
	// ErrFragmentOverflow is returned when a fragmented access unit's
	// declared or accumulated size exceeds MaxFragmentSize, or when the
	// accumulated fragment bytes exceed the size declared by the first
	// fragment.
	ErrFragmentOverflow = errors.New("aac: fragment reassembly overflow")
	// ErrInterleavingUnsupported is returned when a non-first AU-header
	// carries a non-zero AU-Index-delta, signaling interleaved access
	// units, which this package does not reorder.
	ErrInterleavingUnsupported = errors.New("aac: interleaved access units unsupported")
)

// Config holds the RFC 3640 MPEG4-GENERIC AU-header field widths for mode
// AAC-hbr, plus the per-access-unit RTP timestamp increment. The caller
// maps these from its SDP fmtp parameters (see sdp.AACParams). The common
// AAC-hbr case is SizeLength=13, IndexLength=3, IndexDeltaLength=3, but
// New honors whatever valid values it is given.
type Config struct {
	// SizeLength is the AU-size field width in bits (AAC-hbr: 13).
	SizeLength int
	// IndexLength is the AU-Index field width in bits for the first
	// AU-header in a packet (AAC-hbr: 3).
	IndexLength int
	// IndexDeltaLength is the AU-Index-delta field width in bits for
	// every non-first AU-header in a packet (AAC-hbr: 3).
	IndexDeltaLength int
	// SamplesPerFrame is the RTP timestamp increment per access unit,
	// equal to the number of PCM samples one AAC frame decodes to
	// because RFC 3640 clocks AAC RTP at the audio sample rate. Typical
	// values: 1024 (AAC-LC) or 960.
	SamplesPerFrame int
}

// AU is one depacketized access unit: the codec bytes and the RTP
// timestamp offset of this AU relative to the packet's rtpTime, in RTP
// clock ticks. The first AU of a packet has RTPOffset 0; each subsequent
// AU adds SamplesPerFrame ticks. A reassembled fragmented AU has
// RTPOffset 0.
//
// Data may alias the input payload or the depacketizer's internal
// reassembly buffer. It is valid only until the next call to Depacketize
// or Reset; copy to retain.
type AU struct {
	// Data is the raw access-unit bytes, ready to hand to an AAC decoder
	// such as github.com/tphakala/go-aac.
	Data []byte
	// RTPOffset is this AU's RTP timestamp offset from the packet
	// rtpTime, in RTP clock ticks.
	RTPOffset uint32
}

// Depacketizer turns RFC 3640 AAC-hbr RTP payloads into access units. It
// carries the reassembly state for access units fragmented across
// packets, so a single Depacketizer instance serves one RTP stream and is
// not safe for concurrent use.
type Depacketizer struct {
	cfg           Config
	frag          []byte
	fragActive    bool
	fragTotalSize int
	aus           []AU       // reused scratch across calls
	headers       []auHeader // reused scratch; never escapes Depacketize
}

// New validates cfg and returns a Depacketizer. It returns ErrConfigInvalid
// (wrapped with the offending field) when SizeLength is not in 1..31,
// IndexLength or IndexDeltaLength is not in 0..32, SamplesPerFrame is not
// in 1..8192, or a single AU-header (SizeLength+IndexLength or
// SizeLength+IndexDeltaLength) would exceed 64 bits.
//
// SizeLength caps at 31 rather than 32 so a decoded AU-size always fits a
// non-negative platform int, including where int is 32 bits: a 32-bit size
// field can set the sign bit and yield a negative size, which the bounds
// checks in Depacketize are not built to catch. Real AAC-hbr uses 13.
func New(cfg Config) (*Depacketizer, error) {
	switch {
	case cfg.SizeLength < 1 || cfg.SizeLength > 31:
		return nil, fmt.Errorf("%w: SizeLength %d not in 1..31", ErrConfigInvalid, cfg.SizeLength)
	case cfg.IndexLength < 0 || cfg.IndexLength > 32:
		return nil, fmt.Errorf("%w: IndexLength %d not in 0..32", ErrConfigInvalid, cfg.IndexLength)
	case cfg.IndexDeltaLength < 0 || cfg.IndexDeltaLength > 32:
		return nil, fmt.Errorf("%w: IndexDeltaLength %d not in 0..32", ErrConfigInvalid, cfg.IndexDeltaLength)
	case cfg.SamplesPerFrame < 1 || cfg.SamplesPerFrame > 8192:
		return nil, fmt.Errorf("%w: SamplesPerFrame %d not in 1..8192", ErrConfigInvalid, cfg.SamplesPerFrame)
	case cfg.SizeLength+cfg.IndexLength > 64:
		return nil, fmt.Errorf("%w: SizeLength+IndexLength %d exceeds 64", ErrConfigInvalid, cfg.SizeLength+cfg.IndexLength)
	case cfg.SizeLength+cfg.IndexDeltaLength > 64:
		return nil, fmt.Errorf("%w: SizeLength+IndexDeltaLength %d exceeds 64", ErrConfigInvalid, cfg.SizeLength+cfg.IndexDeltaLength)
	}
	return &Depacketizer{cfg: cfg}, nil
}

// Reset discards any partial fragment reassembly state. The caller
// invokes it on an RTP sequence discontinuity (SeqGap > 0) so a lost
// final fragment cannot corrupt the next access unit.
func (d *Depacketizer) Reset() {
	d.fragActive = false
	d.fragTotalSize = 0
	d.frag = d.frag[:0]
}
