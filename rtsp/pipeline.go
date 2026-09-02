package rtsp

import (
	"bytes"
	"log/slog"
	"maps"
	"math"
	"strings"
	"sync/atomic"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/depacket/aac"
	"github.com/tphakala/go-audio-stream/depacket/flac"
	"github.com/tphakala/go-audio-stream/depacket/g711"
	"github.com/tphakala/go-audio-stream/depacket/g726"
	"github.com/tphakala/go-audio-stream/depacket/latm"
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
	// deliverL16 byte-swaps big-endian L16 linear PCM to s16le.
	deliverL16
	// deliverLATM runs the RFC 3016 MP4A-LATM depacketizer.
	deliverLATM
	// deliverG726 runs the ITU-T G.726 ADPCM decoder, expanding to s16le PCM.
	deliverG726
	// deliverFLAC reassembles FLAC frames across RTP packets and delivers each
	// completed frame as one compressed frame.
	deliverFLAC
)

// track is one set-up track's pipeline state. Setup fully initializes it and
// publishes it into the channel table by an atomic store; that store
// establishes the happens-before edge, after which only the reader goroutine
// mutates the non-atomic fields (stream, aac, baseTS/baselineFixed, pcmBuf).
// baseSet is an atomic.Bool because, in UDP transport mode, the RTP receive
// goroutine writes it (via process) while the RTCP receive goroutine reads it
// (via handleRTCP) to gate the sender-clock publish; in TCP mode the single
// reader goroutine both writes and reads it, so the promotion is behavior-
// identical there.
// The atomic stat and RR-snapshot fields are written by the reader and read by
// Stats and the keepalive timer. A track is always referenced by pointer and
// never copied.
type track struct {
	id          int
	kind        codecKind
	clockRate   uint64
	law         audiostream.Law
	discard     bool
	rtcpChannel int
	// control is the resolved control URL, used to match RTP-Info entries to
	// this track. Set once by newTrack and never mutated afterward.
	control string
	// sdpPayloadType is the RTP payload type the SDP resolved this track from,
	// or payloadTypeUnknown when the m= line named none. It is the EXPECTED
	// payload type, not the enforced one: see acceptPayloadType.
	sdpPayloadType int

	// Reader-owned, non-atomic. Valid to touch only after publication.
	stream rtp.Stream
	aac    *aac.Depacketizer
	// flac reassembles FLAC frames across RTP packets for a CodecFLAC track, nil
	// otherwise. Like aac it carries cross-packet fragment state, so it is reset
	// on BOTH a sequence gap and an SSRC change (see resetDepacketizer).
	flac *flac.Depacketizer
	latm *latm.Depacketizer
	// g726 is the ITU-T G.726 ADPCM decoder for a CodecG726 track, nil
	// otherwise. It carries adaptive state across packets, so it is reset only
	// on an SSRC change (a new source), never on a plain sequence gap.
	g726 *g726.Decoder
	// latmASC is the last AudioSpecificConfig deliverLATM reported through
	// Config.OnCodecUpdate (or, for an out-of-band track, the ASC newTrack
	// seeded from Describe to suppress a redundant first callback). It is
	// reader-owned: only deliverLATM and resetDepacketizer touch it, both on
	// the delivery goroutine, so it needs no lock. resetDepacketizer
	// re-seeds it from whatever survives an SSRC reset, so an in-band track
	// re-announces its next resolved config while an out-of-band track
	// (whose ASC survives the reset unchanged) never spuriously refires.
	latmASC []byte
	// pendingGap is the count of lost RTP packets not yet reported through a
	// Frame.SeqGap. Every delivery path folds up.Gap into it before
	// depacketizing and drains it onto the next delivered frame (the FIRST
	// access unit a packet completes, for AAC/LATM; the one frame, for the
	// single-frame codecs). So a loss immediately before a packet that delivers
	// no frame (a buffering AAC fragment, or a malformed packet in any codec)
	// still surfaces on the next delivered frame instead of vanishing from the
	// per-frame signal. TrackStats.SeqGaps was already correct; only
	// Frame.SeqGap was wrong. Reader-owned: only the deliver* methods and
	// resetDepacketizer touch it, all on the delivery goroutine, so it needs no
	// lock. resetDepacketizer clears it only on an SSRC reset, not a plain gap,
	// so a gap from the old sequence space cannot bleed onto the new source's
	// first frame. Shared by every codec path: a track is exactly one codec, so
	// they never contend. The single-frame codecs (Opus, G.711, G.726, L16, raw) drain
	// it in the shared deliverOne; G.711, G.726 and L16 fold only past their
	// onFrame==nil early return, so a nil-callback stream never accumulates.
	pendingGap int
	// wirePayloadType is the payload type this track has settled on and
	// wirePTSet says whether it has settled. Until it settles, ptCandidate and
	// ptRun track the current run of one undeclared type, so a track adopts a
	// type only after seeing it consistently. See acceptPayloadType; the zero
	// value accepts the first type it sees, so a track built by any path other
	// than newTrack cannot silently filter to PT 0.
	wirePayloadType uint8
	wirePTSet       bool
	ptCandidate     uint8
	ptRun           int
	baseTS          uint64
	baseSet         atomic.Bool
	// baselineFixed is set true the first time a baseline is established and is
	// never cleared, so the RTP-Info seed applies only to the very first
	// baseline. An SSRC reset clears baseSet (to re-baseline the new stream)
	// but leaves baselineFixed set, so the reset falls back to the first-packet
	// baseline rather than re-applying a stale seed.
	baselineFixed bool
	// pcmBuf is the reused s16le output scratch shared by the three codecs that
	// write PCM into it via scratch: G.711 (deliverG711 expands companded bytes),
	// L16 (deliverL16 byte-swaps) and G.726 (deliverG726 decodes ADPCM). A track
	// is only ever one codec, so the paths never contend for it.
	pcmBuf []byte
	// l16FrameSize is the byte width of one L16 sample-frame (2 * channels), set
	// by newTrack for a CodecL16 track and 0 otherwise. deliverL16 rejects a
	// payload that is not a whole number of frames, so a truncated multichannel
	// packet cannot deliver a half-frame that shifts channel interleaving.
	l16FrameSize int

	// seed and hasSeed carry the RTP-Info timestamp origin. Play stores them
	// on the caller goroutine, potentially while the reader is already
	// delivering early frames and reading them in seededOrigin, so they are
	// atomic to publish the seed without a data race.
	seed    atomic.Uint64
	hasSeed atomic.Bool

	// Per-track receive counters: reader writes, Stats reads.
	packets      atomic.Uint64
	payloadBytes atomic.Uint64
	wireBytes    atomic.Uint64
	seqGaps      atomic.Uint64
	duplicates   atomic.Uint64
	// reorderDrops counts datagrams the UDP Reorderer dropped before rtp.Stream
	// observed them (a duplicate or a too-late reorder). It is kept separate from
	// duplicates, which publishRRSnapshot overwrites from the stream's own count
	// on every released packet; Stats sums the two so TrackStats.Duplicates means
	// the same over UDP as over TCP. It stays zero on the TCP-interleaved path.
	reorderDrops atomic.Uint64
	malformed    atomic.Uint64
	ssrcResets   atomic.Uint64
	// lastFrameUnixNano is the wall-clock arrival (UnixNano) of the most recent
	// frame on this track's RTP channel, parsed or not: the per-track media
	// clock exposed as TrackStats.LastFrameAt. It is distinct from the Client's
	// lastFrameAt watchdog, which stamps EVERY interleaved frame on any channel
	// (RTCP included) to prove the peer is alive; this one stamps only media
	// arriving on this track's RTP channel.
	lastFrameUnixNano atomic.Int64

	// RTCP Receiver Report snapshot: reader writes, keepalive timer reads.
	rrHighestSeq     atomic.Uint32
	rrCumulativeLost atomic.Uint32
	senderSSRC       atomic.Uint32
	lastSR           atomic.Uint32
	lastSRUnixNano   atomic.Int64

	// srClock is the published RTP-to-NTP correspondence from the most recent
	// Sender Report, stored whole behind one pointer so its fields can never
	// tear across an update. handleRTCP writes it, the SSRC-reset path clears
	// it, Stats loads it; the pointed-to value is immutable.
	srClock atomic.Pointer[audiostream.SenderClock]
}

