// Package g726 depacketizes ITU-T G.726 ADPCM audio (RFC 3551, 16/24/32/40
// kbps) and expands it to little-endian s16le PCM.
//
// Unlike G.711, G.726 is stateful: the adaptive predictor and quantizer step
// size adapt sample to sample and carry across RTP packet boundaries within a
// stream. A Decoder therefore holds that state and must be fed a stream's
// packets in order; one Decoder per stream. Reset it only when the stream
// restarts (an RTP SSRC change), never on a plain sequence gap, which
// re-converges on its own.
//
// The RTP payload is unpacked in RFC 3551 section 4.5.4 order (the first, oldest
// codeword in the least significant bits of each octet). That is the plain
// G726-NN form; the AAL2-G726 variant uses the opposite bit order and is not
// handled here. Output is s16le PCM at the track's clock rate (8000 Hz for the
// RFC 3551 defaults); the caller owns any audiostream.Frame assembly.
//
// The decoder follows ITU-T Recommendation G.726 and the Sun Microsystems
// CCITT g72x reference as a behavioral reference only; no third party code is
// copied, adapted, or translated. See THIRD_PARTY.md.
package g726
