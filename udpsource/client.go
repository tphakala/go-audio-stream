package udpsource

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/depacket/aac"
	"github.com/tphakala/go-audio-stream/depacket/flac"
	"github.com/tphakala/go-audio-stream/depacket/g711"
	"github.com/tphakala/go-audio-stream/depacket/g726"
	"github.com/tphakala/go-audio-stream/depacket/opus"
	"github.com/tphakala/go-audio-stream/internal/mediatime"
	"github.com/tphakala/go-audio-stream/rtsp/rtp"
)

// codecKind is the resolved delivery path for a ModeRTP source, derived from
// Config.Codec once during Open so the hot path switches on a byte.
type codecKind uint8

const (
	kindOpaque codecKind = iota // deliver the RTP payload unchanged
	kindG711                    // expand companded G.711 to s16le
	kindL16                     // byte-swap big-endian L16 to s16le
	kindOpus                    // deliver the Opus packet (compressed) unchanged
	kindAAC                     // run the RFC 3640 AAC-hbr depacketizer, one CodecAAC frame per access unit
	kindG726                    // run the ITU-T G.726 ADPCM decoder, expanding to s16le
	kindFLAC                    // reassemble FLAC frames across packets, one compressed frame per completed frame
)

// Client is a single raw-UDP audio source. It binds one UDP socket (and, when
// Config.RTCPListenAddr is set, a second for RTCP) and, for the source's life,
// delivers frames to Config.OnFrame on its reader goroutine until Close is called
// or the read-idle watchdog fires. When RTCP is enabled (RTCPListenAddr or
// RTCPMux) a received Sender Report populates TrackStats.SenderClock.
//
// Close, Wait, Stats, Info and Format are safe from any goroutine; of those, all
// but Wait may be called from inside OnFrame, because Wait blocks until the
// reader goroutine has finished and would deadlock it.
type Client struct {
	cfg  Config
	conn *net.UDPConn

	// url backs Info().URL; it is set in Open before the reader spawns and never
	// mutated after, so it needs no lock.
	url string

	// Resolved delivery geometry, set during Open and immutable after. kind and
	// law drive the RTP dispatch; frameBytes and swap the PCM path.
	kind       codecKind
	law        audiostream.Law
	frameBytes int
	swap       bool

	// srcIP, when non-nil, is the only source address whose datagrams are
	// accepted (Config.SourceIP). Immutable after Open.
	srcIP net.IP

	// cancel/lifecycle. closing is closed once shutdown begins; done once the
	// reader has finished; termErr records the first terminal cause.
	closeOnce sync.Once
	closing   chan struct{}
	done      chan struct{}
	mu        sync.Mutex
	termErr   error

	// lastReadAt is the liveness clock (UnixNano), stamped on every accepted
	// datagram and read by Stats.
	lastReadAt atomic.Int64

	// Per-source receive counters: the reader writes, Stats reads.
	packets    atomic.Uint64
	payload    atomic.Uint64
	wire       atomic.Uint64
	seqGaps    atomic.Uint64
	duplicates atomic.Uint64
	malformed  atomic.Uint64
	ssrcResets atomic.Uint64

	// reorderDrops counts datagrams the Reorderer dropped as late or duplicate
	// before Stream.Observe ever saw them (Config.Reorder path only). Stats folds
	// it into Duplicates so the reordered path reports the same Duplicates meaning
	// as the immediate path, where Observe counts duplicates directly.
	reorderDrops atomic.Uint64

	// Reader-owned state (no lock needed; only the reader touches it). stream
	// tracks RTP sequence continuity and timestamp unwrap; samples is the ModePCM
	// PTS counter; pcmBuf is reusable scratch for G.711 expansion and L16/PCM
	// byte-swapping.
	stream  rtp.Stream
	samples uint64
	pcmBuf  []byte

	// aac is the per-stream AAC-hbr depacketizer, built once in resolveFormat and
	// nil for every non-AAC source (nil is the "not the AAC path" gate). It owns
	// its own reused scratch and reassembly buffers, so the AAC path needs no
	// udpsource-side copy: an access unit is delivered synchronously to OnFrame
	// before the next Depacketize call that could overwrite its aliased backing.
	aac *aac.Depacketizer
	// flac reassembles FLAC frames across RTP packets for a CodecFLAC ModeRTP
	// source, nil otherwise. Like aac it carries cross-packet fragment state, so
	// it is dropped on both a sequence gap and an SSRC change (see resetReassembly).
	flac *flac.Depacketizer
	// g726 is the ITU-T G.726 ADPCM decoder for a CodecG726 ModeRTP source,
	// nil otherwise. It carries adaptive state across packets, so it is reset
	// only on an SSRC change, never on a plain sequence gap.
	g726 *g726.Decoder

	// pendingGap carries an observed sequence gap forward to the next frame
	// actually delivered. A gap can land on a packet that delivers no frame (an
	// AAC buffering fragment, or a malformed payload in any codec); without this
	// the loss would reach no Frame.SeqGap, and the frame that completes on a
	// later in-order packet would report SeqGap 0, hiding the discontinuity from
	// a consumer that conceals per frame. TrackStats.SeqGaps was already correct
	// (processRTP counts the gap before delivery); only Frame.SeqGap was wrong.
	//
	// Shared by every codec path: a udpsource Client resolves to exactly one
	// codecKind at Open, so the AAC path and the single-frame path never contend
	// for it, mirroring rtsp's shared track.pendingGap. Unlike rtsp, though,
	// deliverRTP runs the codec transform unconditionally and checks OnFrame only
	// at its shared tail, so the single-frame path folds up.Gap in once at the
	// top of that section and drains it at the tail before the nil-callback
	// return: a valid packet always reaches the drain (even under a nil
	// callback), so the counter stays bounded, while a malformed early-return
	// retains the gap for the next valid frame. deliverAAC folds at its own top
	// and drains on the first completed access unit. Reader-owned; only the
	// reader goroutine touches it, and processRTP clears it only on an SSRC
	// reset, never on a plain gap.
	pendingGap int

	// tsBase rebases the RTP timestamp so the first delivered frame is at PTS 0,
	// as the other sources deliver. Stream.Observe reports the absolute unwrapped
	// timestamp, so the source subtracts the first value (re-seeded on an SSRC
	// change, which restarts the media timeline).
	tsBase    uint64
	tsBaseSet bool

	// Reorder state, used only when Config.Reorder is set (reorder != nil, an
	// opt-in allocated in resolveFormat so a disabled or ModePCM source carries no
	// resequencing footprint). reorder resequences datagrams by 16-bit sequence
	// number; released is reused scratch for the packets it emits; lastSSRC and
	// haveSSRC detect an SSRC change from the raw parsed header, before Observe, to
	// flush and reset the resequencer. All are reader-owned; only the reader
	// goroutine touches them.
	reorder  *rtp.Reorderer
	released []rtp.Released
	lastSSRC uint32
	haveSSRC bool

	// RTCP / sender-clock state, used only when Config.RTCPListenAddr or
	// Config.RTCPMux is set (both zero-value-inert). rtcpConn is the second socket
	// for the separate-port model (nil for mux or when RTCP is disabled); rtcpWG
	// tracks its receive goroutine so Wait joins it and Close leaks nothing.
	// mediaSSRC and baseSet are published by the RTP reader (processRTP) so the RTCP
	// path can match a Sender Report to this stream's source and gate publication
	// until the first accepted packet identifies that source; they are read by
	// handleRTCP. srClock holds the most recent RTP-to-wall-clock correspondence
	// (nil until a usable Sender Report arrives): it has TWO writers, the RTP reader
	// (clears it on an SSRC change) and, on the separate-socket path, the RTCP
	// goroutine (publishes it in handleRTCP), and is read by Stats on any external
	// goroutine, so it stays an atomic.Pointer for Stats to read lock-free.
	//
	// rtcpMu serializes handleRTCP's read-match-publish against processRTP's
	// SSRC-reset identity swap, so a delayed Sender Report for the old source cannot
	// publish a mapping after the reset cleared it (a check-then-store the atomics
	// alone cannot make indivisible). It is taken only when the source identity
	// changes (first packet or SSRC reset) and per RTCP packet, never on the
	// steady-state media path, so the zero-alloc hot path is untouched.
	rtcpConn  *net.UDPConn
	rtcpWG    sync.WaitGroup
	rtcpMu    sync.Mutex
	mediaSSRC atomic.Uint32
	baseSet   atomic.Bool
	srClock   atomic.Pointer[audiostream.SenderClock]
}

