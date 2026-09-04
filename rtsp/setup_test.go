package rtsp_test

import (
	"errors"
	"testing"
	"time"

	"github.com/tphakala/go-audio-stream/internal/testserver"
	"github.com/tphakala/go-audio-stream/rtsp"
)

// describeOne dials, answers OPTIONS/DESCRIBE with the given SDP, and returns
// the client and the resolved tracks, leaving the handler in setupFn to script
// the SETUP exchange. setupFn runs on the server goroutine after DESCRIBE.
func describeOne(t *testing.T, sdp string, setupFn func(sc *testserver.ServerConn)) (*rtsp.Client, []rtsp.Track) {
	t.Helper()
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		// keepaliveHeader, not publicHeader: it advertises GET_PARAMETER, which is
		// what makes the KeepaliveMethod assertion below discriminating.
		serve(t, sc, methodOptions, 200, "OK", keepaliveHeader(), nil)
		serve(t, sc, methodDescribe, 200, "OK", sdpHeaders(""), []byte(sdp))
		setupFn(sc)
	}})
	c := dialIdle(t, s.URL("/stream"))
	// The caller registers closeAndWait only after this returns, so without a
	// cleanup of our own a Fatalf below would strand the client, its reader
	// goroutine and its connection for the rest of the binary, where
	// assertNoGoroutineLeak would blame an unrelated test.
	t.Cleanup(func() { _ = c.Close() })
	tracks, err := c.Describe(t.Context())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	return c, tracks
}

// answerSetup reads one SETUP and answers it with the given interleaved pair
// and the shared session id, after asserting the pair the client PROPOSED.
//
// Checking the proposal matters because everything else in these tests observes
// only what the client accepted back, so the allocation logic could propose a
// constant 0-1 for every track, or a pair outside the one-byte channel range,
// with the suite still green.
func answerSetup(t *testing.T, sc *testserver.ServerConn, proposeRTP, proposeRTCP, rtpCh, rtcpCh int) {
	t.Helper()
	req := serve(t, sc, methodSetup, 200, "OK", setupHeaders(rtpCh, rtcpCh, testSessionID, testTimeoutS), nil)
	if req == nil {
		return
	}
	if got, want := req.Header.Get("Transport"), rtsp.BuildTransport(proposeRTP, proposeRTCP); got != want {
		t.Errorf("SETUP proposed Transport %q, want %q", got, want)
	}
}

// captureTeardown reads the next server request (the per-track TEARDOWN a client
// sends for a stream whose echoed Transport it rejected), records it as
// "METHOD URL" on ch, and answers it 200 OK so the client's TEARDOWN round trip
// completes.
func captureTeardown(t *testing.T, sc *testserver.ServerConn, ch chan<- string) {
	t.Helper()
	req, err := sc.ReadRequest()
	if err != nil {
		t.Errorf("reading the expected per-track TEARDOWN: %v", err)
		return
	}
	ch <- req.Method + " " + req.URL + " Session=" + req.Header.Get("Session")
	_ = sc.Respond(req, 200, "OK", nil, nil)
}

