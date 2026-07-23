package rtsp

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/rtsp/rtp"
)

// readChunk is the per-read scratch size for the reader accumulation buffer.
const readChunk = 4096

// errBufferFull is the backstop when the accumulation buffer would exceed
// maxReadBuffer. MaxHeaderBytes and MaxBodySize bound every well-formed unit
// below that ceiling, so it is unreachable except on a desynchronized or
// hostile stream.
var errBufferFull = errors.New("rtsp: read buffer limit exceeded")

// reader is the single reader goroutine. It owns every socket read for the
// connection's whole life. A deferred recover funnels a panic raised inside the
// framing loop into shutdown, so such a panic becomes a clean session end rather
// than a process crash. That covers the consumer's OnFrame callback, which this
// loop calls directly. It does not cover the terminal sequence that runs after it,
// nor any other goroutine. When the framing loop ends, it performs the
// terminal teardown and, as its final act, closes done.
func (c *Client) reader() {
	defer close(c.done)
	defer c.teardownAndJoin()
	defer c.recoverReader()
	c.readLoop()
}

// recoverReader turns a panic in the reader into a fatal shutdown cause.
func (c *Client) recoverReader() {
	if r := recover(); r != nil {
		c.initiateShutdown(fmt.Errorf("rtsp: reader panic: %v", r))
	}
}

// readLoop runs the byte-level framing loop until a terminal condition, at
// which point the causing shutdown has already been funneled and the loop
// returns.
func (c *Client) readLoop() {
	for {
		select {
		case <-c.closing:
			return
		default:
		}
		switch ClassifyStream(c.rbuf[c.start:]) {
		case FrameNeedMore:
			if !c.fillMore() {
				return
			}
		case FrameInterleaved:
			if !c.consume(c.stepInterleaved, unvalidatedUnit) {
				return
			}
		case FrameResponse:
			if !c.consume(c.stepResponse, validatedUnit) {
				return
			}
		case FrameRequest:
			if !c.consume(c.stepRequest, validatedUnit) {
				return
			}
		case FrameUnknown:
			if !c.resyncOne() {
				return
			}
		}
	}
}

// stepFunc parses one wire unit from buf, handles it, and returns the byte
// count consumed and whether the unit turned out to be usable. ErrIncomplete
// signals "read more".
//
// usable is what lets consume tell a genuine re-lock from a false one: a
// message parser only returns a unit it has already validated, so it reports
// true, while an interleaved frame reports whether it reached a track and, when
// this client parses its payload, parsed. Ignored unless the unit was admitted
// by the resync gate.
//
// A unit handed to a step aliases rbuf and is valid only until the next fill,
// so a step must copy anything it keeps.
type stepFunc func(buf []byte) (n int, usable bool, err error)

// Whether a step's parser structurally validates the unit it accepts, which
// decides if the unit may be trusted while resynchronizing. The message
// parsers check the start line, the header caps and Content-Length;
// ParseInterleaved checks only the leading '$'.
const (
	validatedUnit   = true
	unvalidatedUnit = false
)

