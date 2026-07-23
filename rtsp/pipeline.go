package rtsp

import (
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/depacket/aac"
	"github.com/tphakala/go-audio-stream/rtsp/rtp"
	"github.com/tphakala/go-audio-stream/rtsp/sdp"
)

// aacHbrMode is the only RFC 3640 mode this milestone depacketizes; any other
// mode (for example MP4A-LATM) degrades to raw payload delivery.
const aacHbrMode = "AAC-hbr"

// aacSamplesPerFrame is the fixed AAC frame length assumed for intra-packet
// PTS interpolation. Decision 7 defers 960-sample detection from the
// AudioSpecificConfig, so 1024 (AAC-LC) is used for every AAC-hbr track.
const aacSamplesPerFrame = 1024

// codecKind selects a track's per-codec delivery path in the reader.
type codecKind uint8

const (
	// deliverRaw hands the undecoded RTP payload to OnFrame. It is the fallback
	// for unknown codecs, video, and AAC modes this milestone does not decode.
	deliverRaw codecKind = iota
	// deliverAAC runs the RFC 3640 AAC-hbr depacketizer.
	deliverAAC
	// deliverOpus passes the RFC 7587 payload through unchanged.
	deliverOpus
	// deliverG711 expands companded G.711 to s16le PCM.
	deliverG711
)

// track is one set-up track's pipeline state. Setup fully initializes it and
// publishes it into the channel table by an atomic store; that store
// establishes the happens-before edge, after which only the reader goroutine
// mutates the non-atomic fields (stream, aac, baseTS/baseSet, seed, g711Buf).
// The atomic stat and RR-snapshot fields are written by the reader and read by
// Stats and the keepalive timer. A track is always referenced by pointer and
// never copied.
type track struct {
	id          int
	kind        codecKind
	clockRate   uint64
	law         audiostream.Law
	discard     bool
	rtpChannel  int
	rtcpChannel int

	// Reader-owned, non-atomic. Valid to touch only after publication.
	stream  rtp.Stream
	aac     *aac.Depacketizer
	baseTS  uint64
	baseSet bool
	seed    uint64
	hasSeed bool
	g711Buf []byte

	// Per-track receive counters: reader writes, Stats reads.
	packets    atomic.Uint64
	bytes      atomic.Uint64
	seqGaps    atomic.Uint64
	malformed  atomic.Uint64
	ssrcResets atomic.Uint64

	// RTCP Receiver Report snapshot: reader writes, keepalive timer reads.
	rrHighestSeq     atomic.Uint32
	rrCumulativeLost atomic.Uint32
	senderSSRC       atomic.Uint32
	lastSR           atomic.Uint32
	lastSRUnixNano   atomic.Int64
}

// newTrack builds a track's depacketization pipeline from its resolved
// descriptor and the negotiated interleaved channels. It selects the
// depacketizer by codec: an AAC-hbr fmtp yields an aac.Depacketizer; a
// non-AAC-hbr mode, an unrecognized codec, video, or an invalid fmtp falls
// back to raw payload delivery with a logged warning. It never fails, so a
// quirky fmtp degrades one track to raw rather than failing Setup.
func newTrack(id int, desc describedTrack, opts SetupOptions, rtpCh, rtcpCh int, logger *slog.Logger) *track {
	tr := &track{
		id:          id,
		kind:        deliverRaw,
		clockRate:   clockRateTicks(desc.clockRate),
		discard:     opts.Discard,
		rtpChannel:  rtpCh,
		rtcpChannel: rtcpCh,
	}
	switch codec := desc.codec.(type) {
	case audiostream.CodecAAC:
		tr.configureAAC(desc.aac, logger)
	case audiostream.CodecOpus:
		tr.kind = deliverOpus
	case audiostream.CodecG711:
		tr.kind = deliverG711
		tr.law = codec.Law
	default:
		tr.kind = deliverRaw
	}
	return tr
}