// Client satisfies the root package's source-agnostic capture contract.
var _ audiostream.Source = (*Client)(nil)

// Open binds the UDP socket and returns an already-receiving source. ctx bounds
// only the bind; it does not end the stream once Open returns (use Close for
// that). On failure it returns the caller's ctx.Err() (ctx cancelled during the
// bind), ErrInvalidConfig, ErrUnsupportedCodec, or ErrBind.
//
//nolint:gocritic // Config is the documented public constructor signature, so passing it by value is intentional, matching the rtsp and httpsource clients.
func Open(ctx context.Context, cfg Config) (*Client, error) {
	cfg.applyDefaults()
	if cfg.ListenAddr == "" {
		return nil, fmt.Errorf("%w: ListenAddr is required", ErrInvalidConfig)
	}
	c := &Client{
		cfg:     cfg,
		closing: make(chan struct{}),
		done:    make(chan struct{}),
	}
	if err := c.resolveFormat(); err != nil {
		return nil, err
	}
	if cfg.SourceIP != "" {
		ip := net.ParseIP(cfg.SourceIP)
		if ip == nil {
			return nil, fmt.Errorf("%w: source IP %q", ErrInvalidConfig, cfg.SourceIP)
		}
		c.srcIP = ip
	}

	if cerr := ctx.Err(); cerr != nil {
		return nil, cerr
	}
	addr, err := net.ResolveUDPAddr("udp", cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBind, err)
	}
	c.conn = conn
	c.url = "udp://" + conn.LocalAddr().String()

	// Bind the separate RTCP socket before the reader spawns, so a bind failure
	// returns cleanly with no goroutine to tear down. RTCPMux carries RTCP on the
	// media socket and needs no second bind.
	if cfg.RTCPListenAddr != "" {
		rtcpAddr, rerr := net.ResolveUDPAddr("udp", cfg.RTCPListenAddr)
		if rerr != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("%w: RTCPListenAddr: %w", ErrInvalidConfig, rerr)
		}
		rtcpConn, rerr := net.ListenUDP("udp", rtcpAddr)
		if rerr != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("%w: RTCP socket: %w", ErrBind, rerr)
		}
		c.rtcpConn = rtcpConn
	}

	if c.cfg.Logger != nil {
		c.cfg.Logger.Debug("udpsource: listening", "url", c.url, "mode", c.cfg.Mode.String())
	}

	c.lastReadAt.Store(time.Now().UnixNano())
	go c.reader()
	if c.rtcpConn != nil {
		c.rtcpWG.Add(1)
		go c.rtcpReader()
	}
	return c, nil
}

