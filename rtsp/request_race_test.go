package rtsp

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// scriptedConn is a net.Conn whose Write hook can drive the reader goroutine
// to completion before it returns, which is what makes the roundTrip
// delivered-response race reproducible instead of a 1-in-10000 flake.
type scriptedConn struct {
	mu       sync.Mutex
	cond     *sync.Cond
	inbound  []byte
	closed   bool
	onWrite  func(sc *scriptedConn, p []byte)
	writeErr error
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
	for len(sc.inbound) == 0 && !sc.closed {
		sc.cond.Wait()
	}
	if len(sc.inbound) == 0 {
		return 0, io.EOF
	}
	n := copy(p, sc.inbound)
	sc.inbound = sc.inbound[n:]
	return n, nil
}

func (sc *scriptedConn) Write(p []byte) (int, error) {
	if sc.onWrite != nil {
		sc.onWrite(sc, p)
	}
	if sc.writeErr != nil {
		return 0, sc.writeErr
	}
	return len(p), nil
}

func (sc *scriptedConn) Close() error {
	sc.eof()
	return nil
}
func (sc *scriptedConn) LocalAddr() net.Addr                { return dummyAddr{} }
func (sc *scriptedConn) RemoteAddr() net.Addr               { return dummyAddr{} }
func (sc *scriptedConn) SetDeadline(_ time.Time) error      { return nil }
func (sc *scriptedConn) SetReadDeadline(_ time.Time) error  { return nil }
func (sc *scriptedConn) SetWriteDeadline(_ time.Time) error { return nil }

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
				return
			}
			raw, err := MarshalResponse(&Response{StatusCode: StatusOK, Reason: "OK", CSeq: req.CSeq})
			if err != nil {
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
	cfg := Config{URL: scriptedURL, Timeout: 50 * time.Millisecond}
	cfg.applyDefaults()

	conn := newScriptedConn(nil)
	c := newClient(&cfg, conn, &target{requestURL: scriptedURL})
	conn.onWrite = func(sc *scriptedConn, p []byte) {
		req, _, err := ParseRequest(p)
		if err != nil {
			return
		}
		raw, err := MarshalResponse(&Response{StatusCode: StatusOK, Reason: "OK", CSeq: req.CSeq})
		if err != nil {
			return
		}
		sc.deliver(raw)
		// Let the reader dispatch it, then outlast the request timer so both
		// the response and the timer are ready at the select.
		time.Sleep(120 * time.Millisecond)
	}
	go c.reader()

	resp, err := c.roundTrip(context.Background(), &Request{Method: methodOptions, URL: scriptedURL})
	if err != nil {
		t.Fatalf("roundTrip = %v, want the delivered response", err)
	}
	if resp.StatusCode != StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, StatusOK)
	}
	_ = c.Close()
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
