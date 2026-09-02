package udpsource

import (
	"log/slog"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
)

// defaultReadBufSize is the receive buffer, sized to the largest UDP datagram so
// a single ReadFromUDP never truncates a packet.
const defaultReadBufSize = 65535

// defaultAACSamplesPerFrame is the AAC-LC frame length in samples, the default
// for AACParams.SamplesPerFrame. A raw RTP stream carries no fmtp, and 960 (the
// other common frame length) cannot be distinguished from 1024 by the RTP stream
// alone, so a 960-sample stream must set SamplesPerFrame explicitly.
const defaultAACSamplesPerFrame = 1024

// Mode selects how each received datagram is interpreted.
type Mode uint8

const (
	// ModeRTP treats each datagram as one RTP packet: it is parsed, checked for
	// sequence continuity (gaps, duplicates, SSRC changes, timestamp unwrap), and
	// depacketized according to the configured payload type. Out-of-order
	// datagrams are dropped, not reordered, unless Config.Reorder opts into the
	// resequencing path.
	ModeRTP Mode = iota
	// ModePCM treats each datagram as raw interleaved 16-bit PCM samples,
	// delivered as one frame.
	ModePCM
)

// String returns a short lowercase label for logs and diagnostics.
func (m Mode) String() string {
	switch m {
	case ModeRTP:
		return "rtp"
	case ModePCM:
		return "pcm"
	default:
		return "unknown"
	}
}

// PCMFormat describes the raw PCM carried by ModePCM datagrams. Samples are
// interpreted as interleaved signed 16-bit; a big-endian source is byte-swapped
// to little-endian s16le on delivery, matching the byte order every other
// source in this library delivers.
type PCMFormat struct {
	// SampleRate is the PCM sample rate in Hz. Required for ModePCM.
	SampleRate int
	// Channels is the interleaved channel count. Required for ModePCM.
	Channels int
	// BigEndian marks the datagrams as big-endian s16; the default is
	// little-endian, delivered verbatim.
	BigEndian bool
}

// AACParams holds the RFC 3640 MPEG4-GENERIC AU-header field widths (mode
// AAC-hbr) that Config supplies for CodecAAC over raw RTP, the raw-path analogue
// of the SDP fmtp an RTSP client would parse. The common AAC-hbr case is
// SizeLength 13, IndexLength 3, IndexDeltaLength 3. The fields map one-to-one
// onto depacket/aac.Config, so a caller that already has an SDP fmtp can copy
// them across directly.
type AACParams struct {
	// SizeLength is the AU-size field width in bits (AAC-hbr: 13). Required.
	SizeLength int
	// IndexLength is the AU-Index field width in bits for the first AU-header in
	// a packet (AAC-hbr: 3). Required.
	IndexLength int
	// IndexDeltaLength is the AU-Index-delta field width in bits for every
	// non-first AU-header in a packet (AAC-hbr: 3). Required.
	IndexDeltaLength int
	// SamplesPerFrame is the RTP timestamp increment per access unit, equal to
	// the samples one AAC frame decodes to (RFC 3640 clocks AAC at the audio
	// sample rate). Zero defaults to 1024 (AAC-LC); a 960-sample stream must set
	// it explicitly, since the two cannot be told apart from the RTP stream alone.
	SamplesPerFrame int
	// AudioSpecificConfig is the optional raw ASC reported through
	// Format().Codec (CodecAAC.AudioSpecificConfig) for a downstream decoder. It
	// is not needed to depacketize (the AU parser reads only the widths above), so
	// a caller without it may leave it nil. An AudioSpecificConfig set on the
	// CodecAAC value itself takes precedence; this is the fallback.
	AudioSpecificConfig []byte
}

