package rtp

import (
	"encoding/binary"
	"time"
)

// Marshal encodes the Sender Report as a single RTCP packet with zero report
// blocks (28 bytes): V=2, P=0, RC=0, PT=200, length word 6. It is the inverse
// of the SR case in ParseCompound. A lone SR (no compound RR/SDES) is what a
// mic-side sender emits for clock sync, mirroring how ReceiverReport.Marshal
// already emits a standalone RR for the client keepalive.
func (sr SenderReport) Marshal() []byte {
	buf := make([]byte, 0, senderInfoSize)
	buf = append(buf, byte(2<<6), PTSenderReport) // V=2, P=0, RC=0
	buf = binary.BigEndian.AppendUint16(buf, uint16(senderInfoSize/4-1))
	buf = binary.BigEndian.AppendUint32(buf, sr.SSRC)
	buf = binary.BigEndian.AppendUint32(buf, uint32(sr.NTPTimestamp>>32))
	buf = binary.BigEndian.AppendUint32(buf, uint32(sr.NTPTimestamp))
	buf = binary.BigEndian.AppendUint32(buf, sr.RTPTimestamp)
	buf = binary.BigEndian.AppendUint32(buf, sr.PacketCount)
	buf = binary.BigEndian.AppendUint32(buf, sr.OctetCount)
	return buf
}

// NTPFromTime is the inverse of NTPTime: it encodes a wall-clock time as a
// 64-bit NTP timestamp (RFC 5905). The seconds are taken modulo 2^32, which
// NTPTime's era-1 pivot decodes back correctly for any time from 1968 to 2104,
// so no epoch is out of range. The all-zero timestamp is reserved by RFC 3550
// section 6.4.1 for "no wall clock"; a real time never encodes to it.
func NTPFromTime(t time.Time) uint64 {
	sec := uint64(t.Unix()+ntpUnixOffset) & 0xFFFFFFFF
	frac := (uint64(t.Nanosecond()) << 32) / uint64(time.Second)
	return sec<<32 | frac
}
