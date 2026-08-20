package rtp

import "time"

// NTPUnixOffset is the seconds from the NTP epoch (1900-01-01) to the Unix
// epoch (1970-01-01).
const NTPUnixOffset = 2208988800

// NTPTime decodes a 64-bit NTP timestamp (RFC 5905: upper 32 bits whole
// seconds since 1900, lower 32 bits binary fraction) into a time.Time. A
// seconds field with the high bit clear is read as NTP era 1 (2036 to 2104),
// the RFC 5905 pivot, so the decode is correct for any sender clock set
// between 1968 and 2104. The caller filters the all-zero timestamp first.
func NTPTime(ts uint64) time.Time {
	sec := int64(ts >> 32)
	if sec < 0x80000000 {
		sec += 1 << 32
	}
	nsec := ((ts & 0xFFFFFFFF) * uint64(time.Second)) >> 32
	return time.Unix(sec-NTPUnixOffset, int64(nsec))
}
