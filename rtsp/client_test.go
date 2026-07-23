package rtsp_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/internal/testserver"
	"github.com/tphakala/go-audio-stream/rtsp"
)

// testTimeout bounds every round-trip in these tests. It is generous enough
// to never fire on a healthy loopback yet short enough to fail fast.
const testTimeout = 2 * time.Second

// leakSettleTimeout bounds how long assertNoGoroutineLeak waits for the
// teardown window to drain. It is deliberately generous: it costs nothing on a
// passing run and only spends time when a leak is real, so a loaded or -race
// runner does not turn a slow teardown into a failure.
const leakSettleTimeout = 5 * time.Second

// serveOptionsThenIdle answers the Dial OPTIONS probe 200 with a Public
// header, then blocks reading until the client disconnects, so the handler
// goroutine exits cleanly when the client closes.
//
// The Public header advertises GET_PARAMETER deliberately. KeepaliveMethod
// returns "OPTIONS" for a nil or empty method list as well as for a list that
// omits GET_PARAMETER, so a handler that did not advertise it would let the
// assertion pass even if the Public header were never read at all.
func serveOptionsThenIdle(sc *testserver.ServerConn) {
	req, err := sc.ReadRequest()
	if err != nil {
		return
	}
	h := rtsp.Header{}
	h.Set("Public", "OPTIONS, DESCRIBE, SETUP, PLAY, TEARDOWN, GET_PARAMETER")
	if err := sc.Respond(req, 200, "OK", h, nil); err != nil {
		return
	}
	for {
		if _, err := sc.ReadRequest(); err != nil {
			return
		}
	}
}

// assertNoGoroutineLeak polls until the live goroutine count settles at or
// below baseline, dumping all stacks on failure. It has no external
// dependency: it samples runtime.NumGoroutine and waits out the brief teardown
// window.
//
// Callers must not be parallel: NumGoroutine is process-global, so a
// concurrently running test's goroutines would be counted against this
// baseline.
func assertNoGoroutineLeak(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(leakSettleTimeout)
	for {
		// No GC here: collection has nothing to do with goroutine termination,
		// and a stop-the-world per 10ms poll is pure cost on a loaded runner.
		n := runtime.NumGoroutine()
		if n <= baseline {
			return
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			buf = buf[:runtime.Stack(buf, true)]
			t.Fatalf("goroutine leak: have %d, baseline %d\n%s", n, baseline, buf)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDialOptions(t *testing.T) {
	s := testserver.New(t, testserver.Options{Handle: serveOptionsThenIdle})

	ctx := context.Background()
	c, err := rtsp.Dial(ctx, rtsp.Config{URL: s.URL("/stream"), Timeout: testTimeout})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	info := c.SessionInfo()
	// GET_PARAMETER is advertised, so this asserts the Public header was
	// actually parsed rather than restating KeepaliveMethod's default.
	if info.KeepaliveMethod != "GET_PARAMETER" {
		t.Errorf("KeepaliveMethod = %q, want GET_PARAMETER", info.KeepaliveMethod)
	}
	if info.SessionID != "" {
		t.Errorf("SessionID = %q, want empty before Setup", info.SessionID)
	}
	if info.AuthScheme != rtsp.AuthNone {
		t.Errorf("AuthScheme = %v, want AuthNone", info.AuthScheme)
	}

	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := c.Wait(ctx); !errors.Is(err, audiostream.ErrClosed) {
		t.Errorf("Wait = %v, want ErrClosed", err)
	}
}

func TestDialOptionsTolerated(t *testing.T) {
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		req, err := sc.ReadRequest()
		if err != nil {
			return
		}
		// A non-2xx OPTIONS (no Public header) must be tolerated by Dial.
		if err := sc.Respond(req, 501, "Not Implemented", nil, nil); err != nil {
			return
		}
		for {
			if _, err := sc.ReadRequest(); err != nil {
				return
			}
		}
	}})

	ctx := context.Background()
	c, err := rtsp.Dial(ctx, rtsp.Config{URL: s.URL("/stream"), Timeout: testTimeout})
	if err != nil {
		t.Fatalf("Dial with 501 OPTIONS: %v", err)
	}
	if got := c.SessionInfo().KeepaliveMethod; got != methodOptions {
		t.Errorf("KeepaliveMethod = %q, want OPTIONS default", got)
	}
	_ = c.Close()
	if err := c.Wait(ctx); !errors.Is(err, audiostream.ErrClosed) {
		t.Errorf("Wait = %v, want ErrClosed", err)
	}
}

