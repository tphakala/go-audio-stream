package rtsp_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/go-audio-stream/rtsp"
)

// rawConn is a server-side connection for wire-level assertions the
// testserver's request/frame-only API cannot express, chiefly reading a
// response the client wrote (its 200 answer to a server-initiated request).
// It mirrors the client-side helper in the testserver package, reversed.
type rawConn struct {
	conn net.Conn
	buf  []byte
	off  int
	cseq int
}

func (rc *rawConn) fill() error {
	if rc.off > 0 {
		rc.buf = rc.buf[:copy(rc.buf, rc.buf[rc.off:])]
		rc.off = 0
	}
	tmp := make([]byte, 4096)
	n, err := rc.conn.Read(tmp)
	if n > 0 {
		rc.buf = append(rc.buf, tmp[:n]...)
		return nil
	}
	return err
}

// readUnit reads and classifies the next text unit: exactly one of req or
// resp is non-nil on success. Interleaved and unknown bytes are an error.
func (rc *rawConn) readUnit() (req *rtsp.Request, resp *rtsp.Response, err error) {
	for {
		avail := rc.buf[rc.off:]
		switch rtsp.ClassifyStream(avail) {
		case rtsp.FrameNeedMore:
			if e := rc.fill(); e != nil {
				return nil, nil, e
			}
		case rtsp.FrameRequest:
			r, n, e := rtsp.ParseRequest(avail)
			if errors.Is(e, rtsp.ErrIncomplete) {
				if fe := rc.fill(); fe != nil {
					return nil, nil, fe
				}
				continue
			}
			if e != nil {
				return nil, nil, e
			}
			rc.off += n
			return r, nil, nil
		case rtsp.FrameResponse:
			r, n, e := rtsp.ParseResponse(avail)
			if errors.Is(e, rtsp.ErrIncomplete) {
				if fe := rc.fill(); fe != nil {
					return nil, nil, fe
				}
				continue
			}
			if e != nil {
				return nil, nil, e
			}
			rc.off += n
			return nil, r, nil
		default:
			return nil, nil, fmt.Errorf("rawConn: unexpected frame kind %d", rtsp.ClassifyStream(avail))
		}
	}
}

func (rc *rawConn) readReq() (*rtsp.Request, error) {
	req, resp, err := rc.readUnit()
	if err != nil {
		return nil, err
	}
	if resp != nil {
		return nil, fmt.Errorf("rawConn: got response %d, want request", resp.StatusCode)
	}
	return req, nil
}

func (rc *rawConn) readResp() (*rtsp.Response, error) {
	req, resp, err := rc.readUnit()
	if err != nil {
		return nil, err
	}
	if req != nil {
		return nil, fmt.Errorf("rawConn: got request %s, want response", req.Method)
	}
	return resp, nil
}

func (rc *rawConn) writeResp(req *rtsp.Request, code int, reason string, h rtsp.Header) error {
	raw, err := rtsp.MarshalResponse(&rtsp.Response{StatusCode: code, Reason: reason, CSeq: req.CSeq, Header: h})
	if err != nil {
		return err
	}
	_, err = rc.conn.Write(raw)
	return err
}

// writeStrayResponse writes a well-formed response carrying a CSeq no caller
// is waiting on, to exercise the reader's unknown-CSeq drop.
func (rc *rawConn) writeStrayResponse(code, cseq int) error {
	raw, err := rtsp.MarshalResponse(&rtsp.Response{StatusCode: code, Reason: "OK", CSeq: cseq})
	if err != nil {
		return err
	}
	_, err = rc.conn.Write(raw)
	return err
}

func (rc *rawConn) sendServerReq(method, url string) (int, error) {
	rc.cseq++
	raw, err := rtsp.MarshalRequest(&rtsp.Request{Method: method, URL: url, CSeq: rc.cseq})
	if err != nil {
		return 0, err
	}
	if _, err := rc.conn.Write(raw); err != nil {
		return 0, err
	}
	return rc.cseq, nil
}

// startRawServer listens on loopback, accepts one connection, and runs handle
// against it. It returns the rtsp:// URL the client dials.
func startRawServer(t *testing.T, handle func(rc *rawConn, url string)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	url := "rtsp://" + ln.Addr().String() + "/stream"
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		rc := &rawConn{conn: conn}
		handle(rc, url)
	}()
	// The listener close alone does not reach an accepted connection, and
	// nothing waited for the handler, so a handler blocked in Read outlived
	// the test and leaked a goroutine and a socket into every later test.
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
	})
	return url
}

