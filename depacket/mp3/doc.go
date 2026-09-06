// Package mp3 depacketizes MPEG audio (MP3 / MPA) carried over RTP per RFC 2250
// section 3.5.
//
// Every RTP payload begins with a mandatory 4-byte MPEG audio-specific header: a
// 16-bit MBZ field followed by a 16-bit fragmentation offset (big-endian). The
// audio bytes follow. This package strips that header and returns whole MPEG
// audio frames (Layer I/II/III), delimited by the frame headers it parses; it
// never decodes.
//
// The fragmentation offset is the byte offset of this packet's audio data within
// the current MPEG audio frame. An offset of 0 marks a frame boundary: the
// packet then carries one or more whole frames (aggregation, RFC 2250 requires
// an integral number of frames) or the first fragment of a frame too large for
// the path MTU. A non-zero offset continues a frame already in progress, and all
// fragments of one frame share the RTP timestamp. There is no end-of-frame flag,
// so the reassembler learns each frame's length from the MPEG audio header at its
// start (see internal/mp3) and completes the frame when the accumulated bytes
// reach it. The RTP marker bit is not used to delimit MPEG audio frames, and the
// MBZ field is ignored, matching FFmpeg and live555.
//
// The RTP clock for MPA is 90 kHz regardless of the audio sampling rate (RFC
// 2250, RFC 3551 section 4.5.13), so the per-frame RTPOffset a packet's later
// aggregated frames carry is scaled from PCM samples to that clock. New takes the
// clock rate the SDP resolved so a non-standard sender that declares another rate
// is still timed on its own clock.
//
// A returned frame aliases either the input payload (a whole frame carried in one
// packet, allocation-free) or the reassembly buffer (a frame reassembled from
// fragments); it is valid only until the next call to Depacketize or Reset. A
// lost fragment (an RTP sequence gap) can corrupt a reassembled frame, so the
// caller drops the partial by calling Reset on a discontinuity, exactly as the
// AAC and FLAC depacketizers' callers do.
//
// There is no ratified IETF successor to RFC 2250 for MPEG audio; the payload
// format this package implements is the one senders such as FFmpeg and live555
// emit for the static payload type 14 (MPA).
package mp3