func TestDialConnectionRefused(t *testing.T) {
	// Bind then release a port so the address is routable but refuses.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	baseline := runtime.NumGoroutine()
	ctx := context.Background()
	_, derr := rtsp.Dial(ctx, rtsp.Config{URL: "rtsp://" + addr + "/x", Timeout: 500 * time.Millisecond})
	if derr == nil {
		t.Fatal("Dial to refused port = nil error, want error")
	}
	assertNoGoroutineLeak(t, baseline)
}

func TestCloseIdempotent(t *testing.T) {
	s := testserver.New(t, testserver.Options{Handle: serveOptionsThenIdle})

	baseline := runtime.NumGoroutine()
	ctx := context.Background()
	c, err := rtsp.Dial(ctx, rtsp.Config{URL: s.URL("/stream"), Timeout: testTimeout})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if err := c.Wait(ctx); !errors.Is(err, audiostream.ErrClosed) {
		t.Errorf("Wait = %v, want ErrClosed", err)
	}
	assertNoGoroutineLeak(t, baseline)
}

func TestWaitContextCancel(t *testing.T) {
	s := testserver.New(t, testserver.Options{Handle: serveOptionsThenIdle})

	baseline := runtime.NumGoroutine()
	c, err := rtsp.Dial(context.Background(), rtsp.Config{URL: s.URL("/stream"), Timeout: testTimeout})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if err := c.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Wait = %v, want context.Canceled", err)
	}
	assertNoGoroutineLeak(t, baseline)
}

func TestServerTeardownEndsWait(t *testing.T) {
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		req, err := sc.ReadRequest()
		if err != nil {
			return
		}
		if err := sc.Respond(req, 200, "OK", nil, nil); err != nil {
			return
		}
		if _, err := sc.SendServerRequest("TEARDOWN", "rtsp://server/stream", nil); err != nil {
			return
		}
		// Leave the connection briefly so the TEARDOWN is delivered before close.
		for {
			if _, err := sc.ReadRequest(); err != nil {
				return
			}
		}
	}})

	ctx := context.Background()
	c, err := rtsp.Dial(ctx, rtsp.Config{URL: s.URL("/stream"), Timeout: testTimeout})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if err := c.Wait(ctx); !errors.Is(err, rtsp.ErrServerTeardown) {
		t.Errorf("Wait = %v, want ErrServerTeardown", err)
	}
}

func TestAbruptDisconnect(t *testing.T) {
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		req, err := sc.ReadRequest()
		if err != nil {
			return
		}
		if err := sc.Respond(req, 200, "OK", nil, nil); err != nil {
			return
		}
		_ = sc.Close()
	}})

	ctx := context.Background()
	c, err := rtsp.Dial(ctx, rtsp.Config{URL: s.URL("/stream"), Timeout: testTimeout})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if err := c.Wait(ctx); !errors.Is(err, rtsp.ErrConnectionClosed) {
		t.Errorf("Wait = %v, want ErrConnectionClosed", err)
	}
}

func TestStatsEmptyBeforeSetup(t *testing.T) {
	s := testserver.New(t, testserver.Options{Handle: serveOptionsThenIdle})

	ctx := context.Background()
	c, err := rtsp.Dial(ctx, rtsp.Config{URL: s.URL("/stream"), Timeout: testTimeout})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	st := c.Stats()
	if st.Tracks == nil {
		t.Error("Stats().Tracks is nil, want a non-nil empty map")
	}
	if len(st.Tracks) != 0 {
		t.Errorf("Stats().Tracks has %d entries, want 0", len(st.Tracks))
	}
	_ = c.Close()
	_ = c.Wait(ctx)
}

func TestDialTLS(t *testing.T) {
	s := testserver.New(t, testserver.Options{TLS: true, Handle: serveOptionsThenIdle})

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(s.CertPEM()) {
		t.Fatal("AppendCertsFromPEM: no cert added")
	}
	ctx := context.Background()
	c, err := rtsp.Dial(ctx, rtsp.Config{
		URL:     s.URL("/stream"),
		Timeout: testTimeout,
		TLSConfig: &tls.Config{
			RootCAs:    pool,
			ServerName: "127.0.0.1",
			MinVersion: tls.VersionTLS12,
		},
	})
	if err != nil {
		t.Fatalf("Dial rtsps: %v", err)
	}
	_ = c.Close()
	if err := c.Wait(ctx); !errors.Is(err, audiostream.ErrClosed) {
		t.Errorf("Wait = %v, want ErrClosed", err)
	}
}