// consume runs one step, handling ErrIncomplete by filling and a hard error
// by funneling a fatal framing shutdown. It returns false when the loop must
// exit.
func (c *Client) consume(step stepFunc, validated bool) bool {
	resyncing := c.resync > 0

	// While resynchronizing, an unvalidated unit is trusted only when the
	// routing table vouches for it. ParseInterleaved takes any '$' plus three
	// bytes as a frame of up to 65535 bytes without checking the channel or the
	// length, and 0x24 recurs roughly every 256 bytes of RTP payload, so
	// honouring it unconditionally would discard up to 64 KiB of stream
	// (swallowing any genuine response inside it) and then clear the resync
	// budget. maxResyncBytes could then only fire on a 4096-byte run containing
	// no '$' and no two-byte method prefix, leaving the budget inert on exactly
	// the binary streams it exists to bound.
	//
	// Refusing every frame instead is no better once PLAY starts, because the
	// stream is then almost entirely interleaved frames: a desync could never
	// re-lock on one and would exhaust the budget unless a real message arrived
	// within maxResyncBytes. So the discriminator is the channel byte, which
	// carries meaning this client assigned: a frame whose channel c.channels
	// binds is one this session negotiated.
	gated := false
	if resyncing && !validated {
		switch c.gateResyncFrame() {
		case resyncAccept:
			gated = true // fall through to the step, but stay on probation.
		case resyncFill:
			return c.fillMore()
		case resyncDiscard:
			return c.resyncOne()
		}
	}

	n, usable, err := step(c.rbuf[c.start:])
	switch {
	case errors.Is(err, ErrIncomplete):
		return c.fillMore()
	case err != nil:
		// ClassifyStream commits to a message on a two-byte prefix. While
		// resynchronizing those two bytes came from the garbage itself, so a
		// parse failure means "no unit starts here", not "the stream is
		// corrupt": a stray "PL" inside an RTP payload must not kill a session
		// that the FrameUnknown path one branch away would have recovered.
		if resyncing {
			return c.resyncOne()
		}
		c.initiateShutdown(fmt.Errorf("rtsp: framing error: %w", err))
		return false
	default:
		c.start += n
		// A gate-admitted frame clears the budget only once it has proved
		// usable. The channel byte alone is weak evidence: 0x00 and 0x01 are
		// both common bytes inside PCM and the channels most often bound, so a
		// run of garbage can present a plausible header every few hundred
		// bytes. Clearing the budget on that alone would restart it before it
		// could ever fire, and a stream that never yields a parseable packet
		// would hold the session open forever while delivering nothing, which
		// is the bound this whole gate exists to preserve.
		if !gated || usable {
			c.resync = 0
		}
		return true
	}
}

// resyncVerdict is what the resync gate decides about an interleaved frame
// header found while resynchronizing.
type resyncVerdict uint8

const (
	// resyncAccept means the channel byte is bound, so the frame is real
	// enough to re-lock on.
	resyncAccept resyncVerdict = iota
	// resyncFill means the channel byte has not been read yet.
	resyncFill
	// resyncDiscard means the channel byte is bound to nothing.
	resyncDiscard
)

// gateResyncFrame classifies the interleaved frame header at c.start while the
// reader is resynchronizing, by looking up the channel byte in the routing
// table.
//
// Filling on a one-byte buffer is what stops a genuine frame header split
// across two reads from being mistaken for garbage: the '$' arrives in one read
// and its channel byte in the next, and discarding on the strength of the first
// byte alone would drop the header and desync the very stream it is trying to
// recover. Filling here makes no less progress than the FrameNeedMore path it
// borrows: fill either appends bytes, returns a read error, or trips the buffer
// ceiling. It shares that path's one non-advancing case, a zero-byte read with
// a nil error, which re-loops.
//
// The residual false positive is a 0x24 inside an RTP payload followed by a
// byte that happens to equal a bound channel, which consumes whatever length
// follows. Nothing in a byte stream can rule that out, and the odds are not the
// uniform 2-per-256 the channel count suggests: a session binds two channels
// per set-up track, they are almost always 0x00 and 0x01, and those are the two
// most common bytes in near-silent PCM. That is why consume keeps such a frame
// on probation rather than treating admission as a re-lock.
func (c *Client) gateResyncFrame() resyncVerdict {
	buf := c.rbuf[c.start:]
	if len(buf) < 2 {
		return resyncFill
	}
	if _, bound := c.channels.Load().lookup(int(buf[1])); bound {
		return resyncAccept
	}
	return resyncDiscard
}

// stepInterleaved parses and dispatches one interleaved frame. The frame
// payload aliases rbuf, so handleInterleaved must consume it before the next
// read. It reports handleInterleaved's usable verdict.
func (c *Client) stepInterleaved(buf []byte) (n int, usable bool, err error) {
	f, n, err := ParseInterleaved(buf)
	if err != nil {
		return 0, false, err
	}
	return n, c.handleInterleaved(f), nil
}