// assertTeardown fails unless captureTeardown recorded a TEARDOWN addressed to
// wantControl and carrying wantSession within a short window. Checking the
// Session header guards the invariant that the fire-and-forget TEARDOWN carries
// the session recordSession stored before the transport parse, so a compliant
// server can scope the release to the one rejected stream.
func assertTeardown(t *testing.T, ch <-chan string, wantControl, wantSession string) {
	t.Helper()
	select {
	case got := <-ch:
		if want := methodTeardown + " " + wantControl + " Session=" + wantSession; got != want {
			t.Errorf("server received %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Error("server received no per-track TEARDOWN for the rejected stream")
	}
}

func TestSetupRenumberedChannels(t *testing.T) {
	c, tracks := describeOne(t, aacSDP, func(sc *testserver.ServerConn) {
		answerSetup(t, sc, 0, 1, 4, 5) // server renumbers away from the proposed 0-1
		drainRequests(sc)
	})
	defer closeAndWait(t, c)

	if err := c.Setup(t.Context(), tracks[0], rtsp.SetupOptions{}); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	chans := c.SessionInfo().Channels
	if len(chans) != 1 {
		t.Fatalf("Channels = %v, want one pair", chans)
	}
	if chans[0].RTP != 4 || chans[0].RTCP != 5 || chans[0].TrackID != 0 {
		t.Errorf("Channels[0] = %+v, want {TrackID:0 RTP:4 RTCP:5}", chans[0])
	}
}

func TestSetupSecondTrackChannelClaim(t *testing.T) {
	c, tracks := describeOne(t, audioVideoSDP, func(sc *testserver.ServerConn) {
		answerSetup(t, sc, 0, 1, 0, 1)
		answerSetup(t, sc, 2, 3, 2, 3)
		drainRequests(sc)
	})
	defer closeAndWait(t, c)

	if err := c.Setup(t.Context(), tracks[0], rtsp.SetupOptions{}); err != nil {
		t.Fatalf("Setup track 0: %v", err)
	}
	if err := c.Setup(t.Context(), tracks[1], rtsp.SetupOptions{}); err != nil {
		t.Fatalf("Setup track 1: %v", err)
	}
	chans := c.SessionInfo().Channels
	if len(chans) != 2 {
		t.Fatalf("Channels = %v, want two pairs", chans)
	}
	if chans[1].RTP != 2 || chans[1].RTCP != 3 {
		t.Errorf("Channels[1] = %+v, want RTP:2 RTCP:3", chans[1])
	}
}

func TestSetupChannelConflict(t *testing.T) {
	teardownURL := make(chan string, 1)
	c, tracks := describeOne(t, audioVideoSDP, func(sc *testserver.ServerConn) {
		answerSetup(t, sc, 0, 1, 0, 1)
		// Second SETUP returns the same pair the first track already claimed.
		answerSetup(t, sc, 2, 3, 0, 1)
		// The client rejects that Transport and tears down the one stream the
		// server allocated for it, before Setup returns.
		captureTeardown(t, sc, teardownURL)
		drainRequests(sc)
	})
	defer closeAndWait(t, c)

	if err := c.Setup(t.Context(), tracks[0], rtsp.SetupOptions{}); err != nil {
		t.Fatalf("Setup track 0: %v", err)
	}
	err := c.Setup(t.Context(), tracks[1], rtsp.SetupOptions{})
	if !errors.Is(err, rtsp.ErrChannelConflict) {
		t.Fatalf("Setup track 1 = %v, want ErrChannelConflict", err)
	}
	// The rejected stream is released on the server so an aggregate PLAY cannot
	// stream it into track 0's depacketizer.
	assertTeardown(t, teardownURL, tracks[1].Control, testSessionID)
	// The session must survive a rejected Setup: the first track's exact channel
	// pair is still bound, untouched by track 1's teardown.
	chans := c.SessionInfo().Channels
	if len(chans) != 1 {
		t.Fatalf("Channels = %v, want the first track only", chans)
	}
	if chans[0].TrackID != 0 || chans[0].RTP != 0 || chans[0].RTCP != 1 {
		t.Errorf("Channels[0] = %+v, want {TrackID:0 RTP:0 RTCP:1}", chans[0])
	}
}

func TestSetupBadTransport(t *testing.T) {
	teardownURL := make(chan string, 1)
	c, tracks := describeOne(t, aacSDP, func(sc *testserver.ServerConn) {
		req, err := sc.ReadRequest()
		if err != nil {
			return
		}
		h := rtsp.Header{}
		h.Set("Transport", "RTP/AVP/TCP;unicast") // no interleaved pair
		h.Set("Session", sessionValue(testSessionID, testTimeoutS))
		_ = sc.Respond(req, 200, "OK", h, nil)
		captureTeardown(t, sc, teardownURL)
		drainRequests(sc)
	})
	defer closeAndWait(t, c)

	err := c.Setup(t.Context(), tracks[0], rtsp.SetupOptions{})
	if !errors.Is(err, rtsp.ErrNoInterleaved) {
		t.Fatalf("Setup = %v, want ErrNoInterleaved", err)
	}
	assertTeardown(t, teardownURL, tracks[0].Control, testSessionID)
}

// TestSetupMalformedTransportTearsDown covers the ParseTransport-failure branch
// (as opposed to InterleavedChannels): a 2xx with a Session but no Transport
// header. The client rejects with ErrMalformedTransport and must still release
// the stream the server allocated, leaving the session usable.
func TestSetupMalformedTransportTearsDown(t *testing.T) {
	teardownURL := make(chan string, 1)
	c, tracks := describeOne(t, aacSDP, func(sc *testserver.ServerConn) {
		req, err := sc.ReadRequest()
		if err != nil {
			return
		}
		h := rtsp.Header{}
		h.Set("Session", sessionValue(testSessionID, testTimeoutS))
		_ = sc.Respond(req, 200, "OK", h, nil) // no Transport header
		captureTeardown(t, sc, teardownURL)
		drainRequests(sc)
	})
	defer closeAndWait(t, c)

	err := c.Setup(t.Context(), tracks[0], rtsp.SetupOptions{})
	if !errors.Is(err, rtsp.ErrMalformedTransport) {
		t.Fatalf("Setup = %v, want ErrMalformedTransport", err)
	}
	assertTeardown(t, teardownURL, tracks[0].Control, testSessionID)
	if c.SessionInfo().SessionID != testSessionID {
		t.Errorf("SessionID = %q, want %q", c.SessionInfo().SessionID, testSessionID)
	}
}

// TestSetupRejectionKeepsSessionUsable proves the per-track TEARDOWN is truly
// best-effort. A noncompliant server that echoes an unbindable Transport may
// also ignore the TEARDOWN, and the release must not tear the session down
// waiting for a reply that never comes. After the rejection the session is
// still in SETUP state, so a lifecycle call sees ErrTrackAlreadySetUp (session
// alive), not ErrInvalidState (session torn down). A c.do-based teardown would
// instead block Config.Timeout on the unanswered reply and then shut the session
// down, so this fails against that implementation.
func TestSetupRejectionKeepsSessionUsable(t *testing.T) {
	c, tracks := describeOne(t, audioVideoSDP, func(sc *testserver.ServerConn) {
		answerSetup(t, sc, 0, 1, 0, 1) // track 0 binds
		answerSetup(t, sc, 2, 3, 0, 1) // track 1 conflict
		// Read the per-track TEARDOWN but deliberately never answer it.
		if _, err := sc.ReadRequest(); err != nil {
			return
		}
		drainRequests(sc)
	})
	defer closeAndWait(t, c)

	if err := c.Setup(t.Context(), tracks[0], rtsp.SetupOptions{}); err != nil {
		t.Fatalf("Setup track 0: %v", err)
	}
	if err := c.Setup(t.Context(), tracks[1], rtsp.SetupOptions{}); !errors.Is(err, rtsp.ErrChannelConflict) {
		t.Fatalf("Setup track 1 = %v, want ErrChannelConflict", err)
	}
	// The unanswered TEARDOWN must not have torn the session down: re-setting up
	// the bound track reports it is already set up, which only the live SETUP
	// state produces (a shut-down client returns ErrInvalidState instead).
	if err := c.Setup(t.Context(), tracks[0], rtsp.SetupOptions{}); !errors.Is(err, rtsp.ErrTrackAlreadySetUp) {
		t.Fatalf("re-Setup track 0 = %v, want ErrTrackAlreadySetUp (session alive)", err)
	}
}

func TestSetupStatusError(t *testing.T) {
	c, tracks := describeOne(t, aacSDP, func(sc *testserver.ServerConn) {
		serve(t, sc, methodSetup, 461, "Unsupported Transport", nil, nil)
		drainRequests(sc)
	})
	defer closeAndWait(t, c)

	err := c.Setup(t.Context(), tracks[0], rtsp.SetupOptions{})
	var re *rtsp.ResponseError
	if !errors.As(err, &re) {
		t.Fatalf("Setup = %v, want *ResponseError", err)
	}
	if re.Code != 461 {
		t.Errorf("ResponseError.Code = %d, want 461", re.Code)
	}
}

func TestSetupBeforeDescribe(t *testing.T) {
	s := testserver.New(t, testserver.Options{Handle: serveOptionsThenIdle})
	c := dialIdle(t, s.URL("/stream"))
	defer closeAndWait(t, c)

	err := c.Setup(t.Context(), rtsp.Track{ID: 0, Control: "rtsp://x/track0"}, rtsp.SetupOptions{})
	if _, ok := errors.AsType[*rtsp.StateError](err); !ok {
		t.Fatalf("Setup before Describe = %v, want *StateError", err)
	}
	if !errors.Is(err, rtsp.ErrInvalidState) {
		t.Errorf("Setup before Describe does not match ErrInvalidState")
	}
}

func TestSetupDiscardFlag(t *testing.T) {
	c, tracks := describeOne(t, aacSDP, func(sc *testserver.ServerConn) {
		answerSetup(t, sc, 0, 1, 0, 1)
		drainRequests(sc)
	})
	defer closeAndWait(t, c)

	if err := c.Setup(t.Context(), tracks[0], rtsp.SetupOptions{Discard: true}); err != nil {
		t.Fatalf("Setup with Discard: %v", err)
	}
	if len(c.SessionInfo().Channels) != 1 {
		t.Errorf("Channels = %v, want one pair for the discard track", c.SessionInfo().Channels)
	}
}

func TestSessionInfoAfterSetup(t *testing.T) {
	c, tracks := describeOne(t, audioVideoSDP, func(sc *testserver.ServerConn) {
		answerSetup(t, sc, 0, 1, 0, 1)
		answerSetup(t, sc, 2, 3, 2, 3)
		drainRequests(sc)
	})
	defer closeAndWait(t, c)

	if err := c.Setup(t.Context(), tracks[0], rtsp.SetupOptions{}); err != nil {
		t.Fatalf("Setup track 0: %v", err)
	}
	if err := c.Setup(t.Context(), tracks[1], rtsp.SetupOptions{}); err != nil {
		t.Fatalf("Setup track 1: %v", err)
	}

	info := c.SessionInfo()
	if info.SessionID != testSessionID {
		t.Errorf("SessionID = %q, want %q", info.SessionID, testSessionID)
	}
	if info.SessionTimeout != testTimeoutS*time.Second {
		t.Errorf("SessionTimeout = %v, want %ds", info.SessionTimeout, testTimeoutS)
	}
	// The fixture advertises GET_PARAMETER, so this fails if the Public header
	// is not actually parsed. Asserting OPTIONS instead would hold for a nil
	// list, an empty list, and a list that merely omits GET_PARAMETER.
	if info.KeepaliveMethod != methodGetParameter {
		t.Errorf("KeepaliveMethod = %q, want %s (advertised in Public)", info.KeepaliveMethod, methodGetParameter)
	}
	if info.AuthScheme != rtsp.AuthNone {
		t.Errorf("AuthScheme = %v, want AuthNone (this handshake is unauthenticated)", info.AuthScheme)
	}
	if len(info.Channels) != 2 {
		t.Fatalf("Channels = %v, want two pairs", info.Channels)
	}
	// Mutating the returned slice must not affect internal state.
	info.Channels[0].RTP = 999
	if c.SessionInfo().Channels[0].RTP == 999 {
		t.Error("SessionInfo().Channels aliases internal state")
	}
}

// Setup must reject a Track it did not hand out BEFORE issuing the request, so
// the handler here scripts no SETUP at all: if the client sent one, the server
// goroutine would read it and the drain would answer it, and the assertion that
// no channel was claimed would still hold. The proof that nothing was sent is
// that Setup returns before the round trip.
func TestSetupRejectsForeignTrack(t *testing.T) {
	c, tracks := describeOne(t, audioVideoSDP, drainRequests)
	defer closeAndWait(t, c)

	crossed := rtsp.Track{ID: tracks[0].ID, Control: tracks[1].Control}
	if err := c.Setup(t.Context(), crossed, rtsp.SetupOptions{}); !errors.Is(err, rtsp.ErrUnknownTrack) {
		t.Errorf("Setup with a crossed ID and Control = %v, want ErrUnknownTrack", err)
	}
	if err := c.Setup(t.Context(), rtsp.Track{ID: 99, Control: "rtsp://cam/s/x"}, rtsp.SetupOptions{}); !errors.Is(err, rtsp.ErrUnknownTrack) {
		t.Errorf("Setup with an out-of-range id = %v, want ErrUnknownTrack", err)
	}
	if chans := c.SessionInfo().Channels; len(chans) != 0 {
		t.Errorf("Channels = %v, want none claimed after rejected Setups", chans)
	}
}

// A second Setup for a track already set up would publish a second pipeline and
// a second channel pair under one track ID, and per-track stats are keyed by
// that ID.
func TestSetupRejectsRepeatedTrack(t *testing.T) {
	c, tracks := describeOne(t, aacSDP, func(sc *testserver.ServerConn) {
		answerSetup(t, sc, 0, 1, 0, 1)
		drainRequests(sc)
	})
	defer closeAndWait(t, c)

	if err := c.Setup(t.Context(), tracks[0], rtsp.SetupOptions{}); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := c.Setup(t.Context(), tracks[0], rtsp.SetupOptions{}); !errors.Is(err, rtsp.ErrTrackAlreadySetUp) {
		t.Errorf("second Setup for the same track = %v, want ErrTrackAlreadySetUp", err)
	}
	if chans := c.SessionInfo().Channels; len(chans) != 1 {
		t.Errorf("Channels = %v, want the single original pair", chans)
	}
}
