package rtsp_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/internal/testserver"
	"github.com/tphakala/go-audio-stream/rtsp"
)

// publicHeader is a Public header advertising the core methods, for the OPTIONS
// probe of tests that do not care which keepalive method is negotiated.
func publicHeader() rtsp.Header {
	h := rtsp.Header{}
	h.Set("Public", "OPTIONS, DESCRIBE, SETUP, PLAY, TEARDOWN")
	return h
}

// unclassifiableBytes returns n bytes that ClassifyStream treats as
// FrameUnknown: not '$', and no two-byte prefix it reads as the start of a
// message.
func unclassifiableBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 0xFF
	}
	return b
}

// A run of garbage shorter than the resync budget is skipped byte at a time and
// the session survives to parse the next real unit.
func TestResyncRecoversFromGarbage(t *testing.T) {
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		serve(t, sc, methodOptions, 200, "OK", publicHeader(), nil)
		_ = sc.WriteRaw(unclassifiableBytes(64))
		_, _ = sc.SendServerRequest(methodTeardown, "rtsp://x/stream", nil)
		drainRequests(sc)
	}})

	ctx := context.Background()
	c, err := rtsp.Dial(ctx, rtsp.Config{URL: s.URL("/stream"), Timeout: testTimeout})
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
//
// The session here never reaches Setup, so no channel is bound and the resync
// gate vouches for none of those latches. The play-side tests cover the other
// side: once a channel IS bound, a frame on it does re-lock.
func TestResyncBudgetIsBoundedDespiteInterleavedLatch(t *testing.T) {
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		serve(t, sc, methodOptions, 200, "OK", publicHeader(), nil)
		junk := unclassifiableBytes(3 * 4096)
		// Latches start after the first byte so the reader is already
		// resynchronizing when it meets one. A '$' as the very first byte is
		// an in-sync frame start by protocol, not a latch.
		for i := 200; i < len(junk); i += 200 {
			junk[i] = '$'
		}
		_ = sc.WriteRaw(junk)
		drainRequests(sc)
	}})

	ctx := context.Background()
	c, err := rtsp.Dial(ctx, rtsp.Config{URL: s.URL("/stream"), Timeout: testTimeout})
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

func TestMidStreamServerOptions(t *testing.T) {
	type answer struct {
		code     int
		gotCSeq  int
		wantCSeq int
		err      error
	}
	answerCh := make(chan answer, 1)

	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		serve(t, sc, methodOptions, 200, "OK", publicHeader(), nil)
		wantCSeq, err := sc.SendServerRequest(methodOptions, "rtsp://camera/stream", nil)
		if err != nil {
			answerCh <- answer{err: err}
			return
		}
		resp, err := sc.ReadResponse()
		if err != nil {
			answerCh <- answer{err: err}
			return
		}
		answerCh <- answer{code: resp.StatusCode, gotCSeq: resp.CSeq, wantCSeq: wantCSeq}
		// Prove the session is still alive by ending it with a TEARDOWN.
		_, _ = sc.SendServerRequest(methodTeardown, "rtsp://camera/stream", nil)
		drainRequests(sc)
	}})

	ctx := context.Background()
	c, err := rtsp.Dial(ctx, rtsp.Config{URL: s.URL("/stream"), Timeout: testTimeout})
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
	waitCtx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	if err := c.Wait(waitCtx); !errors.Is(err, rtsp.ErrServerTeardown) {
		t.Errorf("Wait = %v, want ErrServerTeardown (session should survive the mid-stream OPTIONS)", err)
	}
}

