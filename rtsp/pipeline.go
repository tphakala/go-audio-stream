package rtsp

import (
	"log/slog"
	"maps"
	"math"
	"strings"
	"sync/atomic"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/depacket/aac"
	"github.com/tphakala/go-audio-stream/rtsp/rtp"
	"github.com/tphakala/go-audio-stream/rtsp/sdp"
)

// aacHbrMode is the only RFC 3640 mode this milestone depacketizes. The other
// modes that RFC defines (generic, CELP-cbr, CELP-vbr, AAC-lbr) degrade to raw
// payload delivery. Note this is the fmtp "mode=" value under an
// mpeg4-generic rtpmap, not an encoding name: a track carrying a different
// encoding name, MP4A-LATM among them, never reaches this comparison, because
// the SDP layer maps only MPEG4-GENERIC to CodecAAC and everything else lands
// on newTrack's unrecognized-codec path.
const aacHbrMode = "AAC-hbr"

// aacSamplesPerFrame is the AAC frame length assumed for intra-packet PTS
// interpolation. The format permits 960 as well, and the depacketizer accepts
// any length, so this is a policy this package chose rather than a limit the
// format imposes: detecting 960 requires decoding the AudioSpecificConfig,
// which this milestone does not do, and 1024 is the AAC-LC value that cameras
// emit. A 960-sample stream therefore drifts in PTS until that detection lands.
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
// A track is always referenced by pointer and never copied.
//
// The atomic fields below are declared for the readers and writers that arrive
// with frame delivery; nothing reads or writes them yet. They are atomic
// because that is the access discipline those callers will need, not because
// anything races over them today.
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

	// Per-track receive counters. The reader will write these and Stats will
	// read them once delivery lands; both are unwired today.
	packets    atomic.Uint64
	bytes      atomic.Uint64
	seqGaps    atomic.Uint64
	malformed  atomic.Uint64
	ssrcResets atomic.Uint64

	// RTCP Receiver Report snapshot. The reader will write these and the
	// keepalive timer will read them; both arrive with delivery.
	rrHighestSeq     atomic.Uint32
	rrCumulativeLost atomic.Uint32
	senderSSRC       atomic.Uint32
	lastSR           atomic.Uint32
	lastSRUnixNano   atomic.Int64
}

// newTrack builds a track's depacketization pipeline from its resolved
// descriptor and the negotiated interleaved channels.
//
// It dispatches on the codec, but only after confirming the track is audio.
// The SDP layer resolves the codec from the a=rtpmap encoding name without
// regard to the m= kind, so a video section advertising MPEG4-GENERIC/90000
// resolves to CodecAAC; without the media check its payloads would be fed to
// the AAC access-unit parser and emitted as audio frames with a PTS derived
// from a 90 kHz video clock.
//
// A non-audio track, an unrecognized codec, a non-AAC-hbr mode, or an invalid
// fmtp falls back to raw payload delivery with a logged warning. It never
// fails, so a quirky fmtp degrades one track to raw rather than failing Setup.
func newTrack(id int, desc describedTrack, opts SetupOptions, rtpCh, rtcpCh int, logger *slog.Logger) *track {
	tr := &track{
		id:          id,
		kind:        deliverRaw,
		clockRate:   clockRateTicks(desc.clockRate),
		discard:     opts.Discard,
		rtpChannel:  rtpCh,
		rtcpChannel: rtcpCh,
	}
	if desc.media != audiostream.MediaAudio {
		logWarn(logger, "track is not audio; delivering raw payloads",
			"track", id, "media", desc.media.String())
		return tr
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
		logWarn(logger, "unrecognized codec; delivering raw payloads", "track", id)
	}
	return tr
}

// clockRateTicks converts an SDP clock rate to the uint64 tick rate used for
// PTS math. Zero is the "rate unknown" sentinel: PTS interpolation is skipped
// for such a track rather than dividing by it, so a missing or nonsensical
// a=rtpmap rate costs timestamps, not a panic.
//
// Both bounds are enforced here because this is where the value crosses from
// the SDP into arithmetic. The rate is remote input parsed with a plain Atoi,
// so it can arrive negative or far beyond anything meaningful; RTP timestamps
// are 32 bits by definition, so a rate above MaxUint32 cannot describe a real
// stream and would silently wrap the first time it is narrowed.
func clockRateTicks(rate int) uint64 {
	if rate <= 0 || int64(rate) > math.MaxUint32 {
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
		logWarn(logger, "aac fmtp invalid; delivering raw payloads", "error", err)
		return
	}
	tr.kind = deliverAAC
	tr.aac = dp
}

// configFromAAC maps SDP MPEG4-GENERIC fmtp parameters to an aac.Config.
// SamplesPerFrame is fixed at aacSamplesPerFrame; see that constant for why
// this package does not detect the 960-sample case. aac.New validates the
// field widths.
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
// frame to onFrame. The pipeline and the channel table are in place, so only
// the per-codec delivery logic remains; it lands with the reader's frame
// routing.
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
// a new table by copy-on-write and an atomic store. It is immutable so that the
// reader can load it lock-free on every interleaved frame once it routes them;
// nothing reads it yet, and the reader will never mutate it.
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
		maps.Copy(bindings, old.bindings)
	}
	bindings[rtpCh] = channelBinding{track: tr, isRTCP: false}
	bindings[rtcpCh] = channelBinding{track: tr, isRTCP: true}
	return &channelTable{bindings: bindings}
}

// logWarn logs at warn level when a logger is configured. No call site passes
// a credential, and none passes a URL; a caller that needs to log one must put
// it through RedactURL first, and note that RedactURL masks userinfo but not
// query parameters.
func logWarn(logger *slog.Logger, msg string, args ...any) {
	if logger != nil {
		logger.Warn(msg, args...)
	}
}
