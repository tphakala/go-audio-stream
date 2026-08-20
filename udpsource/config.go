package udpsource

import (
	"log/slog"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
)

// defaultReadBufSize is the receive buffer, sized to the largest UDP datagram so
// a single ReadFromUDP never truncates a packet.
const defaultReadBufSize = 65535

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
	// CodecOpus, or CodecUnknown for an opaque passthrough. It selects the
	// depacketizer and, via PayloadKindFor, the delivered payload kind.
	Codec audiostream.Codec
	// ClockRate is the RTP timestamp clock in Hz (ModeRTP), used for PTS and, for
	// a PCM codec (G.711, L16), as the delivered sample rate. Required for ModeRTP.
	ClockRate int
	// Channels is the channel count reported for a PCM codec (ModeRTP).
	Channels int

	// Reorder enables RTP resequencing for ModeRTP: late-but-in-window datagrams
	// are recovered and delivered in ascending sequence order through the shared
	// rtsp/rtp.Reorderer, instead of the default immediate-delivery path that
	// drops any backward-sequence datagram as a duplicate. It trades a small,
	// bounded resequencing latency and one heap copy per datagram for
	// out-of-order recovery, so it is opt-in: the default (false) keeps the
	// zero-copy, zero-alloc steady-state delivery path byte-for-byte unchanged.
	// It has no effect in ModePCM, whose datagrams carry no sequence numbers.
	Reorder bool

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
}