// resolveFormat validates the config and resolves the immutable delivery
// geometry (kind/law for RTP, frameBytes/swap for PCM).
// resolveRTPCodec dispatches on Config.Codec for a ModeRTP source, selecting
// the delivery kind and building any per-codec depacketizer or decoder. It is
// split out of resolveFormat so that function stays under the cyclomatic
// complexity limit; resolveFormat still owns the surrounding transport and
// channel validation.
func (c *Client) resolveRTPCodec() error {
	switch codec := c.cfg.Codec.(type) {
	case audiostream.CodecG711:
		c.kind, c.law = kindG711, codec.Law
	case audiostream.CodecL16:
		c.kind = kindL16
	case audiostream.CodecOpus:
		c.kind = kindOpus
	case audiostream.CodecAAC:
		c.kind = kindAAC
		// Build the depacketizer once (applyDefaults has already filled
		// SamplesPerFrame). Surface a bad width as this package's own
		// ErrInvalidConfig: aac.ErrConfigInvalid is an internal detail, and
		// wrapping it keeps the udpsource error contract stable while still
		// letting errors.Is reach the precise cause through the chain.
		dp, derr := aac.New(aac.Config{
			SizeLength:       c.cfg.AAC.SizeLength,
			IndexLength:      c.cfg.AAC.IndexLength,
			IndexDeltaLength: c.cfg.AAC.IndexDeltaLength,
			SamplesPerFrame:  c.cfg.AAC.SamplesPerFrame,
		})
		if derr != nil {
			return fmt.Errorf("%w: AAC params: %w", ErrInvalidConfig, derr)
		}
		c.aac = dp
		// If the caller left the reported ASC empty but supplied one via
		// AACParams, fold it onto the stored CodecAAC value once here so
		// Format() reports it with no per-call allocation. An ASC set on the
		// CodecAAC value itself wins; AACParams.AudioSpecificConfig is the
		// fallback.
		if len(codec.AudioSpecificConfig) == 0 && len(c.cfg.AAC.AudioSpecificConfig) > 0 {
			c.cfg.Codec = audiostream.CodecAAC{AudioSpecificConfig: c.cfg.AAC.AudioSpecificConfig}
		}
	case audiostream.CodecG726:
		// Build the decoder once, with the caller's codeword packing (RFC 3551 by
		// default, AAL2 for an AAL2-G726 stream). A bad bit rate or packing
		// surfaces as this package's own ErrInvalidConfig, wrapping
		// g726.ErrUnknownBitRate or g726.ErrUnknownPacking so errors.Is still
		// reaches the precise cause, matching the AAC arm.
		dec, derr := g726.New(codec.BitRate, codec.Packing)
		if derr != nil {
			return fmt.Errorf("%w: G.726 configuration: %w", ErrInvalidConfig, derr)
		}
		c.g726 = dec
		c.kind = kindG726
	case audiostream.CodecFLAC:
		c.kind = kindFLAC
		c.flac = flac.New()
	case audiostream.CodecUnknown:
		c.kind = kindOpaque
	case nil:
		return fmt.Errorf("%w: ModeRTP requires a Codec (use CodecUnknown for an opaque passthrough)", ErrInvalidConfig)
	default:
		return fmt.Errorf("%w: %T over raw RTP", ErrUnsupportedCodec, c.cfg.Codec)
	}
	return nil
}

func (c *Client) resolveFormat() error {
	switch c.cfg.Mode {
	case ModeRTP:
		if c.cfg.ClockRate <= 0 {
			return fmt.Errorf("%w: ModeRTP requires a positive ClockRate", ErrInvalidConfig)
		}
		// An RTP payload type is 7 bits, so a value above 127 can never equal the
		// masked payload type of any parsed packet: processRTP would drop every
		// datagram as malformed and the source would deliver nothing. Reject it up
		// front rather than silently receive an empty stream.
		if c.cfg.PayloadType > 127 {
			return fmt.Errorf("%w: PayloadType must be 0-127 (7-bit RTP field), got %d", ErrInvalidConfig, c.cfg.PayloadType)
		}
		if err := c.resolveRTPCodec(); err != nil {
			return err
		}
		// A PCM codec (G.711, L16, G.726) delivers interleaved s16le, so its
		// channel count must be known: it is reported in the format descriptor
		// and, for L16, sizes the whole-frame delivery boundary.
		if c.kind == kindG711 || c.kind == kindL16 || c.kind == kindG726 {
			if c.cfg.Channels <= 0 {
				return fmt.Errorf("%w: a PCM codec (G.711/L16/G.726) over RTP requires a positive Channels", ErrInvalidConfig)
			}
		}
		// G.726 is single-channel at an 8 kHz clock (RFC 3551/4856). The decoder
		// holds one adaptive state, so it cannot decode interleaved multi-channel
		// codewords, and a non-8 kHz clock would misreport the sample rate and
		// skew PTS. Reject either rather than emit wrong audio or timing.
		if c.kind == kindG726 && c.cfg.Channels != 1 {
			return fmt.Errorf("%w: G.726 is single-channel, got Channels %d", ErrInvalidConfig, c.cfg.Channels)
		}
		if c.kind == kindG726 && c.cfg.ClockRate != 8000 {
			return fmt.Errorf("%w: G.726 requires an 8000 Hz clock, got %d", ErrInvalidConfig, c.cfg.ClockRate)
		}
		if c.kind == kindL16 {
			c.frameBytes = 2 * c.cfg.Channels
		}
		// Allocate the resequencer only when opted in, so a disabled ModeRTP source
		// keeps the immediate zero-alloc path and carries none of the Reorderer's
		// fixed-window footprint. A nil c.reorder is the disabled gate the reader
		// dispatch checks.
		if c.cfg.Reorder {
			c.reorder = &rtp.Reorderer{}
		}
		// RTCP is opt-in. The two enable modes are mutually exclusive, and RTCP-mux
		// needs a media payload type outside the RFC 5761 reserved range so the
		// second byte disambiguates media from RTCP on the shared socket.
		if c.cfg.RTCPListenAddr != "" && c.cfg.RTCPMux {
			return fmt.Errorf("%w: RTCPListenAddr and RTCPMux are mutually exclusive", ErrInvalidConfig)
		}
		if c.cfg.RTCPMux && c.cfg.PayloadType >= 64 && c.cfg.PayloadType <= 95 {
			return fmt.Errorf("%w: RTCPMux requires a PayloadType outside the RFC 5761 reserved range 64-95, got %d", ErrInvalidConfig, c.cfg.PayloadType)
		}
		return nil
	case ModePCM:
		if c.cfg.Format.SampleRate <= 0 || c.cfg.Format.Channels <= 0 {
			return fmt.Errorf("%w: ModePCM requires a positive SampleRate and Channels", ErrInvalidConfig)
		}
		// RTCP carries no meaning for a raw-PCM datagram stream (no RTP, no SSRC).
		if c.cfg.RTCPListenAddr != "" || c.cfg.RTCPMux {
			return fmt.Errorf("%w: RTCP (RTCPListenAddr/RTCPMux) applies only to ModeRTP", ErrInvalidConfig)
		}
		c.frameBytes = 2 * c.cfg.Format.Channels
		c.swap = c.cfg.Format.BigEndian
		return nil
	default:
		return fmt.Errorf("%w: unknown mode %d", ErrInvalidConfig, c.cfg.Mode)
	}
}

