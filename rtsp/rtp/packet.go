package rtp

import (
	"encoding/binary"
	"errors"
)

const (
	// HeaderSize is the RTP fixed header size, before any CSRC list or
	// header extension.
	HeaderSize = 12
	// MaxCSRC is the largest CSRC count the 4-bit CC field can express.
	MaxCSRC = 15
)

var (
	// ErrShortPacket is returned when the buffer is shorter than the
	// fixed header, CSRC list, or declared header extension.
	ErrShortPacket = errors.New("rtp: packet shorter than fixed header")
	// ErrVersion is returned when the RTP version is not 2.
	ErrVersion = errors.New("rtp: unsupported version")
	// ErrTruncatedExtension is returned when a header extension is
	// declared but the buffer does not contain it in full.
	ErrTruncatedExtension = errors.New("rtp: truncated header extension")
	// ErrBadPadding is returned when the padding length is zero or
	// exceeds the remaining payload.
	ErrBadPadding = errors.New("rtp: invalid padding length")
)

// Header is a parsed RTP fixed header (RFC 3550 section 5.1).
type Header struct {
	Version        uint8 // always 2 on success
	Padding        bool  // P bit (informational; ParsePacket removes padding)
	Extension      bool  // X bit
	Marker         bool  // M bit
	PayloadType    uint8 // 7-bit PT
	SequenceNumber uint16
	Timestamp      uint32   // raw 32-bit RTP timestamp
	SSRC           uint32   // synchronization source
	CSRC           []uint32 // CC entries, nil when CC == 0
}

// Packet is a parsed RTP packet: the header plus the payload with any
// header extension skipped and padding removed.
type Packet struct {
	Header Header
	// Payload aliases the input buffer; copy it to retain beyond the call.
	Payload []byte
}

// ParsePacket parses one RTP packet. It validates the version, reads the
// CSRC list, skips any header extension, and strips padding. It never
// panics and returns a typed error on any truncation or bad padding.
func ParsePacket(buf []byte) (Packet, error) {
	if len(buf) < HeaderSize {
		return Packet{}, ErrShortPacket
	}
	version := buf[0] >> 6
	if version != 2 {
		return Packet{}, ErrVersion
	}

	h := Header{
		Version:        version,
		Padding:        buf[0]&0x20 != 0,
		Extension:      buf[0]&0x10 != 0,
		Marker:         buf[1]&0x80 != 0,
		PayloadType:    buf[1] & 0x7f,
		SequenceNumber: binary.BigEndian.Uint16(buf[2:4]),
		Timestamp:      binary.BigEndian.Uint32(buf[4:8]),
		SSRC:           binary.BigEndian.Uint32(buf[8:12]),
	}

	cc := int(buf[0] & 0x0f)
	off := HeaderSize
	if cc > 0 {
		if len(buf) < off+cc*4 {
			return Packet{}, ErrShortPacket
		}
		h.CSRC = make([]uint32, cc)
		for i := range cc {
			h.CSRC[i] = binary.BigEndian.Uint32(buf[off : off+4])
			off += 4
		}
	}

	if h.Extension {
		if len(buf) < off+4 {
			return Packet{}, ErrTruncatedExtension
		}
		words := int(binary.BigEndian.Uint16(buf[off+2 : off+4]))
		extLen := 4 + words*4
		if len(buf) < off+extLen {
			return Packet{}, ErrTruncatedExtension
		}
		off += extLen
	}

	region := buf[off:]
	if h.Padding {
		if len(region) == 0 {
			return Packet{}, ErrBadPadding
		}
		padLen := int(buf[len(buf)-1])
		if padLen == 0 || padLen > len(region) {
			return Packet{}, ErrBadPadding
		}
		region = region[:len(region)-padLen]
	}

	return Packet{Header: h, Payload: region}, nil
}
