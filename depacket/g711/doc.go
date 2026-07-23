// Package g711 depacketizes G.711 audio (RFC 3551 PCMU/PCMA) and
// expands mu-law and A-law samples to s16le PCM via lookup tables.
//
// The tables are derived from the ITU-T G.711 reference expansion (full
// int16 range, matching common decoders). Output is s16le PCM; the
// caller owns any audiostream.Frame assembly.
package g711
