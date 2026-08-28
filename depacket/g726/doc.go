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
// Both RTP codeword packings are supported, selected per stream when the
// Decoder is built. audiostream.G726PackingRFC3551 is the plain G726-NN form of
// RFC 3551 section 4.5.4, which packs the first (oldest) codeword in the least
// significant bits of each octet; audiostream.G726PackingAAL2 is the
// AAL2-G726-NN form of ITU-T I.366.2 Annex E, which packs it in the most
// significant bits. The two carry the same codewords through the same
// ADPCM state machine and differ only in the unpacking, so a stream decoded
// with the wrong packing yields plausible but wrong audio; this package
// therefore never guesses it, and refuses an unrecognized value rather than
// defaulting. (One layer up, rtsp/sdp must assume the plain order for RFC 3551
// static payload type 2, which section 6 of that RFC marks reserved for exactly
// this ambiguity; every rtpmap-named form is unambiguous.) Output is s16le PCM
// at the track's clock rate (8000
// Hz for the RFC 3551 defaults); the caller owns any audiostream.Frame
// assembly.
//
// The decoder follows ITU-T Recommendation G.726 and the Sun Microsystems
// CCITT g72x reference as a behavioral reference only; no third party code is
// copied, adapted, or translated. See THIRD_PARTY.md.
package g726