// clockRateTicks converts an SDP clock rate to the uint64 tick rate used for
// PTS math, clamping a negative or missing rate to zero (ptsOf treats zero as
// "unknown" and reports a zero PTS).
func clockRateTicks(rate int) uint64 {
	if rate <= 0 {
		return 0
	}
	return uint64(rate)
}

// configureAAC selects the AAC-hbr depacketizer for a CodecAAC track, or falls
// back to raw delivery when the mode is not AAC-hbr or the fmtp field widths
// are invalid.
func (tr *track) configureAAC(params *sdp.AACParams, logger *slog.Logger) {
	if params == nil || !strings.EqualFold(params.Mode, aacHbrMode) {
		tr.kind = deliverRaw
		logWarn(logger, "aac track is not AAC-hbr; delivering raw payloads", "mode", modeOf(params))
		return
	}
	dp, err := aac.New(configFromAAC(params))
	if err != nil {
		tr.kind = deliverRaw
		logWarn(logger, "aac fmtp invalid; delivering raw payloads", "error", err.Error())
		return
	}
	tr.kind = deliverAAC
	tr.aac = dp
}

// configFromAAC maps SDP MPEG4-GENERIC fmtp parameters to an aac.Config with
// SamplesPerFrame fixed at 1024 (decision 7). aac.New validates the field
// widths.
func configFromAAC(p *sdp.AACParams) aac.Config {
	return aac.Config{
		SizeLength:       p.SizeLength,
		IndexLength:      p.IndexLength,
		IndexDeltaLength: p.IndexDeltaLength,
		SamplesPerFrame:  aacSamplesPerFrame,
	}
}

// modeOf returns the fmtp mode for logging, or "" when no params were parsed.
func modeOf(params *sdp.AACParams) string {
	if params == nil {
		return ""
	}
	return params.Mode
}

// deliver depacketizes one RTP packet for its track and hands each resulting
// frame to onFrame. The body is implemented in a later task; Task 3 wires the
// pipeline and channel table so only the per-codec delivery logic remains.
func (tr *track) deliver(pkt rtp.Packet, up rtp.Update, now time.Time, onFrame func(audiostream.Frame)) {
	// Delivery is implemented in a later task.
}

// channelBinding routes one interleaved channel to a track and records whether
// the channel carries RTCP rather than RTP.
type channelBinding struct {
	track  *track
	isRTCP bool
}

// channelTable is an immutable channel-to-track routing table. Setup publishes
// a new table by copy-on-write and an atomic store; the reader loads it
// lock-free on every interleaved frame and never mutates it.
type channelTable struct {
	bindings map[int]channelBinding
}

// lookup returns the binding for an interleaved channel and whether it is
// bound. A nil table (before the first Setup) reports no binding.
func (t *channelTable) lookup(channel int) (channelBinding, bool) {
	if t == nil {
		return channelBinding{}, false
	}
	b, ok := t.bindings[channel]
	return b, ok
}

// newChannelTable returns a new immutable table copying old's bindings and
// adding tr's RTP and RTCP channels. old may be nil for the first track.
func newChannelTable(old *channelTable, tr *track, rtpCh, rtcpCh int) *channelTable {
	size := 2
	if old != nil {
		size += len(old.bindings)
	}
	bindings := make(map[int]channelBinding, size)
	if old != nil {
		for ch, b := range old.bindings {
			bindings[ch] = b
		}
	}
	bindings[rtpCh] = channelBinding{track: tr, isRTCP: false}
	bindings[rtcpCh] = channelBinding{track: tr, isRTCP: true}
	return &channelTable{bindings: bindings}
}

// logWarn logs at warn level when a logger is configured. Credentials are
// never passed to it; only codec and channel diagnostics.
func logWarn(logger *slog.Logger, msg string, args ...any) {
	if logger != nil {
		logger.Warn(msg, args...)
	}
}