// Format returns the source's audio-format descriptor. A ModeRTP source reports
// its configured codec (and, for a PCM codec, the clock rate and channel count);
// a ModePCM source reports L16 PCM at the configured rate and channels. It is
// immutable after Open and safe from any goroutine.
func (c *Client) Format() audiostream.AudioFormat {
	if c.cfg.Mode == ModePCM {
		codec := audiostream.CodecL16{ClockRate: c.cfg.Format.SampleRate, Channels: c.cfg.Format.Channels}
		return audiostream.AudioFormat{
			Codec:      codec,
			Kind:       audiostream.PayloadKindFor(codec),
			SampleRate: c.cfg.Format.SampleRate,
			Channels:   c.cfg.Format.Channels,
		}
	}
	f := audiostream.AudioFormat{Codec: c.cfg.Codec, Kind: audiostream.PayloadKindFor(c.cfg.Codec)}
	if f.Kind == audiostream.KindPCMS16LE {
		f.SampleRate = c.cfg.ClockRate
		f.Channels = c.cfg.Channels
	}
	return f
}

// reader is the single reader goroutine. A deferred recover funnels a panic (an
// OnFrame callback above all) into shutdown, so it becomes a clean end rather
// than a crash. When the loop ends it closes done.
func (c *Client) reader() {
	defer close(c.done)
	defer c.recoverReader()
	c.recvLoop()
}

// recoverReader turns a panic in the reader into a fatal shutdown cause.
func (c *Client) recoverReader() {
	if r := recover(); r != nil {
		if c.cfg.Logger != nil {
			c.cfg.Logger.Error("udpsource: reader panic", "recovered", r)
		}
		c.initiateShutdown(fmt.Errorf("udpsource: reader panic: %v", r))
	}
}

// recvLoop reads datagrams until a terminal condition. Each read arms a fresh
// read-idle deadline (when configured), so an idle socket surfaces as
// ErrReadTimeout without a separate watchdog goroutine.
func (c *Client) recvLoop() {
	buf := make([]byte, c.cfg.readBufSize)
	for {
		select {
		case <-c.closing:
			return
		default:
		}
		if c.cfg.ReadIdle > 0 {
			_ = c.conn.SetReadDeadline(time.Now().Add(c.cfg.ReadIdle))
		}
		n, addr, err := c.conn.ReadFromUDP(buf)
		if err != nil {
			c.initiateShutdown(c.classifyReadErr(err))
			return
		}
		if c.srcIP != nil && !addr.IP.Equal(c.srcIP) {
			continue
		}
		now := time.Now()
		if c.cfg.RTCPMux && isRTCP(buf[:n]) {
			// Demultiplexed RTCP on the shared media socket (RFC 5761). Handle it off
			// the media accounting: RTCP bytes are not media wire bytes, and an
			// RTCP-only sender must not refresh the media liveness clock. The socket
			// read already re-armed the read-idle deadline, which is unavoidable on a
			// shared socket and harmless (a peer sending RTCP is alive).
			c.handleRTCP(buf[:n], now)
			continue
		}
		c.lastReadAt.Store(now.UnixNano())
		c.wire.Add(uint64(n))
		if c.cfg.Mode == ModePCM {
			c.deliverPCM(buf[:n], now)
			continue
		}
		if c.reorder != nil {
			c.handleRTPReordered(buf[:n], now)
			continue
		}
		c.handleRTP(buf[:n], now)
	}
}

// handleRTP is the ModeRTP receive path when reordering is disabled (the
// default). It parses one datagram as an RTP packet, drops a malformed one, and
// delegates the payload-type filter, sequence-continuity accounting, and delivery
// to processRTP. datagram aliases the receive buffer; the payload is copied or
// delivered within the callback before the next read overwrites it.
func (c *Client) handleRTP(datagram []byte, now time.Time) {
	pkt, err := rtp.ParsePacket(datagram)
	if err != nil {
		c.malformed.Add(1)
		return
	}
	c.processRTP(pkt, now)
}

