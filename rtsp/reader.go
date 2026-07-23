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
// maxReadBuffer. The M4a per-unit caps make it unreachable for well-formed
// units.
var errBufferFull = errors.New("rtsp: read buffer limit exceeded")

// reader is the single reader goroutine. It owns every socket read for the
// connection's whole life. A deferred recover funnels any unexpected panic
// (including one from a consumer callback in a later task) into shutdown, so
// the library never brings down the host process. When the framing loop ends,
// it performs the terminal teardown and, as its final act, closes done.
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
			if !c.consume(c.stepInterleaved) {
				return
			}
		case FrameResponse:
			if !c.consume(c.stepResponse) {
				return
			}
		case FrameRequest:
			if !c.consume(c.stepRequest) {
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
type stepFunc func(buf []byte) (int, error)

// consume runs one step, handling ErrIncomplete by filling and a hard error
// by funneling a fatal framing shutdown. It returns false when the loop must
// exit.
func (c *Client) consume(step stepFunc) bool {
	n, err := step(c.rbuf[c.start:])
	switch {
	case errors.Is(err, ErrIncomplete):
		return c.fillMore()
	case err != nil:
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

// resyncOne discards one unrecognized leading byte, bounded by maxResyncBytes.
func (c *Client) resyncOne() bool {
	c.resync++
	if c.resync > maxResyncBytes {
		c.initiateShutdown(fmt.Errorf("rtsp: framing error: resync exceeded %d bytes", maxResyncBytes))
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
	_ = c.conn.SetWriteDeadline(time.Now().Add(teardownDeadline))
	_, _ = c.conn.Write(raw)
	c.writeMu.Unlock()

	_ = c.conn.SetReadDeadline(time.Now().Add(teardownDeadline))
	var scratch [readChunk]byte
	_, _ = c.conn.Read(scratch[:])
}
