// Package flac reassembles FLAC (RFC 9639) frames carried over RTP.
//
// A FLAC-over-RTP stream places raw FLAC frames directly after the RTP header
// with no payload-specific header. A frame that fits in one packet is sent
// whole with the RTP marker bit set; a frame too large for the path MTU is split
// across consecutive packets that share the RTP timestamp, with the marker bit
// set only on the last fragment. This package concatenates the fragments of a
// frame and returns it once the marker arrives.
//
// It does not decode or validate the FLAC bitstream. Framing is delimited by the
// marker bit alone, so a lost fragment (an RTP sequence gap) can corrupt a
// reassembled frame; the caller drops that partial frame by calling Reset on a
// discontinuity, exactly as the AAC depacketizer's caller does. The reassembled
// frame is handed to a FLAC decoder such as one built on RFC 9639 unchanged; the
// STREAMINFO a decoder needs is carried out of band (in the SDP fmtp streaminfo=
// parameter) and surfaced through audiostream.CodecFLAC, not by this package.
//
// The out-of-band STREAMINFO and the per-fragment reassembly follow the de facto
// FLAC-over-RTP convention used by GStreamer-style senders; there is no ratified
// IETF payload format for FLAC over RTP.
package flac