// Config configures a raw UDP audio source. It carries no SDP, so for ModeRTP
// the caller supplies the payload type and its codec, clock rate, and channel
// count directly (the mapping RTSP would otherwise derive from SDP).
type Config struct {
	// ListenAddr is the UDP address to bind, as host:port. An empty host binds
	// all interfaces (for example ":5004"). Required.
	ListenAddr string

	// Mode selects RTP versus raw-PCM datagram interpretation.
	Mode Mode

	// PayloadType is the single RTP payload type this source accepts (ModeRTP).
	// A datagram whose payload type differs is counted malformed and dropped.
	PayloadType uint8
	// Codec identifies the RTP payload's codec (ModeRTP): CodecG711, CodecL16,
	// CodecG726, CodecOpus, CodecAAC, CodecFLAC, or CodecUnknown for an opaque
	// passthrough.
	//
	// A raw RTP source carries no rtpmap, so for CodecG726 the caller supplies
	// both BitRate and Packing. Packing's zero value is the plain RFC 3551
	// least-significant-bit-first order; set it to G726PackingAAL2 for a sender
	// using the AAL2-G726 order. Nothing on the wire distinguishes the two, and
	// decoding with the wrong one yields plausible but wrong audio. It selects
	// the depacketizer and, via PayloadKindFor, the delivered payload kind. For
	// CodecAAC the AU-header widths come from AAC below, since a raw RTP source
	// carries no SDP fmtp to derive them from.
	Codec audiostream.Codec
	// ClockRate is the RTP timestamp clock in Hz (ModeRTP), used for PTS, for a PCM
	// codec (G.711, L16) as the delivered sample rate, and, when RTCP is enabled, as
	// the TrackStats.SenderClock clock rate. Required for ModeRTP.
	ClockRate int
	// Channels is the channel count reported for a PCM codec (ModeRTP).
	Channels int

	// AAC carries the RFC 3640 AAC-hbr AU-header widths for CodecAAC over raw
	// RTP. It is consulted only when Codec is CodecAAC and Mode is ModeRTP, and
	// is ignored otherwise, so it is a zero-value-inert addition that leaves every
	// existing config unaffected. There is no SDP fmtp on the raw path, so the
	// caller supplies the widths directly (see AACParams).
	AAC AACParams

	// Reorder enables RTP resequencing for ModeRTP: late-but-in-window datagrams
	// are recovered and delivered in ascending sequence order through the shared
	// rtsp/rtp.Reorderer, instead of the default immediate-delivery path that
	// drops any backward-sequence datagram as a duplicate. It trades a small,
	// bounded resequencing latency and one heap copy per datagram for
	// out-of-order recovery, so it is opt-in: the default (false) keeps the
	// zero-copy, zero-alloc steady-state delivery path byte-for-byte unchanged.
	// It has no effect in ModePCM, whose datagrams carry no sequence numbers.
	Reorder bool

	// RTCPListenAddr, when non-empty, binds a second UDP socket (host:port) that
	// receives RTCP for this stream on a separate port, the classic RTP/RTCP
	// convention where a sender emits RTCP alongside the media on RTP-port + 1.
	// A received Sender Report populates TrackStats.SenderClock (the RTP-to-wall-
	// clock correspondence), which is otherwise invalid on the raw path since it
	// carries no SDP. RTCP is advisory: a malformed or off-source datagram is
	// ignored and never ends the session, and media delivery is unaffected. It is
	// consulted only in ModeRTP; empty (the default) binds no second socket and is
	// zero-value-inert, leaving the steady-state media path byte-for-byte
	// unchanged. Mutually exclusive with RTCPMux.
	RTCPListenAddr string

	// RTCPMux enables RFC 5761 RTP/RTCP multiplexing: RTCP is demultiplexed off
	// the single media socket (ListenAddr) by packet type, for senders that mux
	// both onto one port. Like RTCPListenAddr it populates TrackStats.SenderClock
	// from Sender Reports and is advisory. It is consulted only in ModeRTP and is
	// zero-value-inert when false (the default). Because RFC 5761 disambiguates by
	// the second byte, PayloadType must not fall in the reserved range 64-95 when
	// RTCPMux is set. Mutually exclusive with RTCPListenAddr.
	RTCPMux bool

	// Format describes the raw PCM datagrams (ModePCM). Required for ModePCM.
	Format PCMFormat

	// SourceIP, when non-empty, restricts accepted datagrams to this source IP,
	// mirroring the RTSP peer filter (IP only, not port). Empty accepts any
	// sender, which is the norm for a wildcard bind.
	SourceIP string

	// OnFrame receives every delivered frame on the reader goroutine. It must not
	// block and must not call Wait. Frame.Data is valid only for the call;
	// consumers that retain audio must copy. Nil is allowed: frames are still
	// counted in Stats, they are simply not delivered.
	OnFrame func(audiostream.Frame)

	// ReadIdle ends the source with audiostream.ErrReadTimeout when no datagram
	// arrives within the window. A value <= 0 disables the read-idle watchdog.
	ReadIdle time.Duration

	// Logger receives diagnostic logs; nil disables logging.
	Logger *slog.Logger

	// readBufSize is the receive buffer size; applyDefaults fills it.
	readBufSize int
}

// applyDefaults fills the zero-value fields that have a sensible default.
func (c *Config) applyDefaults() {
	if c.readBufSize <= 0 {
		c.readBufSize = defaultReadBufSize
	}
	// Default the AAC-LC frame length when the caller leaves it zero, so a common
	// AAC-hbr config need only set the three AU-header widths. Only an AAC config
	// is touched; every other codec is left exactly as given.
	if _, ok := c.Codec.(audiostream.CodecAAC); ok && c.AAC.SamplesPerFrame == 0 {
		c.AAC.SamplesPerFrame = defaultAACSamplesPerFrame
	}
}
