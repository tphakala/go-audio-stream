package rtsp_test

import (
	"context"
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
		serve(t, sc, methodOptions, 200, "OK", publicHeader(), nil)
		serve(t, sc, methodDescribe, 200, "OK", sdpHeaders(""), []byte(sdp))
		setupFn(sc)
	}})
	c := dialIdle(t, s.URL("/stream"))
	tracks, err := c.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	return c, tracks
}

// answerSetup reads one SETUP and answers it with the given interleaved pair
// and the shared session id.
func answerSetup(t *testing.T, sc *testserver.ServerConn, rtpCh, rtcpCh int) {
	t.Helper()
	serve(t, sc, methodSetup, 200, "OK", setupHeaders(rtpCh, rtcpCh, testSessionID, testTimeoutS), nil)
}

func TestSetupRenumberedChannels(t *testing.T) {
	c, tracks := describeOne(t, aacSDP, func(sc *testserver.ServerConn) {
		answerSetup(t, sc, 4, 5) // server renumbers away from the proposed 0-1
		drainRequests(sc)
	})
	defer closeAndWait(t, c)

	if err := c.Setup(context.Background(), tracks[0], rtsp.SetupOptions{}); err != nil {
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
		answerSetup(t, sc, 0, 1)
		answerSetup(t, sc, 2, 3)
		drainRequests(sc)
	})
	defer closeAndWait(t, c)

	if err := c.Setup(context.Background(), tracks[0], rtsp.SetupOptions{}); err != nil {
		t.Fatalf("Setup track 0: %v", err)
	}
	if err := c.Setup(context.Background(), tracks[1], rtsp.SetupOptions{}); err != nil {
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
	c, tracks := describeOne(t, audioVideoSDP, func(sc *testserver.ServerConn) {
		answerSetup(t, sc, 0, 1)
		// Second SETUP returns the same pair the first track already claimed.
		answerSetup(t, sc, 0, 1)
		drainRequests(sc)
	})
	defer closeAndWait(t, c)

	if err := c.Setup(context.Background(), tracks[0], rtsp.SetupOptions{}); err != nil {
		t.Fatalf("Setup track 0: %v", err)
	}
	err := c.Setup(context.Background(), tracks[1], rtsp.SetupOptions{})
	if !errors.Is(err, rtsp.ErrChannelConflict) {
		t.Fatalf("Setup track 1 = %v, want ErrChannelConflict", err)
	}
	// The session must survive a rejected Setup: the first track's channels
	// are still bound.
	if len(c.SessionInfo().Channels) != 1 {
		t.Errorf("Channels = %v, want the first track only", c.SessionInfo().Channels)
	}
}

func TestSetupBadTransport(t *testing.T) {
	c, tracks := describeOne(t, aacSDP, func(sc *testserver.ServerConn) {
		req, err := sc.ReadRequest()
		if err != nil {
			return
		}
		h := rtsp.Header{}
		h.Set("Transport", "RTP/AVP/TCP;unicast") // no interleaved pair
		h.Set("Session", sessionValue(testSessionID, testTimeoutS))
		_ = sc.Respond(req, 200, "OK", h, nil)
		drainRequests(sc)
	})
	defer closeAndWait(t, c)

	err := c.Setup(context.Background(), tracks[0], rtsp.SetupOptions{})
	if !errors.Is(err, rtsp.ErrNoInterleaved) {
		t.Fatalf("Setup = %v, want ErrNoInterleaved", err)
	}
}

func TestSetupStatusError(t *testing.T) {
	c, tracks := describeOne(t, aacSDP, func(sc *testserver.ServerConn) {
		serve(t, sc, methodSetup, 461, "Unsupported Transport", nil, nil)
		drainRequests(sc)
	})
	defer closeAndWait(t, c)

	err := c.Setup(context.Background(), tracks[0], rtsp.SetupOptions{})
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

	err := c.Setup(context.Background(), rtsp.Track{ID: 0, Control: "rtsp://x/track0"}, rtsp.SetupOptions{})
	var se *rtsp.StateError
	if !errors.As(err, &se) {
		t.Fatalf("Setup before Describe = %v, want *StateError", err)
	}
	if !errors.Is(err, rtsp.ErrInvalidState) {
		t.Errorf("Setup before Describe does not match ErrInvalidState")
	}
}

func TestSetupDiscardFlag(t *testing.T) {
	c, tracks := describeOne(t, aacSDP, func(sc *testserver.ServerConn) {
		answerSetup(t, sc, 0, 1)
		drainRequests(sc)
	})
	defer closeAndWait(t, c)

	if err := c.Setup(context.Background(), tracks[0], rtsp.SetupOptions{Discard: true}); err != nil {
		t.Fatalf("Setup with Discard: %v", err)
	}
	if len(c.SessionInfo().Channels) != 1 {
		t.Errorf("Channels = %v, want one pair for the discard track", c.SessionInfo().Channels)
	}
}

func TestSessionInfoAfterSetup(t *testing.T) {
	c, tracks := describeOne(t, audioVideoSDP, func(sc *testserver.ServerConn) {
		answerSetup(t, sc, 0, 1)
		answerSetup(t, sc, 2, 3)
		drainRequests(sc)
	})
	defer closeAndWait(t, c)

	if err := c.Setup(context.Background(), tracks[0], rtsp.SetupOptions{}); err != nil {
		t.Fatalf("Setup track 0: %v", err)
	}
	if err := c.Setup(context.Background(), tracks[1], rtsp.SetupOptions{}); err != nil {
		t.Fatalf("Setup track 1: %v", err)
	}

	info := c.SessionInfo()
	if info.SessionID != testSessionID {
		t.Errorf("SessionID = %q, want %q", info.SessionID, testSessionID)
	}
	if info.SessionTimeout != testTimeoutS*time.Second {
		t.Errorf("SessionTimeout = %v, want %ds", info.SessionTimeout, testTimeoutS)
	}
	if info.KeepaliveMethod != methodOptions {
		t.Errorf("KeepaliveMethod = %q, want OPTIONS", info.KeepaliveMethod)
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
