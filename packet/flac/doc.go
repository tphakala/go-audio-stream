// Package flac packetizes FLAC (RFC 9639) frames for carriage over RTP.
//
// It is the send-side counterpart to depacket/flac. FLAC over RTP places raw
// FLAC frames directly after the RTP header with no payload-specific header; a
// frame larger than the path MTU is split across consecutive packets that share
// the frame's RTP timestamp, with the RTP marker bit set only on the last
// fragment. Packetize performs exactly that split, returning the ordered
// fragments and the marker bit each carries.
//
// It is a pure transform and holds no RTP session state: the caller owns the
// sequence number, SSRC, timestamp, and socket. The library is otherwise a
// receive/ingest library; this package exists so the de facto FLAC-over-RTP
// framing (there is no ratified IETF payload format for FLAC over RTP) is defined
// once and shared by both the sender and the depacket/flac receiver.
package flac
