package rtp_test

// buildRTP assembles an RTP packet. Every field maps to a documented byte
// position (RFC 3550 section 5.1):
//
//	byte 0      V(2) P(1) X(1) CC(4)
//	byte 1      M(1) PT(7)
//	bytes 2-3   sequence number (big endian)
//	bytes 4-7   timestamp (big endian)
//	bytes 8-11  SSRC (big endian)
//	then CC*4   CSRC identifiers
//	then ext    if X: 4-byte ext header (profile, length-in-words) + data
//	then        payload
//	then pad    if P: padding bytes, last byte = total padding length
func buildRTP(marker bool, pt uint8, seq uint16, ts, ssrc uint32,
	csrc []uint32, ext []byte, payload []byte, pad int) []byte {
	// b0
	b0 := byte(2 << 6) // V=2
	if pad > 0 {
		b0 |= 1 << 5 // P
	}
	if len(ext) > 0 {
		b0 |= 1 << 4 // X
	}
	b0 |= byte(len(csrc) & 0x0f) // CC
	// b1
	b1 := pt & 0x7f
	if marker {
		b1 |= 0x80 // M
	}
	out := []byte{b0, b1, byte(seq >> 8), byte(seq)}
	out = appendU32(out, ts)
	out = appendU32(out, ssrc)
	for _, c := range csrc {
		out = appendU32(out, c)
	}
	if len(ext) > 0 {
		// ext must be a whole number of 32-bit words; caller guarantees.
		words := len(ext) / 4
		out = append(out, 0xBE, 0xDE, byte(words>>8), byte(words))
		out = append(out, ext...)
	}
	out = append(out, payload...)
	if pad > 0 {
		for i := 0; i < pad-1; i++ {
			out = append(out, 0xEE)
		}
		out = append(out, byte(pad))
	}
	return out
}

func appendU32(b []byte, v uint32) []byte {
	return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}