// serveOptionsThen answers the Dial OPTIONS probe and then hands the raw
// connection to after, which writes whatever the test needs on the wire.
func serveOptionsThen(t *testing.T, after func(rc *rawConn)) string {
	t.Helper()
	return startRawServer(t, func(rc *rawConn, _ string) {
		req, err := rc.readReq()
		if err != nil {
			t.Errorf("readReq: %v", err)
			return
		}
		if err := rc.writeResp(req, 200, "OK", publicHeader()); err != nil {
			t.Errorf("writeResp: %v", err)
			return
		}
		after(rc)
	})
}

// Garbage that classifies as FrameUnknown: not '$', and no two-byte prefix
// ClassifyStream treats as the start of a message.
func unclassifiableBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 0xFF
	}
	return b
}

// A run of garbage shorter than the budget is skipped byte at a time and the
// session survives to parse the next real unit.
func TestResyncRecoversFromGarbage(t *testing.T) {
	url := serveOptionsThen(t, func(rc *rawConn) {
		_, _ = rc.conn.Write(unclassifiableBytes(64))
		_, _ = rc.sendServerReq("TEARDOWN", "rtsp://x/stream")
		for {
			if _, err := rc.readReq(); err != nil {
				return
			}
		}
	})

	ctx := context.Background()
	c, err := rtsp.Dial(ctx, rtsp.Config{URL: url, Timeout: testTimeout})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	// Reaching the TEARDOWN proves the garbage was skipped rather than
	// terminating the session or desynchronizing the framing.
	if err := c.Wait(waitCtx); !errors.Is(err, rtsp.ErrServerTeardown) {
		t.Errorf("Wait = %v, want ErrServerTeardown", err)
	}
}

// A run longer than the budget is fatal. This also pins the fix for the
// interleaved latch: the garbage contains 0x24 bytes, and ParseInterleaved
// accepts any '$' plus three bytes, so honouring one mid-resync would consume
// a bogus frame and reset the counter, leaving the budget unable to fire.
func TestResyncBudgetIsBoundedDespiteInterleavedLatch(t *testing.T) {
	url := serveOptionsThen(t, func(rc *rawConn) {
		junk := unclassifiableBytes(3 * 4096)
		// Latches start after the first byte so the reader is already
		// resynchronizing when it meets one. A '$' as the very first byte is
		// an in-sync frame start by protocol, not a latch.
		for i := 200; i < len(junk); i += 200 {
			junk[i] = '$'
		}
		_, _ = rc.conn.Write(junk)
		for {
			if _, err := rc.readReq(); err != nil {
				return
			}
		}
	})

	ctx := context.Background()
	c, err := rtsp.Dial(ctx, rtsp.Config{URL: url, Timeout: testTimeout})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	waitCtx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	err = c.Wait(waitCtx)
	if err == nil || !strings.Contains(err.Error(), "resync exceeded") {
		t.Errorf("Wait = %v, want a resync budget framing error", err)
	}
}

func publicHeader() rtsp.Header {
	h := rtsp.Header{}
	h.Set("Public", "OPTIONS, DESCRIBE, SETUP, PLAY, TEARDOWN")
	return h
}

func TestMidStreamServerOptions(t *testing.T) {
	type answer struct {
		code     int
		gotCSeq  int
		wantCSeq int
		err      error
	}
	answerCh := make(chan answer, 1)

	url := startRawServer(t, func(rc *rawConn, srvURL string) {
		req, err := rc.readReq()
		if err != nil {
			answerCh <- answer{err: err}
			return
		}
		if err := rc.writeResp(req, 200, "OK", publicHeader()); err != nil {
			answerCh <- answer{err: err}
			return
		}
		wantCSeq, err := rc.sendServerReq(methodOptions, srvURL)
		if err != nil {
			answerCh <- answer{err: err}
			return
		}
		resp, err := rc.readResp()
		if err != nil {
			answerCh <- answer{err: err}
			return
		}
		answerCh <- answer{code: resp.StatusCode, gotCSeq: resp.CSeq, wantCSeq: wantCSeq}
		// Prove the session is still alive by ending it with a TEARDOWN.
		_, _ = rc.sendServerReq("TEARDOWN", srvURL)
	})

	ctx := context.Background()
	c, err := rtsp.Dial(ctx, rtsp.Config{URL: url, Timeout: testTimeout})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	a := <-answerCh
	if a.err != nil {
		t.Fatalf("server side: %v", a.err)
	}
	if a.code != 200 {
		t.Errorf("client answered server OPTIONS with %d, want 200", a.code)
	}
	if a.gotCSeq != a.wantCSeq {
		t.Errorf("client answer CSeq = %d, want %d (echoed)", a.gotCSeq, a.wantCSeq)
	}
	if err := c.Wait(ctx); !errors.Is(err, rtsp.ErrServerTeardown) {
		t.Errorf("Wait = %v, want ErrServerTeardown (session should survive the mid-stream OPTIONS)", err)
	}
}

