// Package opus packetizes an Opus packet for RTP. Under RFC 7587 the RTP
// payload IS the Opus packet, so packetizing is validation plus passthrough,
// the inverse of depacket/opus.Depacketize.
package opus

import "errors"

// maxPacketBytes is a defensive upper bound on an Opus packet in one RTP
// payload: three 1275-byte frames plus a two-byte TOC/frame-count prefix
// (RFC 6716). Real 20 ms frames are far smaller; this only rejects absurd
// input.
const maxPacketBytes = 1275*3 + 2

var (
	// ErrEmptyPacket is returned for a zero-length packet: RFC 7587 puts no
	// empty or DTX packets on the wire.
	ErrEmptyPacket = errors.New("opus: empty packet")
	// ErrOversizePacket is returned for a packet larger than maxPacketBytes.
	ErrOversizePacket = errors.New("opus: packet exceeds maximum size")
)

// Packetize validates one Opus packet for use as an RTP payload and returns it
// unchanged. It does not inspect the TOC byte or frame structure; the encoder
// owns that.
func Packetize(pkt []byte) ([]byte, error) {
	if len(pkt) == 0 {
		return nil, ErrEmptyPacket
	}
	if len(pkt) > maxPacketBytes {
		return nil, ErrOversizePacket
	}
	return pkt, nil
}
