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
	"github.com/tphakala/go-audio-stream/depacket/g711"
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
)

// Client is a single raw-UDP audio source. It binds one UDP socket and, for the
// source's life, delivers frames to Config.OnFrame on its reader goroutine until
// Close is called or the read-idle watchdog fires.
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
	if c.cfg.Logger != nil {
		c.cfg.Logger.Debug("udpsource: listening", "url", c.url, "mode", c.cfg.Mode.String())
	}

	c.lastReadAt.Store(time.Now().UnixNano())
	go c.reader()
	return c, nil
}

// resolveFormat validates the config and resolves the immutable delivery
// geometry (kind/law for RTP, frameBytes/swap for PCM).
func (c *Client) resolveFormat() error {
	switch c.cfg.Mode {
	case ModeRTP:
		if c.cfg.ClockRate <= 0 {
			return fmt.Errorf("%w: ModeRTP requires a positive ClockRate", ErrInvalidConfig)
		}
		switch codec := c.cfg.Codec.(type) {
		case audiostream.CodecG711:
			c.kind, c.law = kindG711, codec.Law
		case audiostream.CodecL16:
			c.kind = kindL16
		case audiostream.CodecOpus:
			c.kind = kindOpus
		case audiostream.CodecUnknown:
			c.kind = kindOpaque
		case nil:
			return fmt.Errorf("%w: ModeRTP requires a Codec (use CodecUnknown for an opaque passthrough)", ErrInvalidConfig)
		default:
			return fmt.Errorf("%w: %T over raw RTP", ErrUnsupportedCodec, c.cfg.Codec)
		}
		// A PCM codec (G.711, L16) delivers interleaved s16le, so its channel
		// count must be known: it is reported in the format descriptor and, for
		// L16, sizes the whole-frame delivery boundary.
		if c.kind == kindG711 || c.kind == kindL16 {
			if c.cfg.Channels <= 0 {
				return fmt.Errorf("%w: a PCM codec (G.711/L16) over RTP requires a positive Channels", ErrInvalidConfig)
			}
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
		return nil
	case ModePCM:
		if c.cfg.Format.SampleRate <= 0 || c.cfg.Format.Channels <= 0 {
			return fmt.Errorf("%w: ModePCM requires a positive SampleRate and Channels", ErrInvalidConfig)
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
	}
	// Re-seed the PTS origin on the first packet and on every SSRC change, so the
	// media timeline starts at 0 and restarts cleanly for a new stream.
	if up.SSRCReset || !c.tsBaseSet {
		c.tsBase = up.Timestamp
		c.tsBaseSet = true
	}
	if up.Duplicate {
		c.duplicates.Add(1)
		return
	}
	if up.Gap > 0 {
		c.seqGaps.Add(uint64(up.Gap))
	}
	c.packets.Add(1)
	c.deliverRTP(pkt, up, now)
}

// deliverRTP depacketizes the RTP payload per the resolved codec kind and hands
// one frame to OnFrame. out aliases either pkt.Payload (opaque/Opus) or the
// reader-owned pcmBuf (G.711/L16), both valid only for the callback.
func (c *Client) deliverRTP(pkt rtp.Packet, up rtp.Update, now time.Time) {
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
	if c.cfg.OnFrame == nil {
		return
	}
	// Rebase the timestamp to the origin. A conformant sender keeps the timestamp
	// monotonic with the sequence, so this is a plain subtraction; guard the
	// unsigned underflow for a non-conformant sender whose forward-sequence packet
	// carries a timestamp before the origin (which would otherwise wrap to a
	// nonsense PTS) by clamping such a frame to PTS 0.
	var rel uint64
	if up.Timestamp > c.tsBase {
		rel = up.Timestamp - c.tsBase
	}
	c.cfg.OnFrame(audiostream.Frame{
		TrackID:    0,
		Data:       out,
		RTPTime:    pkt.Header.Timestamp,
		PTS:        mediatime.PTSFromSamples(rel, c.cfg.ClockRate),
		ReceivedAt: now,
		SeqGap:     up.Gap,
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
		return c.termError()
	case <-ctx.Done():
		c.initiateShutdown(ctx.Err())
		<-c.done
		return c.termError()
	}
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
