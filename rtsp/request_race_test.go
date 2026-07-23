package rtsp

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

// scriptedConn is a net.Conn whose Write hook can drive the reader goroutine
// to completion before it returns, which is what makes the roundTrip
// delivered-response race reproducible instead of a 1-in-10000 flake.
type scriptedConn struct {
	mu      sync.Mutex
	cond    *sync.Cond
	inbound []byte
	closed  bool
	onWrite func(sc *scriptedConn, p []byte)

	// readDeadline is honoured rather than ignored. A no-op SetReadDeadline
	// makes Client.Close unable to wake a reader parked in Read (Close relies
	// on the deadline interrupt; the socket is closed later by the reader
	// itself), so every test using this conn would leak its reader goroutine
	// forever and any later goroutine-leak assertion would fail with a
	// confusing stack.
	readDeadline time.Time
	deadlineTmr  *time.Timer

	// writeDeadline is honoured too. A no-op SetWriteDeadline hides the whole
	// class of bug where a caller arms an already-expired write deadline: a
	// real poller refuses the write outright, a permissive fake writes anyway,
	// and the test reports success for bytes that would never reach a socket.
	writeDeadline time.Time
}

func newScriptedConn(onWrite func(sc *scriptedConn, p []byte)) *scriptedConn {
	sc := &scriptedConn{onWrite: onWrite}
	sc.cond = sync.NewCond(&sc.mu)
	return sc
}

// deliver queues bytes for the reader to consume.
func (sc *scriptedConn) deliver(b []byte) {
	sc.mu.Lock()
	sc.inbound = append(sc.inbound, b...)
	sc.mu.Unlock()
	sc.cond.Broadcast()
}

// eof makes the next read return io.EOF once the queued bytes are drained.
func (sc *scriptedConn) eof() {
	sc.mu.Lock()
	sc.closed = true
	sc.mu.Unlock()
	sc.cond.Broadcast()
}

func (sc *scriptedConn) Read(p []byte) (int, error) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	for {
		if len(sc.inbound) > 0 {
			n := copy(p, sc.inbound)
			sc.inbound = sc.inbound[n:]
			return n, nil
		}
		if sc.closed {
			return 0, io.EOF
		}
		if !sc.readDeadline.IsZero() && !time.Now().Before(sc.readDeadline) {
			return 0, os.ErrDeadlineExceeded
		}
		sc.cond.Wait()
	}
}

func (sc *scriptedConn) Write(p []byte) (int, error) {
	sc.mu.Lock()
	expired := !sc.writeDeadline.IsZero() && !time.Now().Before(sc.writeDeadline)
	sc.mu.Unlock()
	if expired {
		return 0, os.ErrDeadlineExceeded
	}
	if sc.onWrite != nil {
		sc.onWrite(sc, p)
	}
	return len(p), nil
}

func (sc *scriptedConn) Close() error {
	sc.eof()
	return nil
}
func (sc *scriptedConn) LocalAddr() net.Addr  { return dummyAddr{} }
func (sc *scriptedConn) RemoteAddr() net.Addr { return dummyAddr{} }
func (sc *scriptedConn) SetDeadline(t time.Time) error {
	_ = sc.SetWriteDeadline(t)
	return sc.SetReadDeadline(t)
}

// SetReadDeadline wakes a parked Read when the deadline is already past, and
// schedules a wake for a future one, so the shutdown interrupt behaves the way
// a real socket's does.
func (sc *scriptedConn) SetReadDeadline(t time.Time) error {
	sc.mu.Lock()
	sc.readDeadline = t
	if sc.deadlineTmr != nil {
		sc.deadlineTmr.Stop()
		sc.deadlineTmr = nil
	}
	if d := time.Until(t); !t.IsZero() && d > 0 {
		sc.deadlineTmr = time.AfterFunc(d, func() { sc.cond.Broadcast() })
	}
	sc.mu.Unlock()
	sc.cond.Broadcast()
	return nil
}

func (sc *scriptedConn) SetWriteDeadline(t time.Time) error {
	sc.mu.Lock()
	sc.writeDeadline = t
	sc.mu.Unlock()
	return nil
}

// scriptedURL is the target every scripted-conn test dials. The conn is a fake,
// so the value only has to be a well-formed rtsp URL.
const scriptedURL = "rtsp://scripted/stream"

type dummyAddr struct{}

func (dummyAddr) Network() string { return "scripted" }
func (dummyAddr) String() string  { return "scripted" }

// A response that was actually delivered must win over every terminal branch
// of roundTrip's select.
//
// The Write hook feeds the response and then an EOF, and does not return until
// the reader has finished and closed done. So when roundTrip reaches its
// select, the pending channel holds a valid response AND done is closed: both
// cases are ready, and select picks uniformly at random. Without the
// non-blocking drain in each terminal branch, this returns the shutdown cause
// about half the time and discards a fully parsed 200 response.
func TestRoundTripPrefersDeliveredResponseOverShutdown(t *testing.T) {
	const iterations = 60
	for i := range iterations {
		cfg := Config{URL: scriptedURL, Timeout: 2 * time.Second}
		cfg.applyDefaults()

		conn := newScriptedConn(nil)
		c := newClient(&cfg, conn, &target{requestURL: scriptedURL})

		conn.onWrite = func(sc *scriptedConn, p []byte) {
			req, _, err := ParseRequest(p)
			if err != nil {
				t.Errorf("iteration %d: ParseRequest in hook: %v", i, err)
				return
			}
			raw, err := MarshalResponse(&Response{StatusCode: StatusOK, Reason: "OK", CSeq: req.CSeq})
			if err != nil {
				t.Errorf("iteration %d: MarshalResponse in hook: %v", i, err)
				return
			}
			sc.deliver(raw)
			sc.eof()
			// Block until the reader has dispatched the response and closed
			// done, so the caller returns from Write into a select where both
			// cases are already ready.
			<-c.done
		}

		go c.reader()

		ctx := context.Background()
		resp, err := c.roundTrip(ctx, &Request{Method: methodOptions, URL: scriptedURL})
		if err != nil {
			t.Fatalf("iteration %d: roundTrip = %v, want the delivered 200 response", i, err)
		}
		if resp.StatusCode != StatusOK {
			t.Fatalf("iteration %d: status = %d, want %d", i, resp.StatusCode, StatusOK)
		}
	}
}