// newTrack builds a track's depacketization pipeline from its resolved
// descriptor and the RTCP channel the server assigned. The RTP channel is not
// stored: the routing table maps it to this track, and nothing reads it back.
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
//
//nolint:gocritic // hugeParam: desc is the per-track descriptor threaded from Describe; newTrack runs once per Setup, not on the packet path, so the copy is not worth a pointer.
func newTrack(id int, desc describedTrack, opts SetupOptions, rtcpCh int, logger *slog.Logger) *track {
	tr := &track{
		id:             id,
		kind:           deliverRaw,
		clockRate:      clockRateTicks(desc.clockRate),
		discard:        opts.Discard,
		rtcpChannel:    rtcpCh,
		control:        desc.control,
		sdpPayloadType: desc.payloadType,
	}
	if desc.media != audiostream.MediaAudio {
		logWarn(logger, "track is not audio; delivering raw payloads",
			"track", id, "media", desc.media.String())
		return tr
	}
	switch codec := desc.codec.(type) {
	case audiostream.CodecAAC:
		tr.configureAAC(desc.aac, logger)
	case audiostream.CodecMP4ALATM:
		tr.configureLATM(codec, desc.latmDepack, desc.latmResolved, logger)
	case audiostream.CodecOpus:
		tr.kind = deliverOpus
	case audiostream.CodecG711:
		tr.kind = deliverG711
		tr.law = codec.Law
	case audiostream.CodecL16:
		tr.kind = deliverL16
		tr.l16FrameSize = 2 * codec.Channels
	case audiostream.CodecG726:
		tr.configureG726(codec, opts.G726Packing, logger)
	case audiostream.CodecFLAC:
		tr.kind = deliverFLAC
		tr.flac = flac.New()
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

// configureG726 selects the ITU-T G.726 ADPCM decoder for a CodecG726 track,
// with the codeword packing the SDP resolved (RFC 3551 for the plain G726-NN
// names, AAL2 for the AAL2-G726-NN ones) unless the caller overrode it through
// SetupOptions.G726Packing, for a camera whose encoding name does not match how
// it actually packs. The SDP layer only resolves the four valid bit rates and
// the two defined packings, and the override maps onto those same two, so
// g726.New cannot fail here in practice; the fallback to raw delivery is
// defensive, matching the other configure* helpers.
func (tr *track) configureG726(codec audiostream.CodecG726, override G726PackingOverride, logger *slog.Logger) {
	packing, ok := override.resolve(codec.Packing)
	if !ok {
		// An out-of-range override resolves to the SDP packing (fail-open, so a bad
		// config value never fails Setup), but that is exactly the
		// silent-wrong-audio outcome the option exists to escape, so it must not
		// pass without a diagnostic. Name the bad value and state that the SDP
		// packing is being used instead.
		logWarn(logger, "g726 packing override value out of range; using SDP packing",
			"track", tr.id, "override", uint8(override), "sdp", codec.Packing.String())
	}
	if packing != codec.Packing {
		logWarn(logger, "g726 codeword packing overridden by caller",
			"track", tr.id, "sdp", codec.Packing.String(), "using", packing.String())
	}
	dec, err := g726.New(codec.BitRate, packing)
	if err != nil {
		tr.kind = deliverRaw
		logWarn(logger, "g726 configuration invalid; delivering raw payloads", "error", err)
		return
	}
	tr.kind = deliverG726
	tr.g726 = dec
}

// scratch returns tr.pcmBuf resized to n bytes for a PCM delivery path to write
// into, growing the backing array with a 1.25x headroom factor when it must
// reallocate and reslicing in place otherwise. The returned slice aliases
// tr.pcmBuf and stays valid only until the next scratch call on the same track;
// the single-codec-per-track invariant means the G.711, L16 and G.726 paths
// never contend for it.
func (tr *track) scratch(n int) []byte {
	if cap(tr.pcmBuf) < n {
		tr.pcmBuf = make([]byte, n, n+n/4)
	} else {
		tr.pcmBuf = tr.pcmBuf[:n]
	}
	return tr.pcmBuf
}

// configureLATM selects the MP4A-LATM depacketizer for a CodecMP4ALATM track,
// falling back to raw delivery when the config cannot be parsed (an out-of-band
// track with an unparseable, unsupported, or empty StreamMuxConfig; an in-band
// track always succeeds here, since it learns its config from the stream rather
// than from Setup).
//
// An out-of-band track's StreamMuxConfig was already parsed once at Describe:
// resolved carries the depacketizer built there (or nil when that parse failed)
// and alreadyResolved is true, so this adopts that result verbatim rather than
// calling latm.New again and re-logging a malformed config. An in-band track,
// and any track constructed without going through Describe, has alreadyResolved
// false and builds the depacketizer here as before.
//
// For an out-of-band track the depacketizer already knows its
// AudioSpecificConfig, equal to what Describe put on the Track (see
// resolveLATMASC), so tr.latmASC is seeded with it here: the first call to
// deliverLATM then sees no change and never fires OnCodecUpdate, matching the
// documented "does not fire for a config already known at Describe" contract.
// A seedless in-band track has no config yet, so dp.AudioSpecificConfig() is
// nil and tr.latmASC stays nil until the first packet resolves one. An in-band
// track whose SDP carried a config= (an RFC 3016 seed) knows its ASC at Setup
// just like the out-of-band case, so tr.latmASC is seeded here and the first
// reused-config packet likewise fires no OnCodecUpdate.
func (tr *track) configureLATM(codec audiostream.CodecMP4ALATM, resolved *latm.Depacketizer, alreadyResolved bool, logger *slog.Logger) {
	dp := resolved
	if !alreadyResolved {
		var err error
		dp, err = latm.New(latm.Config{
			MuxConfigPresent: codec.MuxConfigPresent,
			StreamMuxConfig:  codec.StreamMuxConfig,
		})
		if err != nil {
			tr.kind = deliverRaw
			logWarn(logger, "latm fmtp invalid; delivering raw payloads", "error", err)
			return
		}
	}
	if dp == nil {
		// Describe parsed the out-of-band StreamMuxConfig and it was malformed;
		// it logged there. Fall back to raw without re-parsing or re-logging.
		tr.kind = deliverRaw
		return
	}
	tr.kind = deliverLATM
	tr.latm = dp
	if asc := dp.AudioSpecificConfig(); asc != nil {
		tr.latmASC = append([]byte(nil), asc...)
	}
}

// ptAdoptThreshold is how many consecutive packets of one undeclared payload
// type a track tolerates before concluding the SDP was wrong and adopting that
// type. Five packets is on the order of 100 ms of audio at a typical 20 ms
// packet time: short enough that a camera which mis-declares its format loses
// only a prologue, long enough that a stray packet cannot capture the track.
const ptAdoptThreshold = 5

// acceptPayloadType decides whether a packet's payload type belongs to this
// track. Reader-goroutine only.
//
// The type the SDP declared always wins and settles the track for good. A track
// whose SDP declared nothing settles on the first type it sees. Otherwise the
// track adopts an undeclared type only after ptAdoptThreshold consecutive
// packets of that SAME type, with the declared type never appearing.
//
// Both halves are needed, because each fails alone. Enforcing the SDP's value
// alone hands a camera whose m= line disagrees with what it sends a session in
// which every packet is rejected, nothing is delivered, and the read-idle
// watchdog never fires because frames keep arriving: a silent, permanent stall
// on exactly the quirky firmware this package exists to tolerate. Adopting the
// first packet's type alone has the mirror-image failure: a session joined
// mid-DTMF latches onto the telephone-event, delivers IT as audio, and rejects
// every real packet after. Requiring a consistent run distinguishes a
// mis-declared stream, which is steadily wrong, from a stray, which is not; and
// requiring the SAME type across the run keeps a second stray from being the
// packet that gets adopted.
//
// A track whose SDP declared nothing has no basis to prefer any type, so it
// settles on its first packet and a stray at the head of the stream can capture
// it. That is the accepted cost of raw delivery for an unrecognized codec; the
// threshold is spent only where a declared type gives something to hold out for.
func (tr *track) acceptPayloadType(pt uint8, logger *slog.Logger) bool {
	if tr.sdpPayloadType == payloadTypeUnknown {
		if !tr.wirePTSet {
			tr.wirePayloadType, tr.wirePTSet = pt, true
		}
		return pt == tr.wirePayloadType
	}
	if int(pt) == tr.sdpPayloadType {
		tr.wirePayloadType, tr.wirePTSet = pt, true
		tr.ptRun = 0
		return true
	}
	if tr.wirePTSet {
		return pt == tr.wirePayloadType
	}
	if tr.ptRun > 0 && pt == tr.ptCandidate {
		tr.ptRun++
	} else {
		tr.ptCandidate, tr.ptRun = pt, 1
	}
	if tr.ptRun < ptAdoptThreshold {
		return false
	}
	logWarn(logger, "stream payload type differs from the one the SDP declared; using the stream's",
		"track", tr.id, "sdp", tr.sdpPayloadType, "stream", int(pt))
	tr.wirePayloadType, tr.wirePTSet = pt, true
	return true
}

// deliver depacketizes one RTP packet for its track and hands each resulting
// frame to onFrame. It selects the per-codec path from tr.kind. A
// depacketization error increments tr.malformed and delivers nothing; the
// session continues. onFrame may be nil, in which case nothing is delivered but
// counters still advance. The delivered Frame is a stack value whose Data
// aliases reader-owned memory (the AAC depacketizer buffer, the RTP payload, or
// tr.pcmBuf); it is valid only for the duration of the callback.
func (tr *track) deliver(pkt rtp.Packet, up rtp.Update, now time.Time, onFrame func(audiostream.Frame)) {
	switch tr.kind {
	case deliverAAC:
		tr.deliverAAC(pkt, up, now, onFrame)
	case deliverOpus:
		tr.deliverOpus(pkt, up, now, onFrame)
	case deliverG711:
		tr.deliverG711(pkt, up, now, onFrame)
	case deliverL16:
		tr.deliverL16(pkt, up, now, onFrame)
	case deliverG726:
		tr.deliverG726(pkt, up, now, onFrame)
	case deliverFLAC:
		tr.deliverFLAC(pkt, up, now, onFrame)
	case deliverLATM:
		// nil onCodecUpdate: this generic dispatch has no Config to read one
		// from, so it is test-only for LATM. Production delivery routes
		// through Client.process, which calls tr.deliverLATM directly with
		// cfg.OnCodecUpdate; see reader.go.
		tr.deliverLATM(pkt, up, now, onFrame, nil)
	default: // deliverRaw, and any kind a future codec adds without a path here.
		tr.deliverRaw(pkt, up, now, onFrame)
	}
}

// deliverAAC runs the RFC 3640 AAC-hbr depacketizer and delivers one frame per
// access unit. SeqGap is drained onto the FIRST AU a packet completes and zero
// on the rest, so summing SeqGap across frames counts each loss once; RTPTime
// is the packet timestamp for every AU; PTS interpolates by the AU's RTP
// offset. A malformed packet counts as malformed and yields no frame.
//
// The gap is drained from tr.pendingGap, not read from up.Gap directly, so a
// loss immediately before a buffering fragment (which completes no AU) or
// before a malformed packet (which yields no frame) still surfaces on the next
// delivered frame instead of being lost. The drain runs even when onFrame is
// nil, so the counter cannot grow unbounded; when the packet completes no AU
// the gap stays pending until one does.
func (tr *track) deliverAAC(pkt rtp.Packet, up rtp.Update, now time.Time, onFrame func(audiostream.Frame)) {
	tr.pendingGap += up.Gap
	aus, err := tr.aac.Depacketize(pkt.Payload, pkt.Header.Marker, pkt.Header.Timestamp)
	if err != nil {
		// The pending gap is retained for the next delivered frame.
		tr.malformed.Add(1)
		return
	}
	for i := range aus {
		gap := 0
		if i == 0 {
			gap = tr.pendingGap
			tr.pendingGap = 0
		}
		if onFrame == nil {
			continue
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

// deliverLATM runs the RFC 3016 MP4A-LATM depacketizer and delivers one
// frame per access unit, exactly like deliverAAC: RTPTime is the packet
// timestamp for every AU, PTS interpolates by the AU's RTP offset, and SeqGap
// is drained (from tr.pendingGap) onto the first AU only. A malformed packet
// counts as malformed and yields no frame, retaining the pending gap for the
// next delivered frame. Unlike AAC, LATM never buffers a fragment across
// packets, so the only path that completes no AU is the malformed one.
//
// Unlike the other codecs, it also carries the OnCodecUpdate integration
// point: onCodecUpdate is called from HERE, between Depacketize and the AU
// delivery loop below, which is what gives Config.OnCodecUpdate its ordering
// guarantee (it precedes the OnFrame call for the first AU decoded under the
// newly resolved config). The resolution runs on every packet, in-band or
// out-of-band, but only fires the callback when the depacketizer's ASC is
// now non-nil and differs from the tr.latmASC snapshot; configureLATM seeds
// that snapshot for an out-of-band track, so this path is a no-op there.
// onCodecUpdate may be nil (deliver()'s generic dispatch passes nil, since it
// has no Config to read one from; Client.process passes cfg.OnCodecUpdate).
func (tr *track) deliverLATM(pkt rtp.Packet, up rtp.Update, now time.Time,
	onFrame func(audiostream.Frame), onCodecUpdate func(audiostream.CodecUpdate),
) {
	tr.pendingGap += up.Gap
	aus, err := tr.latm.Depacketize(pkt.Payload, pkt.Header.Marker, pkt.Header.Timestamp)
	if err != nil {
		// The pending gap is retained for the next delivered frame.
		tr.malformed.Add(1)
		return
	}

	if asc := tr.latm.AudioSpecificConfig(); asc != nil && !bytes.Equal(asc, tr.latmASC) {
		// Two copies of one slice, each defending a distinct hazard, not the same
		// one twice. AudioSpecificConfig returns the depacketizer's own array by
		// reference, and the depacketizer recycles two ASC arrays across parses:
		// each new config is parsed into the scratch array (the one displaced two
		// parses ago), which then becomes active while the array it displaces is
		// kept as the next parse's scratch. So the snapshot below must not alias
		// asc. If it did, a resend of
		// the same config (which flips which physical array is the scratch)
		// followed by a genuine mid-stream change would parse that change straight
		// into the array tr.latmASC aliases, overwriting it in place; the
		// bytes.Equal test above would then compare that array against itself,
		// report no change, and OnCodecUpdate would never fire for the real change.
		// Taking a private copy keeps the comparison anchored to the last value we
		// actually reported.
		//
		// The callback's separate copy defends the opposite direction: a consumer
		// that violates the read-only contract on CodecUpdate and mutates what it
		// was handed must not be able to reach the depacketizer's live array
		// through it. Handing the callback its own copy keeps any such mutation
		// local. The two copies are not interchangeable: dropping the snapshot
		// silences real config changes, dropping the callback copy exposes the
		// depacketizer's buffer to a misbehaving consumer.
		tr.latmASC = append([]byte(nil), asc...)
		if onCodecUpdate != nil {
			onCodecUpdate(audiostream.CodecUpdate{
				TrackID: tr.id,
				Codec: audiostream.CodecMP4ALATM{
					MuxConfigPresent:    true,
					AudioSpecificConfig: append([]byte(nil), asc...),
				},
			})
		}
	}

	for i := range aus {
		gap := 0
		if i == 0 {
			gap = tr.pendingGap
			tr.pendingGap = 0
		}
		if onFrame == nil {
			continue
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
	tr.pendingGap += up.Gap
	data, err := opus.Depacketize(pkt.Payload)
	if err != nil {
		// The pending gap is retained for the next delivered frame.
		tr.malformed.Add(1)
		return
	}
	tr.deliverOne(data, pkt, up, now, onFrame)
}

// deliverG711 expands companded G.711 into s16le PCM in the reused tr.pcmBuf,
// growing it only when a larger packet arrives, and delivers one frame.
//
// The expansion is skipped entirely when nothing will consume it. A nil OnFrame
// is a supported configuration (statistics without delivery), and unlike AAC
// this codec carries no cross-packet state, so there is nothing to keep
// coherent by running the transform anyway.
//
// Growth reserves a quarter more than the packet needs. Sizing the buffer to
// exactly len(input) reallocates on every upward tick, which for a sender whose
// packet size ramps means an allocation per packet on the hot path.
func (tr *track) deliverG711(pkt rtp.Packet, up rtp.Update, now time.Time, onFrame func(audiostream.Frame)) {
	if onFrame == nil {
		return
	}
	// Folded past the nil-callback return above, not before it: with no callback
	// deliverOne is never reached to drain, so accumulating here would leak.
	tr.pendingGap += up.Gap
	buf := tr.scratch(2 * len(pkt.Payload))
	n, err := g711.Depacketize(buf, pkt.Payload, tr.law)
	if err != nil {
		// The pending gap is retained for the next delivered frame.
		tr.malformed.Add(1)
		return
	}
	tr.deliverOne(buf[:n], pkt, up, now, onFrame)
}

// deliverL16 converts big-endian L16 linear PCM (RFC 3551, network byte order)
// to little-endian s16le in the reused tr.pcmBuf and delivers one frame, so an
// L16 track hands the consumer PCM in the same byte order a G.711 track does.
//
// A payload that is empty or not a whole number of sample-frames counts as
// malformed and yields no frame, mirroring the empty-Opus rule. The frame width
// is 2*channels (tr.l16FrameSize): validating whole frames rather than whole
// samples means a truncated stereo packet is rejected instead of delivering a
// half-frame that would shift L/R interleaving for a consumer concatenating
// frames. A track not built by newTrack has l16FrameSize 0, which falls back to
// whole-sample (mono) validation rather than dividing by zero. malformed is
// counted before the onFrame==nil check, so the statistic does not depend on a
// callback being registered, matching deliverAAC and deliverOpus.
//
// The byte swap is a deliberate manual loop, not encoding/binary: a benchmark
// found binary.BigEndian.Uint16 plus binary.LittleEndian.PutUint16 about 40%
// slower on this per-packet path, and the manual form is equally alloc-free.
func (tr *track) deliverL16(pkt rtp.Packet, up rtp.Update, now time.Time, onFrame func(audiostream.Frame)) {
	n := len(pkt.Payload)
	frame := max(tr.l16FrameSize, 2)
	if n == 0 || n%frame != 0 {
		tr.malformed.Add(1)
		// Retain this packet's gap for the next delivered frame, but only when a
		// callback will consume one: with a nil callback deliverOne is never
		// reached to drain, so accumulating would leak. malformed is counted
		// first, before the nil check, so the statistic does not depend on a
		// callback being registered.
		if onFrame != nil {
			tr.pendingGap += up.Gap
		}
		return
	}
	if onFrame == nil {
		return
	}
	// Folded past the nil-callback return, mirroring the malformed branch: the
	// accumulator is touched only when a frame will be delivered to drain it.
	tr.pendingGap += up.Gap
	dst := tr.scratch(n)
	src := pkt.Payload
	for i := 0; i+1 < n; i += 2 {
		dst[i] = src[i+1]
		dst[i+1] = src[i]
	}
	tr.deliverOne(dst[:n], pkt, up, now, onFrame)
}

// deliverG726 runs the ITU-T G.726 ADPCM decoder and delivers one frame of
// s16le PCM, structurally identical to deliverG711: the sequence gap is folded
// only past the onFrame==nil return (so a nil-callback stream never
// accumulates), and the reused tr.pcmBuf holds the output. The decoder carries
// adaptive state across packets, so it is never reset here; only an SSRC change
// resets it (see resetDepacketizer). A Decode error (a too-small buffer, which
// the sizing below prevents) counts as malformed and yields no frame.
func (tr *track) deliverG726(pkt rtp.Packet, up rtp.Update, now time.Time, onFrame func(audiostream.Frame)) {
	if onFrame == nil {
		return
	}
	tr.pendingGap += up.Gap
	buf := tr.scratch(tr.g726.OutputLen(pkt.Payload))
	n, err := tr.g726.Decode(buf, pkt.Payload)
	if err != nil {
		// The pending gap is retained for the next delivered frame.
		tr.malformed.Add(1)
		return
	}
	tr.deliverOne(buf[:n], pkt, up, now, onFrame)
}

// deliverFLAC reassembles a FLAC frame across RTP packets and delivers one frame
// per completed FLAC frame, mirroring deliverAAC's fragment handling. The
// sequence gap is folded into tr.pendingGap and drained onto the delivered frame
// (through the shared deliverOne), so a loss immediately before a buffering
// fragment (which completes no frame) or a malformed packet (which yields no
// frame) still surfaces on the next delivered frame. A packet that only continues
// a fragmented frame completes nothing and retains the pending gap; a malformed
// payload (empty, or a reassembly overflow) is counted once and yields no frame.
// FLAC carries no cross-packet decoder state beyond the reassembly buffer, which
// resetDepacketizer clears on a gap or an SSRC change exactly as it does for AAC,
// so a lost fragment cannot be spliced onto the next frame.
func (tr *track) deliverFLAC(pkt rtp.Packet, up rtp.Update, now time.Time, onFrame func(audiostream.Frame)) {
	tr.pendingGap += up.Gap
	frame, err := tr.flac.Depacketize(pkt.Payload, pkt.Header.Marker, pkt.Header.Timestamp)
	if err != nil {
		// The depacketizer self-resets its fragment state on an error, so the next
		// packet starts clean. The pending gap is retained for the next delivered
		// frame.
		tr.malformed.Add(1)
		return
	}
	if frame == nil {
		// A buffering fragment completes no frame yet: not a frame and not an error.
		// The pending gap stays pending and surfaces when the final fragment
		// completes the frame.
		return
	}
	tr.deliverOne(frame, pkt, up, now, onFrame)
}

// deliverRaw delivers the undecoded RTP payload as one frame. It is the
// fallback for every track newTrack could not build a decoder for: an
// unrecognized codec, a non-audio media kind, an AAC mode other than AAC-hbr,
// and an AAC fmtp whose field widths are invalid.
func (tr *track) deliverRaw(pkt rtp.Packet, up rtp.Update, now time.Time, onFrame func(audiostream.Frame)) {
	tr.pendingGap += up.Gap
	tr.deliverOne(pkt.Payload, pkt, up, now, onFrame)
}

// deliverOne delivers a single-frame codec's payload, shared by Opus, G.711,
// L16, and raw. onFrame may be nil.
//
// SeqGap is drained from tr.pendingGap, not read from up.Gap directly, so a loss
// stranded on a preceding malformed single-frame packet (which yields no frame)
// still surfaces on the next delivered frame instead of being lost, exactly as
// deliverAAC/deliverLATM do. Each caller folds up.Gap into tr.pendingGap before
// calling here (Opus and raw at the top; G.711 and L16 only once past their
// onFrame==nil early return, since those skip the byte-level transform when
// nothing consumes it). The drain runs even when onFrame is nil, so on the paths
// that reach deliverOne (raw on every packet; Opus, G.711 and L16 on every valid
// packet) the counter cannot grow unbounded: a stranded gap is retained only
// across consecutive no-frame packets and drains on the next delivered frame,
// exactly as deliverAAC/deliverLATM drain on the first completed AU.
func (tr *track) deliverOne(data []byte, pkt rtp.Packet, up rtp.Update, now time.Time, onFrame func(audiostream.Frame)) {
	gap := tr.pendingGap
	tr.pendingGap = 0
	if onFrame == nil {
		return
	}
	onFrame(audiostream.Frame{
		TrackID:    tr.id,
		Data:       data,
		RTPTime:    pkt.Header.Timestamp,
		PTS:        tr.ptsOf(up.Timestamp),
		ReceivedAt: now,
		SeqGap:     gap,
	})
}

// maxPTSSeconds is the largest whole-second count a time.Duration holds. A
// duration is an int64 nanosecond count, so the seconds term of a PTS must stay
// below this or the multiply that scales it wraps negative.
const maxPTSSeconds = math.MaxInt64 / int64(time.Second)

// ptsOf computes the presentation time of an unwrapped 64-bit RTP timestamp
// relative to tr.baseTS and the track clock rate, or 0 when the clock rate is
// unknown.
//
// Three separate overflows are possible and all three are handled here, because
// every input is remote. A timestamp below the baseline (a reordered packet, or
// an AU offset applied to one) would wrap the unsigned subtraction into an
// enormous delta, so it clamps to zero. delta*1e9 would overflow a uint64 after
// about 585 seconds of stream, so the division is split into whole seconds and
// a remainder. And the seconds term itself is unbounded, because a sender may
// advance the RTP timestamp by up to 2^31 per packet, so it is clamped to what
// a time.Duration can express rather than being allowed to wrap negative.
func (tr *track) ptsOf(ts uint64) time.Duration {
	rate := tr.clockRate
	if rate == 0 || ts < tr.baseTS {
		return 0
	}
	delta := ts - tr.baseTS
	sec := delta / rate
	if sec >= uint64(maxPTSSeconds) {
		return time.Duration(maxPTSSeconds) * time.Second
	}
	frac := (delta % rate) * uint64(time.Second) / rate
	return time.Duration(sec)*time.Second + time.Duration(frac)
}

// resetDepacketizer clears codec reassembly state. AAC and FLAC reassembly state
// is cleared on BOTH a gap and an SSRC change (regardless of onSSRCChange), so a
// lost fragment cannot corrupt the next access unit or frame: both fragment a
// unit across packets, so a hole leaves partial state that must be dropped.
// LATM does not fragment across packets, so it carries no cross-packet fragment
// state, only a retained StreamMuxConfig. That config must SURVIVE a gap and be
// reset only on an SSRC change (onSSRCChange true), where a new source may use a
// different config. Clearing it on a gap would silence an in-band sender that
// transmits the config once and thereafter sets useSameStreamMux (RFC 3016
// permits this): a single lost packet would leave every later packet
// ErrNoConfig for the rest of the session.
//
// The nil checks are here and not in deliverAAC/deliverLATM, which
// dereference tr.aac/tr.latm directly, because the two are asking different
// questions. Those methods run only for their own kind, and configureAAC/
// configureLATM are the only writers of that kind, setting the depacketizer
// field in the same two lines, so there it cannot be nil. This function runs
// for EVERY track on every gap, most of which have no depacketizer at all.
//
// On an SSRC change tr.latmASC is re-seeded from whatever survives
// tr.latm.Reset(), rather than cleared unconditionally, so the change
// re-announces the resolved config for an in-band track without spuriously
// refiring one for an out-of-band track. latm.Depacketizer.Reset documents the
// asymmetry this relies on: it clears a seedless in-band config (Reset sets its
// ASC back to nil), re-establishes an in-band config= seed (Reset lands on the
// seed's ASC), and leaves an out-of-band one untouched (Reset is a no-op there,
// so the ASC survives unchanged). Re-seeding tr.latmASC after Reset therefore
// lands on nil for a seedless in-band track, so the next resolved config's
// deliverLATM sees no snapshot to compare against and reports it through
// OnCodecUpdate again; and on unchanged bytes for a seeded in-band or an
// out-of-band track, so the next packet's deliverLATM sees no change and never
// fires.
func (tr *track) resetDepacketizer(onSSRCChange bool) {
	if onSSRCChange {
		// A new source restarts the sequence space, so any gap still pending from
		// the old source must not carry onto the new source's first frame. Cleared
		// only here (SSRC reset), never on a plain gap, where the pending gap must
		// survive to drain onto the next delivered frame. This is a standalone
		// block, not nested under the tr.latm guard below, so it runs for every
		// track: AAC and the single-frame codecs (Opus, G.711, G.726, L16, raw) all use
		// the accumulator, and none of them owns a tr.latm.
		tr.pendingGap = 0
	}
	if tr.aac != nil {
		tr.aac.Reset()
	}
	if tr.flac != nil {
		// FLAC reassembles a frame across packets exactly as AAC does, so its
		// partial reassembly is dropped on BOTH a gap and an SSRC change (no
		// onSSRCChange guard): a lost final fragment must not be spliced onto the
		// next frame, which marker-bit framing alone cannot prevent.
		tr.flac.Reset()
	}
	if onSSRCChange && tr.g726 != nil {
		// G.726 carries adaptive predictor and quantizer state across packets;
		// a new source restarts the stream, so the decoder must too. On a plain
		// gap it is deliberately not reset: there is no clean G.726 resync point
		// mid stream and the state re-converges on its own.
		tr.g726.Reset()
	}
	if onSSRCChange && tr.latm != nil {
		tr.latm.Reset()
		tr.latmASC = nil
		if asc := tr.latm.AudioSpecificConfig(); asc != nil {
			tr.latmASC = append([]byte(nil), asc...)
		}
	}
}

// publishRRSnapshot copies the reader-owned rtp.Stream counters into the atomic
// RR-snapshot fields the keepalive timer reads when building Receiver Reports,
// so the timer never touches the reader-owned Stream.
func (tr *track) publishRRSnapshot() {
	st := tr.stream.Stats()
	tr.rrHighestSeq.Store(st.ExtendedHighestSeq)
	tr.rrCumulativeLost.Store(cumulativeLost(st.SeqGaps))
	// Mirror the reader-owned stream's duplicate counter into an atomic so Stats
	// can surface it from any goroutine.
	tr.duplicates.Store(st.Duplicates)
}

// cumulativeLost narrows the 64-bit loss count to what an RFC 3550 report block
// can carry. The field is 24 bits and SIGNED (RFC 3550 section 6.4.1: the count
// may be negative when duplicates arrive), so the largest positive value it
// holds is 0x7FFFFF, and Appendix A.3 prescribes clamping there rather than
// wrapping. Saturating at 0xFFFFFF instead would put every value above 8.4
// million on the wire as a negative number, and the saturation value itself as
// -1, telling the server the client received one packet MORE than expected: a
// very lossy stream would be reported as a perfect one.
func cumulativeLost(seqGaps uint64) uint32 {
	const maxCumulativeLost = 0x7FFFFF
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
// reader can load it lock-free on the per-frame path, and again for the resync
// gate's channel-byte test on the rare frames that arrive while the reader is
// resynchronizing. The reader never mutates it.
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
