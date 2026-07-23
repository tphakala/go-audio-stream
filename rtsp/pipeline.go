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
	"github.com/tphakala/go-audio-stream/depacket/g711"
	"github.com/tphakala/go-audio-stream/depacket/opus"
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
// mutates the non-atomic fields (stream, aac, baseTS/baseSet/baselineFixed,
// g711Buf).
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
	// control is the resolved control URL, used to match RTP-Info entries to
	// this track. Set once by newTrack and never mutated afterward.
	control string
	// payloadType is the RTP payload type the SDP resolved this track from, or
	// payloadTypeUnknown when the m= line named none. The reader rejects a
	// packet whose PT differs, so a second format multiplexed onto the same
	// interleaved channel is not fed to this track's depacketizer.
	payloadType int

	// Reader-owned, non-atomic. Valid to touch only after publication.
	stream  rtp.Stream
	aac     *aac.Depacketizer
	baseTS  uint64
	baseSet bool
	// baselineFixed is set true the first time a baseline is established and is
	// never cleared, so the RTP-Info seed applies only to the very first
	// baseline. An SSRC reset clears baseSet (to re-baseline the new stream)
	// but leaves baselineFixed set, so the reset falls back to the first-packet
	// baseline rather than re-applying a stale seed.
	baselineFixed bool
	g711Buf       []byte

	// seed and hasSeed carry the RTP-Info timestamp origin. Play stores them
	// on the caller goroutine, potentially while the reader is already
	// delivering early frames and reading them in seededOrigin, so they are
	// atomic to publish the seed without a data race.
	seed    atomic.Uint64
	hasSeed atomic.Bool

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
		control:     desc.control,
		payloadType: desc.payloadType,
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

// acceptsPayloadType reports whether pt is the payload type this track carries.
// A track the SDP resolved no format for accepts everything: refusing every
// packet would be worse than delivering an unknown payload raw, which is
// already what such a track does.
func (tr *track) acceptsPayloadType(pt uint8) bool {
	if tr.payloadType == payloadTypeUnknown {
		return true
	}
	return int(pt) == tr.payloadType
}

// deliver depacketizes one RTP packet for its track and hands each resulting
// frame to onFrame. It selects the per-codec path from tr.kind. A
// depacketization error increments tr.malformed and delivers nothing; the
// session continues. onFrame may be nil, in which case nothing is delivered but
// counters still advance. The delivered Frame is a stack value whose Data
// aliases reader-owned memory (the AAC depacketizer buffer, the RTP payload, or
// tr.g711Buf); it is valid only for the duration of the callback.
func (tr *track) deliver(pkt rtp.Packet, up rtp.Update, now time.Time, onFrame func(audiostream.Frame)) {
	switch tr.kind {
	case deliverAAC:
		tr.deliverAAC(pkt, up, now, onFrame)
	case deliverOpus:
		tr.deliverOpus(pkt, up, now, onFrame)
	case deliverG711:
		tr.deliverG711(pkt, up, now, onFrame)
	default: // deliverRaw, and any kind a future codec adds without a path here.
		tr.deliverRaw(pkt, up, now, onFrame)
	}
}

// deliverAAC runs the RFC 3640 AAC-hbr depacketizer and delivers one frame per
// access unit. SeqGap is reported on the first AU of the packet and zero on the
// rest, so summing SeqGap across frames counts each loss once; RTPTime is the
// packet timestamp for every AU; PTS interpolates by the AU's RTP offset. A
// malformed packet counts as malformed and yields no frame.
func (tr *track) deliverAAC(pkt rtp.Packet, up rtp.Update, now time.Time, onFrame func(audiostream.Frame)) {
	aus, err := tr.aac.Depacketize(pkt.Payload, pkt.Header.Marker, pkt.Header.Timestamp)
	if err != nil {
		tr.malformed.Add(1)
		return
	}
	if onFrame == nil {
		return
	}
	for i := range aus {
		gap := 0
		if i == 0 {
			gap = up.Gap
		}
		onFrame(audiostream.Frame{
			TrackID:    tr.id,
			Data:       aus[i].Data,
			RTPTime:    pkt.Header.Timestamp,
			PTS:        tr.ptsOf(up.Timestamp + uint64(aus[i].RTPOffset)),
			ReceivedAt: now,
			SeqGap:     gap,
		})
	}
}