// stepResponse parses one response and routes it to its pending caller.
func (c *Client) stepResponse(buf []byte) (n int, usable bool, err error) {
	resp, n, err := ParseResponse(buf)
	if err != nil {
		return 0, false, err
	}
	c.dispatchResponse(resp)
	return n, true, nil
}

// stepRequest parses one server-initiated request and answers it.
func (c *Client) stepRequest(buf []byte) (n int, usable bool, err error) {
	req, n, err := ParseRequest(buf)
	if err != nil {
		return 0, false, err
	}
	c.handleServerRequest(req)
	return n, true, nil
}

// resyncOne discards one unrecognized leading byte, bounded by maxResyncBytes
// CONSECUTIVE bytes: consume resets the counter on every successfully parsed
// unit, so the budget bounds one run of garbage, not the session total.
func (c *Client) resyncOne() bool {
	c.resync++
	if c.resync > maxResyncBytes {
		c.initiateShutdown(fmt.Errorf("rtsp: framing error: resync exceeded %d consecutive bytes", maxResyncBytes))
		return false
	}
	c.start++
	return true
}

// handleInterleaved routes one interleaved frame through its track's pipeline
// and delivers the resulting audiostream.Frame via OnFrame. It runs entirely on
// the reader goroutine before the next read, because f.Payload aliases the
// accumulation buffer. Unknown channels are dropped; a discard track's bytes
// are counted and dropped without per-packet parsing; RTCP is dispatched to
// handleRTCP; a malformed RTP packet is counted and skipped without ending the
// session.
//
// The watchdog clock is stamped for EVERY interleaved frame, before routing and
// whatever this function then decides to do with it. audiostream.ErrReadTimeout
// means the peer went quiet, so any frame is evidence it did not: a camera
// streaming on a channel this client never bound is alive and misbehaving, not
// dead. A caller that needs "no audio for N seconds" has OnFrame to measure it
// with. A stream that produces frames but never a usable packet is bounded
// instead by the resync budget, through the usable verdict returned here.
func (c *Client) handleInterleaved(f InterleavedFrame) bool {
	now := time.Now()
	c.lastFrameAt.Store(now.UnixNano())

	bind, ok := c.channels.Load().lookup(f.Channel)
	if !ok {
		return false // unknown or not-yet-published channel.
	}
	tr := bind.track
	if tr.discard {
		// A discard track is deliberately never parsed, so there is nothing to
		// validate and the frame counts as usable on the strength of its
		// channel alone.
		tr.packets.Add(1)
		tr.bytes.Add(uint64(len(f.Payload)))
		return true
	}
	if bind.isRTCP {
		return c.handleRTCP(tr, f.Payload, now)
	}

	pkt, err := rtp.ParsePacket(f.Payload)
	if err != nil {
		tr.malformed.Add(1)
		return false
	}
	if !tr.acceptPayloadType(pkt.Header.PayloadType, c.cfg.Logger) {
		// A second format multiplexed onto this track's RTP channel (a
		// telephone-event alongside speech, or the second entry of an m= line
		// listing several formats). Dropped BEFORE rtp.Stream observes it, so a
		// foreign SSRC cannot force a spurious reset and a foreign timestamp
		// cannot disturb this track's baseline. The cost is that the packet's
		// sequence number becomes a hole this track counts as loss, which is
		// the cheaper of the two errors: an inflated loss counter is a
		// diagnostic, a corrupted baseline is every PTS for the rest of the
		// session.
		tr.malformed.Add(1)
		return false
	}

	up := tr.stream.Observe(pkt.Header)
	if up.SSRCReset {
		tr.ssrcResets.Add(1)
		tr.baseSet = false
		tr.resetDepacketizer()
	}
	if !tr.baseSet {
		// Also the first packet of the session, where Observe reports no reset
		// because there was no previous stream to reset from. The sender SSRC
		// is recorded here rather than under SSRCReset for exactly that reason:
		// gating it on the reset left it zero for every session in which the
		// camera never changed SSRC and sent no Sender Report, so every
		// Receiver Report named a source the server was not sending.
		tr.senderSSRC.Store(pkt.Header.SSRC)
		tr.baseTS = c.seededOrigin(tr, up.Timestamp)
		tr.baseSet = true
		tr.baselineFixed = true // the seed applies only to this first baseline.
	}
	if up.Gap > 0 {
		tr.seqGaps.Add(uint64(up.Gap))
		tr.resetDepacketizer()
	}

	tr.deliver(pkt, up, now, c.cfg.OnFrame)
	tr.packets.Add(1)
	tr.bytes.Add(uint64(len(pkt.Payload)))
	tr.publishRRSnapshot()
	return true
}

