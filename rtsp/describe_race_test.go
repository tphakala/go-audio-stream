package rtsp

import (
	"errors"
	"testing"
	"time"
)

// minimalAudioSDP is the smallest DESCRIBE body resolveTracks accepts: one
// audio media section with a control attribute. It is enough to carry Describe
// past resolveTracks and into the commit window this test targets.
const minimalAudioSDP = "v=0\r\n" +
	"o=- 0 0 IN IP4 127.0.0.1\r\n" +
	"s=Stream\r\n" +
	"m=audio 0 RTP/AVP 0\r\n" +
	"a=control:audio\r\n"

// TestDescribeObservesTerminalErrorMidShutdown drives the guard in Describe
// that rejects a DESCRIBE whose round trip already succeeded when the reader
// has meanwhile begun shutdown. The DESCRIBE completes on the wire and
// resolveTracks yields tracks, but a terminal error lands in the narrow window
// between the round trip returning and Describe re-acquiring mu to commit
// (a server TEARDOWN or a dropped connection is what does this for real).
// Describe must surface that terminal error and must not commit the described
// state.
//
// That window is otherwise unreachable deterministically, so the test installs
// afterDescribeRoundTrip, the test-only seam that fires at exactly that point,
// and uses it to record the terminal error the way the reader does: under mu,
// first cause wins, via setTermErr. Setting it synchronously in the hook makes
// the race the guard defends against happen on every run instead of once in
// many, so the branch is covered without a flake.
func TestDescribeObservesTerminalErrorMidShutdown(t *testing.T) {
	cfg := Config{URL: scriptedURL, Timeout: 2 * time.Second}
	cfg.applyDefaults()

	conn := newScriptedConn(nil)
	c := newClient(&cfg, conn, &target{requestURL: scriptedURL})

	// Answer the DESCRIBE with a valid application/sdp body so the round trip
	// succeeds and Describe reaches the commit window rather than bailing out
	// earlier on a content-type or parse error.
	conn.onWrite = func(sc *scriptedConn, p []byte) {
		req, _, err := ParseRequest(p)
		if err != nil {
			t.Errorf("ParseRequest in hook: %v", err)
			return
		}
		h := Header{}
		h.Set("Content-Type", sdpContentType)
		raw, merr := MarshalResponse(&Response{
			StatusCode: StatusOK,
			Reason:     "OK",
			CSeq:       req.CSeq,
			Header:     h,
			Body:       []byte(minimalAudioSDP),
		})
		if merr != nil {
			t.Errorf("MarshalResponse in hook: %v", merr)
			return
		}
		sc.deliver(raw)
	}

	sentinel := errors.New("reader won the mid-Describe race")
	// Trip termErr in the exact window the guard defends, the way the reader
	// does: under mu, first cause wins. The hook runs synchronously in
	// Describe's own goroutine, so the guard observes it deterministically.
	c.afterDescribeRoundTrip = func() {
		c.setTermErr(sentinel)
	}

	go c.reader()

	_, err := c.Describe(t.Context())
	if !errors.Is(err, sentinel) {
		t.Fatalf("Describe = %v, want the terminal error observed mid-shutdown", err)
	}

	// The described state must not be committed: a Describe that lost this race
	// leaves nothing behind for Setup to read and does not advance the state.
	c.mu.Lock()
	described := c.described
	state := c.state
	c.mu.Unlock()
	if described != nil {
		t.Errorf("described state committed despite the terminal error: %+v", described)
	}
	if state == stateDescribed {
		t.Error("state advanced to described despite the terminal error")
	}

	_ = c.Close()
	<-c.done
}
