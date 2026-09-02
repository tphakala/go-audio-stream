package hlssource

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestTeardownClosesIdleConnections pins the fix for the idle-connection leak.
// The HLS transport deliberately keeps connections alive across a session's many
// small segment fetches, so when the stream ends and the Client is torn down its
// idle keep-alive sockets must be closed rather than stranded. Without teardown
// calling CloseIdleConnections the pooled connection lingers idle (until the 90s
// IdleConnTimeout or the origin closes it), and under supervisor's
// reconnect-per-attempt Factory a flapping source would accumulate one abandoned
// pool per attempt, each pinned against GC by its readLoop/writeLoop pair.
//
// The assertion is behavioral and origin-observed: the server records when a
// client connection reaches http.StateClosed, and after Wait returns the test
// polls for that close. Buggy code never closes within the poll window so the
// test fails; the fix closes promptly. Polling rather than asserting an instant
// transition keeps it robust on platforms (Windows CI) where TCP teardown is not
// synchronous.
func TestTeardownClosesIdleConnections(t *testing.T) {
	stream, _ := adtsStream(2, 40)
	seg := buildTSSegment(stream, 0x1000, 0x0100)
	segs := map[string][]byte{tsSegURL0: seg}
	body := buildMediaPlaylist(1, 0, true, []segSpec{{uri: tsSegRel0, duration: 1.0}})

	var mu sync.Mutex
	closed := 0
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b, ok := segs[r.URL.Path]; ok {
			w.Header().Set("Content-Type", "video/mp2t")
			_, _ = w.Write(b)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write([]byte(body))
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			mu.Lock()
			closed++
			mu.Unlock()
		}
	}
	srv.Start()
	defer srv.Close()

	c, err := Open(context.Background(), Config{URL: srv.URL + "/vod.m3u8"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if werr := c.Wait(context.Background()); !errors.Is(werr, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", werr)
	}

	// Wait returning means reader() has returned and its deferred teardown ran, so
	// CloseIdleConnections has already been called on the transport. The
	// client-side close still propagates to the origin asynchronously, so poll for
	// the observed StateClosed rather than requiring it instantly. The server is
	// only closed by the deferred srv.Close() after this poll returns, so any
	// StateClosed seen here is client-initiated, not server teardown.
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := closed
		mu.Unlock()
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("origin never observed the client closing its idle keep-alive connection after teardown")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestOpenFailureClosesIdleConnections is the sibling of the teardown reap: if
// Open fails AFTER establishing a keep-alive connection (the playlist fetched
// successfully but openHandshake then failed, here because the playlist declares
// no playable segment), the idle socket left in the pool must be closed before
// Open returns its error. The reader teardown never runs on this path, since the
// reader goroutine is only started once Open succeeds. Without the reap in Open's
// error defer, supervisor's reconnect-per-attempt Factory would strand one idle
// socket per failed attempt until IdleConnTimeout (90s) or the origin closes it.
func TestOpenFailureClosesIdleConnections(t *testing.T) {
	// A valid media playlist that parses but declares no segment: the playlist GET
	// succeeds (connection established and returned to the idle pool), then
	// openHandshake fails at firstPlayable. No segment fetch is attempted.
	body := "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-ENDLIST\n"

	var mu sync.Mutex
	closed := 0
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write([]byte(body))
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			mu.Lock()
			closed++
			mu.Unlock()
		}
	}
	srv.Start()
	defer srv.Close()

	if _, err := Open(context.Background(), Config{URL: srv.URL + "/vod.m3u8"}); err == nil {
		t.Fatal("Open succeeded, want an error for a playlist with no playable segment")
	}

	// Same poll rationale as TestTeardownClosesIdleConnections: the client-side
	// close reaches the origin asynchronously, and srv.Close() runs only after this
	// returns, so any StateClosed observed here is client-initiated.
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := closed
		mu.Unlock()
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("origin never observed the client closing its idle connection after a failed Open")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