func TestUnknownCSeqResponseDropped(t *testing.T) {
	url := startRawServer(t, func(rc *rawConn, srvURL string) {
		req, err := rc.readReq()
		if err != nil {
			return
		}
		if err := rc.writeResp(req, 200, "OK", publicHeader()); err != nil {
			return
		}
		// A stray response no caller awaits must be dropped, not fatal.
		if err := rc.writeStrayResponse(200, 9999); err != nil {
			return
		}
		_, _ = rc.sendServerReq("TEARDOWN", srvURL)
	})

	ctx := context.Background()
	c, err := rtsp.Dial(ctx, rtsp.Config{URL: url, Timeout: testTimeout})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	// Reaching the TEARDOWN proves the stray response neither terminated the
	// session nor desynchronized the framing loop.
	if err := c.Wait(ctx); !errors.Is(err, rtsp.ErrServerTeardown) {
		t.Errorf("Wait = %v, want ErrServerTeardown (stray response must be dropped)", err)
	}
}

func TestReadIdleWatchdogDisabledPrePlay(t *testing.T) {
	// With ReadIdle set but no PLAY yet, the pre-play phase must not apply the
	// watchdog: an idle idle-state session stays alive until closed.
	s := startRawServer(t, func(rc *rawConn, _ string) {
		req, err := rc.readReq()
		if err != nil {
			return
		}
		_ = rc.writeResp(req, 200, "OK", publicHeader())
		// Go idle: never send anything. The client must not time out pre-play.
		for {
			if _, err := rc.readReq(); err != nil {
				return
			}
		}
	})

	ctx := context.Background()
	c, err := rtsp.Dial(ctx, rtsp.Config{URL: s, Timeout: testTimeout, ReadIdle: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	// Wait longer than ReadIdle; the session must still be alive (no frames yet).
	waitCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if err := c.Wait(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Wait = %v, want DeadlineExceeded (pre-play watchdog must be inert)", err)
	}
	_ = c.Close()
}

// A server that answers OPTIONS and ends the session in the SAME segment must
// still produce a successful Dial: the reader has to parse both units out of
// one fill and deliver the response before acting on the teardown.
//
// This covers the coalesced-segment parse path end to end. It does NOT cover
// the delivered-response-versus-shutdown race in roundTrip's select: by the
// time the server writes, the caller is already parked in the select, so the
// response case commits before done closes. That race needs the caller to be
// preempted between the write and the select, which
// TestRoundTripPrefersDeliveredResponseOverShutdown forces deterministically.
func TestDialSucceedsWhenResponseAndTeardownShareASegment(t *testing.T) {
	const iterations = 5
	for i := range iterations {
		url := startRawServer(t, func(rc *rawConn, _ string) {
			req, err := rc.readReq()
			if err != nil {
				return
			}
			resp, err := rtsp.MarshalResponse(&rtsp.Response{
				StatusCode: 200, Reason: "OK", CSeq: req.CSeq, Header: publicHeader(),
			})
			if err != nil {
				t.Errorf("MarshalResponse: %v", err)
				return
			}
			teardown, err := rtsp.MarshalRequest(&rtsp.Request{
				Method: "TEARDOWN", URL: "rtsp://x/stream", CSeq: 1,
			})
			if err != nil {
				t.Errorf("MarshalRequest: %v", err)
				return
			}
			// One write, so the reader parses both units from one fill.
			if _, err := rc.conn.Write(append(resp, teardown...)); err != nil {
				return
			}
			for {
				if _, err := rc.readReq(); err != nil {
					return
				}
			}
		})

		ctx := context.Background()
		c, err := rtsp.Dial(ctx, rtsp.Config{URL: url, Timeout: testTimeout})
		if err != nil {
			t.Fatalf("iteration %d: Dial = %v, want success (the server answered 200)", i, err)
		}
		_ = c.Close()
		waitCtx, cancel := context.WithTimeout(ctx, testTimeout)
		_ = c.Wait(waitCtx)
		cancel()
	}
}