// handleRTCP parses an RTCP compound packet and stores one Sender Report's
// fields into the track's atomic snapshot for the keepalive timer's Receiver
// Report builder: the sender SSRC, the middle 32 bits of the SR NTP timestamp
// (LSR), and the local receive time. It reports whether a report was stored.
//
// A compound packet may carry a Sender Report per contributing source when the
// peer is a mixer or translator, so the one this track wants is the one whose
// SSRC matches the media it is receiving, not simply the last in the packet.
// Falling back to the first keeps a peer that reports under an SSRC this track
// has not observed yet (an RTCP packet ahead of the first RTP packet) working.
//
// Malformed RTCP is ignored: RTCP is advisory, and a compound packet this
// parser cannot read must not end a session whose media is arriving fine.
func (c *Client) handleRTCP(tr *track, payload []byte, now time.Time) bool {
	reports, err := rtp.ParseCompound(payload)
	if err != nil || len(reports) == 0 {
		return false
	}
	sr := reports[0]
	if want := tr.senderSSRC.Load(); want != 0 {
		for i := range reports {
			if reports[i].SSRC == want {
				sr = reports[i]
				break
			}
		}
	}
	tr.senderSSRC.Store(sr.SSRC)
	tr.lastSR.Store(uint32(sr.NTPTimestamp >> 16))
	tr.lastSRUnixNano.Store(now.UnixNano())
	return true
}

// dispatchResponse routes a response to the caller waiting on its CSeq. An
// unknown CSeq (no pending caller) is dropped without terminating the session.
func (c *Client) dispatchResponse(resp *Response) {
	c.pendMu.Lock()
	ch, ok := c.pending[resp.CSeq]
	if ok {
		delete(c.pending, resp.CSeq)
	}
	c.pendMu.Unlock()
	if ok {
		ch <- resp // buffered cap 1: never blocks.
	}
}

// handleServerRequest answers a server-initiated request. A TEARDOWN ends the
// session with ErrServerTeardown; any other method is answered 200 OK echoing
// the CSeq.
func (c *Client) handleServerRequest(req *Request) {
	if strings.EqualFold(req.Method, methodTeardown) {
		c.initiateShutdown(ErrServerTeardown)
		return
	}
	raw, err := MarshalResponse(&Response{StatusCode: StatusOK, Reason: "OK", CSeq: req.CSeq})
	if err != nil {
		return
	}
	if werr := c.writeMessage(raw); werr != nil {
		c.initiateShutdown(fmt.Errorf("%w: %w", ErrConnectionClosed, werr))
	}
}

// fillMore reads more bytes, funneling the classified cause on a read error.
func (c *Client) fillMore() bool {
	err := c.fill()
	if err == nil {
		return true
	}
	if errors.Is(err, errBufferFull) {
		c.initiateShutdown(err)
		return false
	}
	c.initiateShutdown(c.classifyReadErr(err))
	return false
}

// fill compacts the consumed prefix, enforces the buffer ceiling, arms the
// read deadline for the current phase, and does one socket read. A zero-byte
// read with no error re-loops.
func (c *Client) fill() error {
	if c.start > 0 {
		c.rbuf = c.rbuf[:copy(c.rbuf, c.rbuf[c.start:])]
		c.start = 0
	}
	// Tested before the read, so the buffer can transiently reach
	// maxReadBuffer+readChunk-1. That slack is deliberate: testing the
	// post-read length instead would refuse to read once len(rbuf) passed
	// maxReadBuffer-readChunk and would reject a well-formed message in the
	// top readChunk bytes of the documented range. maxReadBuffer bounds the
	// largest unit the parsers accept, not the scratch append that carries it.
	if len(c.rbuf) >= maxReadBuffer {
		return errBufferFull
	}
	c.armReadDeadline()
	n, err := c.conn.Read(c.rscratch[:])
	if n > 0 {
		c.rbuf = append(c.rbuf, c.rscratch[:n]...)
		return nil
	}
	return err
}