// deliverOpus passes the RFC 7587 payload through unchanged, one frame per
// packet. An empty payload counts as malformed and yields no frame.
func (tr *track) deliverOpus(pkt rtp.Packet, up rtp.Update, now time.Time, onFrame func(audiostream.Frame)) {
	data, err := opus.Depacketize(pkt.Payload)
	if err != nil {
		tr.malformed.Add(1)
		return
	}
	tr.deliverOne(data, pkt, up, now, onFrame)
}

// deliverG711 expands companded G.711 into s16le PCM in the reused tr.g711Buf,
// growing it only when a larger packet arrives, and delivers one frame.
func (tr *track) deliverG711(pkt rtp.Packet, up rtp.Update, now time.Time, onFrame func(audiostream.Frame)) {
	need := 2 * len(pkt.Payload)
	if cap(tr.g711Buf) < need {
		tr.g711Buf = make([]byte, need)
	} else {
		tr.g711Buf = tr.g711Buf[:need]
	}
	n, err := g711.Depacketize(tr.g711Buf, pkt.Payload, tr.law)
	if err != nil {
		tr.malformed.Add(1)
		return
	}
	tr.deliverOne(tr.g711Buf[:n], pkt, up, now, onFrame)
}

// deliverRaw delivers the undecoded RTP payload as one frame, for unknown
// codecs, video, and AAC modes this milestone does not decode.
func (tr *track) deliverRaw(pkt rtp.Packet, up rtp.Update, now time.Time, onFrame func(audiostream.Frame)) {
	tr.deliverOne(pkt.Payload, pkt, up, now, onFrame)
}

// deliverOne delivers a single-frame codec's payload, shared by Opus, G.711,
// and raw. onFrame may be nil.
func (tr *track) deliverOne(data []byte, pkt rtp.Packet, up rtp.Update, now time.Time, onFrame func(audiostream.Frame)) {
	if onFrame == nil {
		return
	}
	onFrame(audiostream.Frame{
		TrackID:    tr.id,
		Data:       data,
		RTPTime:    pkt.Header.Timestamp,
		PTS:        tr.ptsOf(up.Timestamp),
		ReceivedAt: now,
		SeqGap:     up.Gap,
	})
}

// ptsOf computes the presentation time of an unwrapped 64-bit RTP timestamp
// relative to tr.baseTS and the track clock rate. It splits the division to
// avoid overflow (delta*1e9 overflows a uint64 after about 585 seconds of
// stream) and returns 0 when the clock rate is unknown.
//
// A timestamp below the baseline (a reordered packet, or an AU offset applied
// to one) would wrap the unsigned subtraction into an enormous PTS, so it
// clamps to zero instead.
func (tr *track) ptsOf(ts uint64) time.Duration {
	rate := tr.clockRate
	if rate == 0 || ts < tr.baseTS {
		return 0
	}
	delta := ts - tr.baseTS
	sec := delta / rate
	frac := (delta % rate) * uint64(time.Second) / rate
	return time.Duration(sec)*time.Second + time.Duration(frac)
}

// resetDepacketizer clears any codec reassembly state on an SSRC change or a
// sequence gap, so a lost fragment cannot corrupt the next access unit. Only
// AAC carries cross-packet state; the other codecs are stateless.
func (tr *track) resetDepacketizer() {
	if tr.aac != nil {
		tr.aac.Reset()
	}
}

// publishRRSnapshot copies the reader-owned rtp.Stream counters into the atomic
// RR-snapshot fields the keepalive timer reads when building Receiver Reports,
// so the timer never touches the reader-owned Stream.
func (tr *track) publishRRSnapshot() {
	st := tr.stream.Stats()
	tr.rrHighestSeq.Store(st.ExtendedHighestSeq)
	tr.rrCumulativeLost.Store(cumulativeLost(st.SeqGaps))
}

// cumulativeLost narrows the 64-bit loss count to the 24 bits the RFC 3550
// report block carries. It saturates rather than truncating: a stream that has
// lost more than 16.7 million packets should report "very lossy", not wrap
// around to a small number and claim it recovered.
func cumulativeLost(seqGaps uint64) uint32 {
	const maxCumulativeLost = 0x00FFFFFF
	if seqGaps > maxCumulativeLost {
		return maxCumulativeLost
	}
	return uint32(seqGaps)
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