// handleRTPReordered is the ModeRTP receive path when Config.Reorder is set. It
// parses each datagram, resequences the active stream through the Reorderer, and
// drains released packets in ascending sequence order through the shared
// processRTP tail (which applies the payload-type filter at drain time). An SSRC
// change flushes and resets the Reorderer, whose sequence space restarts with the
// new source. Packets still buffered when the stream ends are dropped, not
// flushed, matching the RTSP UDP receiver.
//
// Payload-type handling is deliberately split. A foreign payload type whose SSRC
// does not match the active stream is unrelated traffic on the wildcard port and
// is dropped here, before the Reorderer, so its unrelated sequence space cannot
// poison the buffer or thrash it via a spurious SSRC change. A foreign payload
// type sharing the active SSRC is an in-band mux on this stream (for example an
// RFC 4733 telephone-event) that occupies a real sequence number in this stream's
// space: it is pushed like any other packet and filtered out only at drain time,
// so it fills its slot instead of leaving a phantom gap that would stall the run
// for a whole reorder window. (Such a mux packet is dropped and counted exactly
// once: as malformed at drain when it arrives in order, or, if it arrives late,
// as a Reorderer drop folded into Duplicates.)
func (c *Client) handleRTPReordered(datagram []byte, now time.Time) {
	pkt, err := rtp.ParsePacket(datagram)
	if err != nil {
		c.malformed.Add(1)
		return
	}
	if pkt.Header.PayloadType != c.cfg.PayloadType && (!c.haveSSRC || pkt.Header.SSRC != c.lastSSRC) {
		c.malformed.Add(1)
		return
	}
	if c.haveSSRC && pkt.Header.SSRC != c.lastSSRC {
		// A new source restarts the sequence space, so drain the old source's
		// buffered packets in order and reset before the new packet establishes a
		// fresh release point. processRTP re-baselines the PTS origin when Observe
		// reports the SSRC change on the first released new-source packet.
		c.released = c.reorder.Flush(c.released[:0])
		c.drainReleased(c.released, now)
		c.reorder.Reset()
	}
	c.lastSSRC = pkt.Header.SSRC
	c.haveSSRC = true

	// The Reorderer retains the slice and datagram aliases the reused receive
	// buffer, so hand Push a fresh copy; re-parsing it on release keeps each
	// Released.Payload a self-contained RTP packet. This per-packet heap copy is
	// exactly why reordering is opt-in. A reused ring keyed by seq %
	// MaxReorderWindow is unsafe here: an arriving packet whose sequence is a
	// window multiple ahead of a still-buffered one shares its slot, and writing
	// the copy before Push force-releases the older packet would corrupt it.
	cp := make([]byte, len(datagram))
	copy(cp, datagram)

	// Read Late immediately before Push (Reset above zeroes it) and compare rather
	// than subtract, so a hypothetical decrease can never underflow-wrap the delta
	// into a huge positive that inflates the count.
	lateBefore := c.reorder.Stats().Late
	c.released = c.reorder.Push(pkt.Header.SequenceNumber, cp, c.released[:0])
	if lateAfter := c.reorder.Stats().Late; lateAfter > lateBefore {
		c.reorderDrops.Add(lateAfter - lateBefore)
	}
	c.drainReleased(c.released, now)
}

// drainReleased folds a run of Reorderer-released packets into the pipeline in
// sequence order. Each Released.Payload is a self-contained copy of a full RTP
// datagram, so it is re-parsed here; a copy that somehow fails to parse is
// counted malformed and skipped rather than dropping the whole run.
func (c *Client) drainReleased(released []rtp.Released, now time.Time) {
	for i := range released {
		pkt, err := rtp.ParsePacket(released[i].Payload)
		if err != nil {
			c.malformed.Add(1)
			continue
		}
		c.processRTP(pkt, now)
	}
}

// processRTP runs the payload-type filter plus the sequence-continuity
// accounting and delivery tail shared by the immediate path (handleRTP) and the
// reordered drain path (drainReleased): it drops a foreign payload type as
// malformed (without observing it, so it never perturbs the sequence or timestamp
// state), then observes the header for gap, duplicate, SSRC-reset, and
// timestamp-unwrap, re-seeds the PTS origin on the first packet and every SSRC
// change, drops a duplicate, folds the gap and packet counts, and delivers the
// frame. pkt aliases memory valid only for this call.
func (c *Client) processRTP(pkt rtp.Packet, now time.Time) {
	if pkt.Header.PayloadType != c.cfg.PayloadType {
		c.malformed.Add(1)
		return
	}
	up := c.stream.Observe(pkt.Header)
	if up.SSRCReset {
		c.ssrcResets.Add(1)
		// A new source restarts the media timeline, so any AAC access unit half
		// reassembled from the old source must be dropped before its first packet,
		// and any gap still pending from the old source's sequence space (AAC or a
		// single-frame codec) must not carry onto the new source's first frame.
		c.resetReassembly()
		if c.g726 != nil {
			// A new source restarts the G.726 stream, so its adaptive predictor
			// and quantizer state must restart too. Done only on an SSRC change,
			// not on a plain gap, where the state re-converges on its own.
			c.g726.Reset()
		}
		c.pendingGap = 0
	}
	// Re-seed the PTS origin on the first packet and on every SSRC change, so the
	// media timeline starts at 0 and restarts cleanly for a new stream, and publish
	// the media source identity for the RTCP path. Hold rtcpMu across the whole
	// identity swap so it is indivisible with respect to handleRTCP: a Sender Report
	// for the old source that races an SSRC reset either publishes before the swap
	// (and is then cleared by srClock.Store(nil) inside the same critical section)
	// or is evaluated after it against the new mediaSSRC, never leaving a stale
	// mapping. The lock is taken only when the identity changes, never on the
	// steady-state media path, so the zero-alloc delivery path is unaffected.
	if up.SSRCReset || !c.tsBaseSet {
		c.tsBase = up.Timestamp
		c.tsBaseSet = true
		c.rtcpMu.Lock()
		if up.SSRCReset {
			// Invalidate the old source's mapping as part of the atomic swap.
			c.srClock.Store(nil)
		}
		c.mediaSSRC.Store(pkt.Header.SSRC)
		c.baseSet.Store(true)
		c.rtcpMu.Unlock()
	}
	if up.Duplicate {
		c.duplicates.Add(1)
		return
	}
	if up.Gap > 0 {
		c.seqGaps.Add(uint64(up.Gap))
		// A sequence gap means a lost packet, which for AAC may be a lost
		// fragment; drop the partial reassembly so the missing bytes cannot be
		// spliced onto the next access unit, mirroring rtsp.resetDepacketizer. The
		// gap branch is after the duplicate return, so a duplicate (no real hole)
		// never discards a valid in-progress fragment.
		c.resetReassembly()
	}
	c.packets.Add(1)
	c.deliverRTP(pkt, up, now)
}