// armReadDeadline sets the socket read deadline for the current phase, under
// deadlineMu so it can never race past a shutdown. Pre-play and with the
// watchdog disabled there is no deadline; while playing with ReadIdle > 0 the
// deadline is lastFrameAt + ReadIdle. When shutdown has begun it sets an
// immediate deadline instead, so a blocked read cannot outlive Close.
func (c *Client) armReadDeadline() {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	if c.shuttingDown {
		_ = c.conn.SetReadDeadline(time.Now())
		return
	}
	var deadline time.Time
	if c.playing.Load() && c.cfg.ReadIdle > 0 {
		deadline = time.Unix(0, c.lastFrameAt.Load()).Add(c.cfg.ReadIdle)
	}
	_ = c.conn.SetReadDeadline(deadline)
}

// classifyReadErr maps a read error to the terminal cause: the recorded cause
// when shutdown is already in progress, ErrReadTimeout on a watchdog timeout
// while playing, or ErrConnectionClosed (wrapping the cause) otherwise.
func (c *Client) classifyReadErr(err error) error {
	select {
	case <-c.closing:
		if te := c.termError(); te != nil {
			return te
		}
	default:
	}
	var nerr net.Error
	if c.playing.Load() && errors.As(err, &nerr) && nerr.Timeout() {
		return audiostream.ErrReadTimeout
	}
	return fmt.Errorf("%w: %w", ErrConnectionClosed, err)
}

// teardownAndJoin runs the reader's terminal sequence: a best-effort TEARDOWN
// when a session was established, then close the connection and join the timer
// goroutine (a no-op when Play never started it).
func (c *Client) teardownAndJoin() {
	if c.sessionEstablished() {
		c.sendTeardownBestEffort()
	}
	_ = c.conn.Close()
	c.wg.Wait()
}

// sessionEstablished reports whether a Session id was negotiated, so the
// terminal sequence knows whether a TEARDOWN is warranted.
func (c *Client) sessionEstablished() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID != ""
}

// sendTeardownBestEffort writes a TEARDOWN to the base URL with a short write
// deadline, then reads and discards whatever arrives for up to
// teardownDeadline. Any error is ignored: this runs only during shutdown.
func (c *Client) sendTeardownBestEffort() {
	c.mu.Lock()
	reqURL := c.baseURL
	c.mu.Unlock()

	// Credentials matter more here than anywhere. A TEARDOWN answered 401 leaves
	// the server holding the session until its own timeout, and on a camera that
	// allows one session at a time that refuses the next Dial. They come from
	// marshalBareRequest, shared with the keepalive, because this request does
	// not go through roundTrip and an inline copy of the header set is exactly
	// how they came to be missing here in the first place.
	raw, err := c.marshalBareRequest(methodTeardown, reqURL)
	if err != nil {
		return
	}

	c.writeMu.Lock()
	// Both deadlines go through the guarded helpers. Setting them directly
	// would push the socket deadline FORWARD, which is exactly what
	// deadlineMu exists to stop: initiateShutdown closes c.closing before it
	// takes deadlineMu, so the reader can observe the shutdown and reach here
	// while the interrupt is still in flight, and an unguarded set would
	// either be clobbered by it (aborting the TEARDOWN) or would itself undo
	// the immediate deadline the interrupt just installed.
	c.armTeardownDeadlines()
	if _, werr := c.conn.Write(raw); werr != nil {
		// Nothing to do about it during shutdown, but returning here skips a
		// discard read that cannot receive an answer to a request that was
		// never sent.
		c.writeMu.Unlock()
		return
	}
	c.writeMu.Unlock()

	var scratch [readChunk]byte
	_, _ = c.conn.Read(scratch[:])
}