// The same preference applies when the request times out: a response that
// landed before the timer fired is the answer, not a timeout.
func TestRoundTripPrefersDeliveredResponseOverTimeout(t *testing.T) {
	// Looped for the same reason the shutdown variant is: with both the
	// response and the timer ready, select picks uniformly, so a single run
	// catches a reverted drain only half the time. A regression test that
	// misses its regression on every other run is worse than none, because a
	// green run reads as proof.
	const iterations = 30
	for i := range iterations {
		cfg := Config{URL: scriptedURL, Timeout: 20 * time.Millisecond}
		cfg.applyDefaults()

		conn := newScriptedConn(nil)
		c := newClient(&cfg, conn, &target{requestURL: scriptedURL})
		delivered := make(chan struct{})
		conn.onWrite = func(sc *scriptedConn, p []byte) {
			req, _, err := ParseRequest(p)
			if err != nil {
				t.Errorf("iteration %d: ParseRequest in hook: %v", i, err)
				return
			}
			raw, err := MarshalResponse(&Response{StatusCode: StatusOK, Reason: "OK", CSeq: req.CSeq})
			if err != nil {
				t.Errorf("iteration %d: MarshalResponse in hook: %v", i, err)
				return
			}
			sc.deliver(raw)
			// Wait for the reader to actually dispatch it, then outlast the
			// request timer so both cases are ready at the select. Waiting on
			// the signal rather than sleeping keeps the loop fast.
			<-delivered
			time.Sleep(30 * time.Millisecond)
		}
		go func() {
			// The reader dispatches into the pending table; give it the
			// go-ahead once it has consumed the delivered bytes.
			for {
				conn.mu.Lock()
				empty := len(conn.inbound) == 0
				conn.mu.Unlock()
				if empty {
					close(delivered)
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
		go c.reader()

		resp, err := c.roundTrip(context.Background(), &Request{Method: methodOptions, URL: scriptedURL})
		if err != nil {
			t.Fatalf("iteration %d: roundTrip = %v, want the delivered response", i, err)
		}
		if resp.StatusCode != StatusOK {
			t.Fatalf("iteration %d: status = %d, want %d", i, resp.StatusCode, StatusOK)
		}
		_ = c.Close()
	}
}

// A genuine timeout with no response delivered still reports ErrRequestTimeout,
// so the drain above cannot mask a real stall.
func TestRoundTripTimeoutIsMatchable(t *testing.T) {
	cfg := Config{URL: scriptedURL, Timeout: 30 * time.Millisecond}
	cfg.applyDefaults()

	conn := newScriptedConn(nil) // never answers
	c := newClient(&cfg, conn, &target{requestURL: scriptedURL})
	go c.reader()

	_, err := c.roundTrip(context.Background(), &Request{Method: methodOptions, URL: scriptedURL})
	if !errors.Is(err, ErrRequestTimeout) {
		t.Fatalf("roundTrip = %v, want ErrRequestTimeout", err)
	}
	_ = c.Close()
}

// The best-effort TEARDOWN must actually reach the wire.
//
// sendTeardownBestEffort runs only from the terminal sequence, which runs only
// after initiateShutdown, so shuttingDown is always set by then. Routing its
// write through the ordinary guarded helper therefore armed an
// already-expired deadline and the poller refused the write outright: the
// TEARDOWN was silently never sent, and the discarded write error hid it.
func TestTeardownIsActuallyWritten(t *testing.T) {
	cfg := Config{URL: scriptedURL, Timeout: time.Second}
	cfg.applyDefaults()

	var mu sync.Mutex
	var written [][]byte
	conn := newScriptedConn(nil)
	conn.onWrite = func(_ *scriptedConn, p []byte) {
		mu.Lock()
		written = append(written, append([]byte(nil), p...))
		mu.Unlock()
	}

	c := newClient(&cfg, conn, &target{requestURL: scriptedURL})
	// A session id is what gates the teardown; Setup will set this for real.
	c.mu.Lock()
	c.sessionID = testInternalSessionID
	c.mu.Unlock()

	go c.reader()
	_ = c.Close()
	<-c.done

	mu.Lock()
	defer mu.Unlock()
	var sawTeardown bool
	for _, w := range written {
		if req, _, err := ParseRequest(w); err == nil && req.Method == methodTeardown {
			sawTeardown = true
		}
	}
	if !sawTeardown {
		t.Fatalf("no TEARDOWN reached the connection; writes = %d", len(written))
	}
}