// resetReassembly drops any partially reassembled AAC access unit or FLAC frame.
// It is a no-op for every source that carries neither (c.aac and c.flac both
// nil), so the shared processRTP path can call it unconditionally on a
// discontinuity. Both fragment a unit across packets, so a lost fragment must not
// be spliced onto the next unit; a Client resolves to exactly one codec, so at
// most one of the two is non-nil.
func (c *Client) resetReassembly() {
	if c.aac != nil {
		c.aac.Reset()
	}
	if c.flac != nil {
		c.flac.Reset()
	}
}

// deliverRTP depacketizes the RTP payload per the resolved codec kind and hands
// one frame to OnFrame. out aliases either pkt.Payload (opaque/Opus) or the
// reader-owned pcmBuf (G.711/L16), both valid only for the callback.
func (c *Client) deliverRTP(pkt rtp.Packet, up rtp.Update, now time.Time) {
	if c.kind == kindAAC {
		// AAC yields zero-or-more access units per packet, not a single output
		// slice, so it has its own delivery loop.
		c.deliverAAC(pkt, up, now)
		return
	}
	if c.kind == kindFLAC {
		// FLAC reassembles a frame across packets and completes zero-or-one frame
		// per packet, so like AAC it has its own path rather than the single-output
		// switch below.
		c.deliverFLAC(pkt, up, now)
		return
	}
	// Fold this packet's gap into the pending total before the codec transform.
	// Unlike rtsp, which runs the transform only under a non-nil callback and so
	// must fold G.711/L16 past their own nil-callback return, udpsource always
	// runs the transform and checks OnFrame only at the shared tail below, so a
	// single fold here serves every single-frame codec. A malformed packet
	// returns early from the switch and RETAINS the pending gap; a valid packet
	// reaches the tail drain even under a nil callback, so the counter stays
	// bounded. In the common no-loss case up.Gap is 0 and this is inert.
	c.pendingGap += up.Gap
	var out []byte
	switch c.kind {
	case kindG711:
		dst := c.ensurePCMBuf(2 * len(pkt.Payload))
		wn, derr := g711.Depacketize(dst, pkt.Payload, c.law)
		if derr != nil {
			c.malformed.Add(1)
			return
		}
		out = dst[:wn]
	case kindL16:
		// L16 is interleaved big-endian s16. Deliver only whole sample-frames so
		// a consumer never sees a half-frame that shifts channel interleaving,
		// mirroring the rtsp L16 path and the ModePCM rule.
		usable := len(pkt.Payload) - len(pkt.Payload)%c.frameBytes
		if usable == 0 {
			c.malformed.Add(1)
			return
		}
		if usable < len(pkt.Payload) {
			c.malformed.Add(1)
		}
		out = byteSwapInto(c.ensurePCMBuf(usable), pkt.Payload[:usable])
	case kindG726:
		// G.726 carries adaptive state across packets; it is reset only on an
		// SSRC change (in processRTP), never here on a plain gap.
		dst := c.ensurePCMBuf(c.g726.OutputLen(pkt.Payload))
		wn, derr := c.g726.Decode(dst, pkt.Payload)
		if derr != nil {
			c.malformed.Add(1)
			return
		}
		out = dst[:wn]
	case kindOpus:
		b, derr := opus.Depacketize(pkt.Payload)
		if derr != nil {
			c.malformed.Add(1)
			return
		}
		out = b
	default: // kindOpaque
		out = pkt.Payload
	}
	c.payload.Add(uint64(len(out)))
	// Drain the pending gap onto this frame BEFORE the nil-callback return, so a
	// loss stranded on a preceding malformed packet surfaces here and the counter
	// cannot grow unbounded across a nil-callback stream. A plain gap never clears
	// it early; only an SSRC reset (in processRTP) does.
	gap := c.pendingGap
	c.pendingGap = 0
	if c.cfg.OnFrame == nil {
		return
	}
	c.cfg.OnFrame(audiostream.Frame{
		TrackID:    0,
		Data:       out,
		RTPTime:    pkt.Header.Timestamp,
		PTS:        mediatime.PTSFromSamples(c.relTicks(up.Timestamp), c.cfg.ClockRate),
		ReceivedAt: now,
		SeqGap:     gap,
	})
}

// relTicks rebases an absolute unwrapped RTP timestamp to the media origin
// (tsBase). A conformant sender keeps the timestamp monotonic with the sequence,
// so this is a plain subtraction; the guard clamps a non-conformant sender whose
// forward-sequence packet carries a timestamp before the origin to 0 rather than
// letting the unsigned subtraction wrap to a nonsense PTS.
func (c *Client) relTicks(ts uint64) uint64 {
	if ts > c.tsBase {
		return ts - c.tsBase
	}
	return 0
}

