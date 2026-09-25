package rtp

import (
	"encoding/binary"
	"errors"
)

var (
	// ErrMarshalExtension is returned by AppendTo when the Extension bit is
	// set: the send side does not build header extensions.
	ErrMarshalExtension = errors.New("rtp: header extension not supported on marshal")
	// ErrTooManyCSRC is returned by AppendTo when len(CSRC) exceeds MaxCSRC,
	// the largest count the 4-bit CC field can express.
	ErrTooManyCSRC = errors.New("rtp: CSRC count exceeds 15")
)

// AppendTo appends the 12-byte fixed header (plus any CSRC entries) to dst and
// returns the extended slice. It is the inverse of ParsePacket's header parse.
// The version is forced to 2; the payload type is masked to 7 bits. The send
// side does not support the extension bit (ErrMarshalExtension) and never emits
// padding, so the P and X bits are always zero.
func (h Header) AppendTo(dst []byte) ([]byte, error) {
	if h.Extension {
		return dst, ErrMarshalExtension
	}
	cc := len(h.CSRC)
	if cc > MaxCSRC {
		return dst, ErrTooManyCSRC
	}

	var fixed [HeaderSize]byte
	fixed[0] = 0x80 | byte(cc) // V=2, P=0, X=0, CC
	fixed[1] = h.PayloadType & 0x7f
	if h.Marker {
		fixed[1] |= 0x80
	}
	binary.BigEndian.PutUint16(fixed[2:4], h.SequenceNumber)
	binary.BigEndian.PutUint32(fixed[4:8], h.Timestamp)
	binary.BigEndian.PutUint32(fixed[8:12], h.SSRC)
	dst = append(dst, fixed[:]...)

	for _, c := range h.CSRC {
		var w [4]byte
		binary.BigEndian.PutUint32(w[:], c)
		dst = append(dst, w[:]...)
	}
	return dst, nil
}

// AppendPacket appends a full RTP packet (fixed header, any CSRCs, then the
// payload) to dst and returns the extended slice. With a dst that already has
// capacity it performs no allocation.
func AppendPacket(dst []byte, h Header, payload []byte) ([]byte, error) {
	dst, err := h.AppendTo(dst)
	if err != nil {
		return dst, err
	}
	return append(dst, payload...), nil
}