func TestUnknownCSeqResponseDropped(t *testing.T) {
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		serve(t, sc, methodOptions, 200, "OK", publicHeader(), nil)
		// A stray response no caller awaits must be dropped, not fatal. It
		// carries a CSeq no request produced, which only SendResponse expresses.
		_ = sc.SendResponse(200, 9999)
		_, _ = sc.SendServerRequest(methodTeardown, "rtsp://camera/stream", nil)
		drainRequests(sc)
	}})

	ctx := context.Background()
	c, err := rtsp.Dial(ctx, rtsp.Config{URL: s.URL("/stream"), Timeout: testTimeout})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	// Reaching the TEARDOWN proves the stray response neither terminated the
	// session nor desynchronized the framing loop.
	waitCtx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	if err := c.Wait(waitCtx); !errors.Is(err, rtsp.ErrServerTeardown) {
		t.Errorf("Wait = %v, want ErrServerTeardown (stray response must be dropped)", err)
	}
}

func TestReadIdleWatchdogDisabledPrePlay(t *testing.T) {
	// With ReadIdle set but no PLAY yet, the pre-play phase must not apply the
	// watchdog: an idle idle-state session stays alive until closed.
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		serve(t, sc, methodOptions, 200, "OK", publicHeader(), nil)
		// Go idle: never send anything. The client must not time out pre-play.
		drainRequests(sc)
	}})

	ctx := context.Background()
	c, err := rtsp.Dial(ctx, rtsp.Config{URL: s.URL("/stream"), Timeout: testTimeout, ReadIdle: 50 * time.Millisecond})
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
		s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
			req, err := sc.ReadRequest()
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
				Method: methodTeardown, URL: "rtsp://x/stream", CSeq: 1,
			})
			if err != nil {
				t.Errorf("MarshalRequest: %v", err)
				return
			}
			// One write, so the reader parses both units from one fill.
			_ = sc.WriteRaw(append(resp, teardown...))
			drainRequests(sc)
		}})

		ctx := context.Background()
		c, err := rtsp.Dial(ctx, rtsp.Config{URL: s.URL("/stream"), Timeout: testTimeout})
		if err != nil {
			t.Fatalf("iteration %d: Dial = %v, want success (the server answered 200)", i, err)
		}
		_ = c.Close()
		waitCtx, cancel := context.WithTimeout(ctx, testTimeout)
		_ = c.Wait(waitCtx)
		cancel()
	}
}

func TestBlockedWriteShutdownIsPrompt(t *testing.T) {
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		if _, err := sc.Handshake(testserver.HandshakeConfig{
			SDP:            opusSDP,
			SessionID:      testSessionID,
			SessionTimeout: 2, // keepalive interval = 1s, so the timer contends for writeMu
		}); err != nil {
			return
		}
		// Stop reading and flood the client with server-initiated requests so
		// its send buffer fills and a reader or timer write blocks under
		// writeMu with a future write deadline armed.
		for {
			if _, err := sc.SendServerRequest(methodOptions, "rtsp://camera/stream", nil); err != nil {
				return
			}
		}
	}})
	c, err := rtsp.Dial(context.Background(), rtsp.Config{
		URL: s.URL("/stream"),
		// A large Timeout is the whole point: if the deadlineMu/shuttingDown
		// write gate failed, a blocked write would park teardown for this long.
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	describeSetupPlay(t, c, nil)

	// Let the flood fill the socket buffer and a keepalive fire and contend for
	// writeMu behind the buffer-blocked reader write.
	time.Sleep(1200 * time.Millisecond)

	start := time.Now()
	_ = c.Close()
	waitErr := c.Wait(context.Background())
	elapsed := time.Since(start)
	if !errors.Is(waitErr, audiostream.ErrClosed) {
		t.Errorf("Wait = %v, want ErrClosed", waitErr)
	}
	// The write-side deadlineMu/shuttingDown gate must let initiateShutdown's
	// now-deadline unblock the buffer-blocked write so teardown is not parked
	// for Config.Timeout. The client goroutines all exit before Wait returns;
	// the testserver's own flood goroutine stays blocked on the half-dead
	// socket until t.Cleanup closes it, so a leak assertion here would measure
	// the server, not the client.
	if elapsed > 5*time.Second {
		t.Fatalf("shutdown took %v, want well under Config.Timeout (30s)", elapsed)
	}
}