// deliverAAC depacketizes one RTP payload through the AAC-hbr depacketizer and
// hands each completed access unit to OnFrame as its own CodecAAC frame. A packet
// may complete several access units (a multi-AU packet), exactly one (a single-AU
// packet or the final fragment of a fragmented unit), or none (a buffering
// fragment). A malformed payload is counted once and skipped, keeping the reader
// running, matching the single-frame codecs.
//
// Each AU.Data is valid only until the next Depacketize call: within one packet
// the multi-AU slices are disjoint windows into pkt.Payload, and a reassembled
// fragment aliases the depacketizer's own buffer. The reader is single-threaded
// and calls OnFrame synchronously here before any further Depacketize, so the
// aliased bytes stay valid for the callback and no copy is needed. The AAC path
// therefore allocates nothing in steady state, the same posture as the opaque and
// Opus paths.
func (c *Client) deliverAAC(pkt rtp.Packet, up rtp.Update, now time.Time) {
	// Fold this packet's gap into the pending total. If the packet completes no
	// access unit (buffering fragment or malformed), the gap stays pending and
	// surfaces on the next delivered frame, so a loss immediately before a
	// fragmented access unit is not lost from Frame.SeqGap. In the common
	// no-loss case up.Gap is 0 and this is inert.
	c.pendingGap += up.Gap
	aus, err := c.aac.Depacketize(pkt.Payload, pkt.Header.Marker, pkt.Header.Timestamp)
	if err != nil {
		// A malformed AAC payload (truncated headers, an AU size past the payload,
		// fragment overflow, or interleaving) is counted once; the depacketizer
		// self-resets its fragment state on the error, so the next packet starts
		// clean. The pending gap is retained for the next delivered frame.
		c.malformed.Add(1)
		return
	}
	// A buffering fragment completes no access unit yet: not a frame and not an
	// error. Its wire bytes are already counted; it surfaces as a frame when its
	// final fragment arrives.
	for i := range aus {
		c.payload.Add(uint64(len(aus[i].Data)))
		// Report the pending gap on the first AU only and clear it, so summing
		// SeqGap across the frames of this packet counts each loss once. The drain
		// happens even when OnFrame is nil so the counter cannot grow unbounded.
		seqGap := 0
		if i == 0 {
			seqGap = c.pendingGap
			c.pendingGap = 0
		}
		if c.cfg.OnFrame == nil {
			continue
		}
		// Each access unit's PTS advances by its RTPOffset within the packet, so a
		// multi-AU packet spaces its units by SamplesPerFrame.
		c.cfg.OnFrame(audiostream.Frame{
			TrackID:    0,
			Data:       aus[i].Data,
			RTPTime:    pkt.Header.Timestamp,
			PTS:        mediatime.PTSFromSamples(c.relTicks(up.Timestamp+uint64(aus[i].RTPOffset)), c.cfg.ClockRate),
			ReceivedAt: now,
			SeqGap:     seqGap,
		})
	}
}

// deliverFLAC reassembles a FLAC frame across RTP packets and hands each
// completed frame to OnFrame as one compressed frame. A packet completes exactly
// one frame (an unfragmented packet or a frame's final fragment), or none (a
// buffering non-final fragment). A malformed payload (empty, or a reassembly
// overflow) is counted once and skipped, keeping the reader running, matching the
// AAC and single-frame paths.
//
// The pending gap is folded in at the top and drained onto the delivered frame,
// so a loss immediately before a buffering fragment (which delivers no frame)
// still surfaces on the next delivered frame. The drain runs before the
// nil-callback return so the counter stays bounded. The returned frame aliases
// either pkt.Payload (unfragmented) or the depacketizer's reassembly buffer, both
// valid only for the synchronous callback, so the FLAC path allocates nothing in
// steady state, matching the AAC and Opus paths.
func (c *Client) deliverFLAC(pkt rtp.Packet, up rtp.Update, now time.Time) {
	c.pendingGap += up.Gap
	frame, err := c.flac.Depacketize(pkt.Payload, pkt.Header.Marker, pkt.Header.Timestamp)
	if err != nil {
		// The depacketizer self-resets its fragment state on the error, so the next
		// packet starts clean. The pending gap is retained for the next delivered
		// frame.
		c.malformed.Add(1)
		return
	}
	if frame == nil {
		// A buffering fragment: not a frame and not an error. Its wire bytes are
		// already counted; the frame surfaces when its final fragment arrives.
		return
	}
	c.payload.Add(uint64(len(frame)))
	// Drain the pending gap onto this frame before the nil-callback return, so a
	// loss stranded on a preceding buffering fragment or malformed packet surfaces
	// here and the counter cannot grow unbounded. A plain gap never clears it
	// early; only an SSRC reset (in processRTP) does.
	gap := c.pendingGap
	c.pendingGap = 0
	if c.cfg.OnFrame == nil {
		return
	}
	c.cfg.OnFrame(audiostream.Frame{
		TrackID:    0,
		Data:       frame,
		RTPTime:    pkt.Header.Timestamp,
		PTS:        mediatime.PTSFromSamples(c.relTicks(up.Timestamp), c.cfg.ClockRate),
		ReceivedAt: now,
		SeqGap:     gap,
	})
}

// deliverPCM delivers one raw-PCM datagram as a frame, byte-swapping a
// big-endian source to s16le. Only the whole-sample-frame prefix is delivered; a
// trailing partial frame is counted malformed and dropped, so a consumer
// concatenating frames never sees a half-frame that shifts channel interleaving.
func (c *Client) deliverPCM(datagram []byte, now time.Time) {
	usable := len(datagram) - len(datagram)%c.frameBytes
	if usable == 0 {
		c.malformed.Add(1)
		return
	}
	if usable < len(datagram) {
		c.malformed.Add(1)
	}
	out := datagram[:usable]
	if c.swap {
		out = byteSwapInto(c.ensurePCMBuf(usable), datagram[:usable])
	}
	c.packets.Add(1)
	c.payload.Add(uint64(usable))
	pts := mediatime.PTSFromSamples(c.samples, c.cfg.Format.SampleRate)
	if c.cfg.OnFrame != nil {
		c.cfg.OnFrame(audiostream.Frame{
			TrackID:    0,
			Data:       out,
			RTPTime:    0,
			PTS:        pts,
			ReceivedAt: now,
			SeqGap:     0,
		})
	}
	c.samples += uint64(usable / c.frameBytes)
}

