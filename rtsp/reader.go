package rtsp

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
)

// readChunk is the per-read scratch size for the reader accumulation buffer.
const readChunk = 4096

// errBufferFull is the backstop when the accumulation buffer would exceed
// maxReadBuffer. MaxHeaderBytes and MaxBodySize bound every well-formed unit
// below that ceiling, so it is unreachable except on a desynchronized or
// hostile stream.
var errBufferFull = errors.New("rtsp: read buffer limit exceeded")

// reader is the single reader goroutine. It owns every socket read for the
// connection's whole life. A deferred recover funnels a panic raised inside
// the framing loop (including one from a consumer callback in a later task)
// into shutdown, so such a panic becomes a clean session end rather than a
// process crash. It does not cover the terminal sequence that runs after it,
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
// count consumed. ErrIncomplete signals "read more".
//
// A unit handed to a step aliases rbuf and is valid only until the next fill,
// so a step must copy anything it keeps.
type stepFunc func(buf []byte) (int, error)

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

	// While resynchronizing, only a structurally validated unit may be
	// accepted. ParseInterleaved takes any '$' plus three bytes as a frame of
	// up to 65535 bytes without checking the channel or the length, and 0x24
	// recurs roughly every 256 bytes of RTP payload, so honouring it here
	// would discard up to 64 KiB of stream (swallowing any genuine response
	// inside it) and then clear the resync budget. maxResyncBytes could then
	// only fire on a 4096-byte run containing no '$' and no two-byte method
	// prefix, leaving the budget inert on exactly the binary streams it exists
	// to bound.
	//
	// Known consequence, to be removed with the channel routing table: once a
	// session is playing, the stream is almost entirely interleaved frames, so
	// a desync there cannot re-lock on a frame and will exhaust the budget
	// unless a real message arrives within maxResyncBytes. The discriminator
	// should become semantic (accept a frame mid-resync when its channel is in
	// the negotiated set) as soon as Setup populates channelPairs.
	if resyncing && !validated {
		return c.resyncOne()
	}

	n, err := step(c.rbuf[c.start:])
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
		c.resync = 0
		return true
	}
}

// stepInterleaved parses and dispatches one interleaved frame. The frame
// payload aliases rbuf, so handleInterleaved must consume it before the next
// read.
func (c *Client) stepInterleaved(buf []byte) (int, error) {
	f, n, err := ParseInterleaved(buf)
	if err != nil {
		return 0, err
	}
	c.handleInterleaved(f)
	return n, nil
}

// stepResponse parses one response and routes it to its pending caller.
func (c *Client) stepResponse(buf []byte) (int, error) {
	resp, n, err := ParseResponse(buf)
	if err != nil {
		return 0, err
	}
	c.dispatchResponse(resp)
	return n, nil
}

// stepRequest parses one server-initiated request and answers it.
func (c *Client) stepRequest(buf []byte) (int, error) {
	req, n, err := ParseRequest(buf)
	if err != nil {
		return 0, err
	}
	c.handleServerRequest(req)
	return n, nil
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

// handleInterleaved handles one interleaved frame. In this milestone no track
// is set up, so there is nothing to route to and every frame is skipped. The
// interleaved-channel routing table, per-track depacketization, and OnFrame
// delivery are added in a later task.
func (c *Client) handleInterleaved(f InterleavedFrame) {
	// Stamped here even though nothing is routed yet. armReadDeadline derives
	// the watchdog deadline from this value, so if Play set playing before the
	// first frame stamped it, the deadline would be 1970+ReadIdle, already
	// expired, and a healthy stream would die with ErrReadTimeout on its first
	// read. Advancing it from the moment frames arrive removes that ordering
	// hazard from the task that adds routing.
	c.lastFrameAt.Store(time.Now().UnixNano())
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
	var scratch [readChunk]byte
	n, err := c.conn.Read(scratch[:])
	if n > 0 {
		c.rbuf = append(c.rbuf, scratch[:n]...)
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
	sess := c.sessionID
	c.mu.Unlock()

	req := &Request{Method: methodTeardown, URL: reqURL, CSeq: int(c.cseq.Add(1)), Header: Header{}}
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	if sess != "" {
		req.Header.Set("Session", sess)
	}
	raw, err := MarshalRequest(req)
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