func TestDialMarshalErrorNoHang(t *testing.T) {
	s := testserver.New(t, testserver.Options{Handle: serveOptionsThenIdle})

	baseline := runtime.NumGoroutine()
	// A path that pushes the request URI past rtsp.MaxRequestURILen (2048)
	// makes MarshalRequest fail inside Dial's OPTIONS round-trip. That path
	// runs before the pending entry and any write, so if it did not funnel
	// shutdown, closing/done would never close and Dial would block forever.
	longPath := "/" + strings.Repeat("a", 2100)

	done := make(chan error, 1)
	go func() {
		_, err := rtsp.Dial(context.Background(), rtsp.Config{URL: s.URL(longPath), Timeout: testTimeout})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Dial with oversize request URI = nil error, want a marshal error")
		}
		if !errors.Is(err, rtsp.ErrInvalidRequest) {
			t.Errorf("Dial error = %v, want ErrInvalidRequest", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Dial hung on an oversize request URI: the marshal-error path did not funnel shutdown")
	}
	assertNoGoroutineLeak(t, baseline)
}

func TestGoroutineLeakBaseline(t *testing.T) {
	s := testserver.New(t, testserver.Options{Handle: serveOptionsThenIdle})

	baseline := runtime.NumGoroutine()
	ctx := context.Background()
	c, err := rtsp.Dial(ctx, rtsp.Config{URL: s.URL("/stream"), Timeout: testTimeout})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	_ = c.Close()
	if err := c.Wait(ctx); !errors.Is(err, audiostream.ErrClosed) {
		t.Errorf("Wait = %v, want ErrClosed", err)
	}
	assertNoGoroutineLeak(t, baseline)
}

// InsecureTLS is a security-relevant opt-in that was previously never
// exercised: TestDialTLS always supplies its own TLSConfig, so the entire
// nil-config default branch (where InsecureTLS and the TLS 1.2 floor live)
// never ran. A regression inverting the flag would have been undetectable.
func TestDialTLSInsecureSkipsVerification(t *testing.T) {
	s := testserver.New(t, testserver.Options{TLS: true, Handle: serveOptionsThenIdle})

	ctx := context.Background()
	c, err := rtsp.Dial(ctx, rtsp.Config{
		URL:         s.URL("/stream"),
		Timeout:     testTimeout,
		InsecureTLS: true,
	})
	if err != nil {
		t.Fatalf("Dial with InsecureTLS: %v", err)
	}
	_ = c.Close()
	if err := c.Wait(ctx); !errors.Is(err, audiostream.ErrClosed) {
		t.Errorf("Wait = %v, want ErrClosed", err)
	}
}

// The complement: without InsecureTLS and without a RootCAs pool, the
// self-signed certificate must be rejected. Together these two pin the flag's
// meaning in both directions.
func TestDialTLSVerifiesByDefault(t *testing.T) {
	s := testserver.New(t, testserver.Options{TLS: true, Handle: serveOptionsThenIdle})

	ctx := context.Background()
	_, err := rtsp.Dial(ctx, rtsp.Config{URL: s.URL("/stream"), Timeout: testTimeout})
	if err == nil {
		t.Fatal("Dial succeeded against a self-signed cert without InsecureTLS")
	}
	if !errors.Is(err, rtsp.ErrConnectionClosed) {
		t.Errorf("Dial error = %v, want it to wrap ErrConnectionClosed", err)
	}
}

// A caller-supplied TLSConfig is cloned and normalized: an empty ServerName is
// filled in from the URL host, so a config carrying only RootCAs still
// verifies. This is the branch TestDialTLS bypasses by setting ServerName.
func TestDialTLSFillsServerNameFromURL(t *testing.T) {
	s := testserver.New(t, testserver.Options{TLS: true, Handle: serveOptionsThenIdle})

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(s.CertPEM()) {
		t.Fatal("AppendCertsFromPEM: no cert added")
	}
	ctx := context.Background()
	c, err := rtsp.Dial(ctx, rtsp.Config{
		URL:       s.URL("/stream"),
		Timeout:   testTimeout,
		TLSConfig: &tls.Config{RootCAs: pool},
	})
	if err != nil {
		t.Fatalf("Dial with only RootCAs set: %v", err)
	}
	_ = c.Close()
	_ = c.Wait(ctx)
}