// ensurePCMBuf returns a reader-owned scratch slice of length n, growing the
// backing array only when it must, so the steady-state delivery path allocates
// nothing.
func (c *Client) ensurePCMBuf(n int) []byte {
	if cap(c.pcmBuf) < n {
		// Over-provision so a ramping payload size does not reallocate on every
		// upward step, matching the rtsp deliverG711/deliverL16 scratch sizing.
		c.pcmBuf = make([]byte, n, n+n/4)
	}
	return c.pcmBuf[:n]
}

// byteSwapInto writes the little-endian image of a big-endian s16 buffer into
// dst and returns the written prefix. It swaps whole 2-byte samples; an odd
// trailing byte is dropped. dst must be at least len(src) long.
func byteSwapInto(dst, src []byte) []byte {
	n := len(src) &^ 1
	for i := 0; i+1 < n; i += 2 {
		dst[i] = src[i+1]
		dst[i+1] = src[i]
	}
	return dst[:n]
}

// classifyReadErr maps a socket read error to the terminal cause: the recorded
// cause when shutdown is already in progress (a Close), ErrReadTimeout on a
// read-idle deadline, and ErrConnectionClosed otherwise. It checks the shutdown
// state first, so a Close-induced closed-socket error surfaces as the first
// recorded cause rather than a spurious connection error.
func (c *Client) classifyReadErr(err error) error {
	select {
	case <-c.closing:
		if te := c.termError(); te != nil {
			return te
		}
	default:
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return audiostream.ErrReadTimeout
	}
	return fmt.Errorf("%w: %w", ErrConnectionClosed, err)
}

// initiateShutdown funnels every terminal trigger through one place, exactly
// once; the first cause wins. It records the cause, signals the reader by
// closing closing, and closes the socket to unblock a parked ReadFromUDP.
func (c *Client) initiateShutdown(cause error) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		if c.termErr == nil {
			c.termErr = cause
		}
		c.mu.Unlock()
		close(c.closing)
		if c.conn != nil {
			_ = c.conn.Close()
		}
		// Close the RTCP socket too so its parked ReadFromUDP unblocks and the RTCP
		// goroutine exits; a nil rtcpConn (mux or RTCP disabled) is skipped.
		if c.rtcpConn != nil {
			_ = c.rtcpConn.Close()
		}
	})
}

// termError returns the recorded terminal cause, or nil if none yet.
func (c *Client) termError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.termErr
}

// Wait blocks until the stream ends and returns the terminal cause:
// audiostream.ErrClosed after Close, ctx.Err() if ctx cancels first,
// audiostream.ErrReadTimeout on watchdog expiry, ErrConnectionClosed on a socket
// failure, or the wrapped cause of a recovered OnFrame panic. Per the Source
// contract this set is not closed; the first cause wins. After Wait returns,
// OnFrame will not be called again. Do not call Wait from inside OnFrame; it
// deadlocks.
func (c *Client) Wait(ctx context.Context) error {
	select {
	case <-c.done:
	case <-ctx.Done():
		c.initiateShutdown(ctx.Err())
		<-c.done
	}
	// Join the RTCP goroutine (a no-op when RTCP is disabled or muxed, where
	// rtcpWG is zero): the media reader closed done, and initiateShutdown closed
	// the RTCP socket, so its goroutine is already unblocking. This guarantees no
	// RTCP goroutine outlives Wait.
	c.rtcpWG.Wait()
	return c.termError()
}

// Close ends the stream. It is idempotent and safe from any goroutine, including
// from inside OnFrame. Wait afterwards returns audiostream.ErrClosed unless an
// earlier cause had already ended the stream. Close returns nil.
func (c *Client) Close() error {
	c.initiateShutdown(audiostream.ErrClosed)
	return nil
}

// Stats returns a snapshot of the source's receive counters in a freshly
// allocated map keyed by the single track ID 0. Safe from any goroutine,
// including from inside OnFrame.
func (c *Client) Stats() audiostream.Stats {
	ts := audiostream.TrackStats{
		Packets:      c.packets.Load(),
		PayloadBytes: c.payload.Load(),
		WireBytes:    c.wire.Load(),
		SeqGaps:      c.seqGaps.Load(),
		Duplicates:   c.duplicates.Load() + c.reorderDrops.Load(),
		Malformed:    c.malformed.Load(),
		SSRCResets:   c.ssrcResets.Load(),
	}
	if nanos := c.lastReadAt.Load(); nanos != 0 {
		ts.LastFrameAt = time.Unix(0, nanos)
	}
	// Publish a value copy of the sender-clock mapping so the snapshot never
	// aliases the internal pointer the RTCP goroutine swaps. Nil (the default and
	// the disabled path) leaves SenderClock at its invalid zero value.
	if sc := c.srClock.Load(); sc != nil {
		ts.SenderClock = *sc
	}
	return audiostream.Stats{
		CapturedAt: time.Now(),
		Tracks:     map[int]audiostream.TrackStats{0: ts},
	}
}

// Info returns the source-neutral identity: the bound UDP address as a udp:// URL
// and an empty Server (a raw UDP peer sends no server identity). Safe from any
// goroutine, including from inside OnFrame.
func (c *Client) Info() audiostream.SourceInfo {
	return audiostream.SourceInfo{URL: c.url, Server: ""}
}
