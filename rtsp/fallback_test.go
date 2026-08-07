package rtsp_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/internal/testserver"
	"github.com/tphakala/go-audio-stream/rtsp"
)

// fallbackTwoAudioSDP declares two audio tracks with distinct control URLs, so
// the session-wide pin test can set up two tracks and assert that the second
// SETUP follows the transport the first one resolved.
const fallbackTwoAudioSDP = "v=0\r\n" +
	"o=- 0 0 IN IP4 127.0.0.1\r\n" +
	"s=Stream\r\n" +
	"m=audio 0 RTP/AVP 96\r\n" +
	"a=rtpmap:96 opus/48000/2\r\n" +
	"a=control:audio0\r\n" +
	"m=audio 0 RTP/AVP 96\r\n" +
	"a=rtpmap:96 opus/48000/2\r\n" +
	"a=control:audio1\r\n"

// wantTransportTCP is the SessionInfo.Transport value a TCP-pinned session
// reports, whether it was pinned directly under PreferTCP or by a fallback.
const wantTransportTCP = "TCP"

// wantTransportUDP is the SessionInfo.Transport value a UDP-pinned session
// reports.
const wantTransportUDP = "UDP"

// isUDPProposal reports whether a SETUP Transport header is the RTP/AVP unicast
// UDP proposal (client_port), as opposed to the TCP-interleaved profile.
func isUDPProposal(transport string) bool {
	return strings.HasPrefix(transport, "RTP/AVP;unicast;client_port=")
}

// readSetup reads the next client request off sc, asserts it is a SETUP, and
// returns the request together with the Transport header it proposed. On a read
// error it reports via t and returns a nil request, so the caller can bail
// without dereferencing it.
func readSetup(t *testing.T, sc *testserver.ServerConn) (req *rtsp.Request, transport string) {
	t.Helper()
	req, err := sc.ReadRequest()
	if err != nil {
		t.Errorf("read SETUP: %v", err)
		return nil, ""
	}
	if req.Method != methodSetup {
		t.Errorf("got method %s, want SETUP", req.Method)
	}
	return req, req.Header.Get("Transport")
}

// dialTransport dials with the given transport preference, wiring an OnFrame
// that deep-copies each frame onto frames when it is non-nil.
func dialTransport(t *testing.T, url string, pref rtsp.TransportPreference, frames chan<- audiostream.Frame) *rtsp.Client {
	t.Helper()
	cfg := rtsp.Config{URL: url, Timeout: testTimeout, Transport: pref}
	if frames != nil {
		cfg.OnFrame = func(f audiostream.Frame) {
			cp := f
			cp.Data = append([]byte(nil), f.Data...)
			frames <- cp
		}
	}
	c, err := rtsp.Dial(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	return c
}

// playSessionHeaders builds the header set for a scripted PLAY response, echoing
// the shared session id so the exchange mirrors a real server.
func playSessionHeaders() rtsp.Header {
	h := rtsp.Header{}
	h.Set("Session", sessionValue(testSessionID, testTimeoutS))
	return h
}

// TestFallbackPreferUDPThenTCPFallsBackOn461 covers the primary D6 fallback: the
// server rejects the UDP SETUP with 461 (no Session id, no allocation), and the
// client re-issues the same track's SETUP over TCP interleaved, pins the session
// TCP, and streams frames over the interleaved channels exactly as phase 1.
func TestFallbackPreferUDPThenTCPFallsBackOn461(t *testing.T) {
	t.Parallel()
	payload := []byte{0x78, 0xaa, 0xbb, 0xcc}
	frames := make(chan audiostream.Frame, 8)
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		serve(t, sc, methodOptions, 200, "OK", keepaliveHeader(), nil)
		serve(t, sc, methodDescribe, 200, "OK", sdpHeaders(""), []byte(opusSDP))

		udpReq, udpTransport := readSetup(t, sc)
		if udpReq == nil {
			return
		}
		if !isUDPProposal(udpTransport) {
			t.Errorf("first SETUP proposed %q, want a UDP client_port proposal", udpTransport)
		}
		// 461 with no Session header: the server declined and allocated nothing.
		_ = sc.Respond(udpReq, 461, "Unsupported Transport", nil, nil)

		tcpReq, tcpTransport := readSetup(t, sc)
		if tcpReq == nil {
			return
		}
		if want := rtsp.BuildTransport(0, 1); tcpTransport != want {
			t.Errorf("fallback SETUP proposed %q, want the TCP-interleaved %q", tcpTransport, want)
		}
		_ = sc.Respond(tcpReq, 200, "OK", setupHeaders(0, 1, testSessionID, testTimeoutS), nil)

		serve(t, sc, methodPlay, 200, "OK", playSessionHeaders(), nil)
		_ = sc.InjectFrame(0, buildRTPPacket(ptOpus, 1, 960, 0xabcdef01, false, payload))
		drainRequests(sc)
	}})

	c := dialTransport(t, s.URL("/stream"), rtsp.PreferUDPThenTCP, frames)
	defer closeAndWait(t, c)

	tracks, err := c.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if err := c.Setup(context.Background(), tracks[0], rtsp.SetupOptions{}); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := c.Play(context.Background()); err != nil {
		t.Fatalf("Play: %v", err)
	}

	info := c.SessionInfo()
	if info.Transport != wantTransportTCP {
		t.Errorf("SessionInfo().Transport = %q, want TCP after the fallback", info.Transport)
	}
	if len(info.Channels) != 1 || info.Channels[0].RTP != 0 || info.Channels[0].RTCP != 1 || info.Channels[0].TrackID != 0 {
		t.Errorf("Channels = %+v, want one pair {TrackID:0 RTP:0 RTCP:1}", info.Channels)
	}
	if info.UDPEndpoints != nil {
		t.Errorf("UDPEndpoints = %+v, want nil after a TCP fallback", info.UDPEndpoints)
	}

	f := recvFrame(t, frames)
	if !bytes.Equal(f.Data, payload) {
		t.Errorf("Data = % x, want the Opus payload % x delivered over interleaved channels", f.Data, payload)
	}
}

// TestFallback2xxUnusableServerPortDoesNotFallBack covers the second D6 shape: a
// 2xx SETUP that accepted the session (a Session id) but echoed a Transport with
// no usable server_port. The server created state, so the client must NOT fall
// back to TCP (that would re-SETUP a live session); it tears the accepted
// session down and returns ErrUDPSetupRejected. It holds under both PreferUDP
// and PreferUDPThenTCP.
func TestFallback2xxUnusableServerPortDoesNotFallBack(t *testing.T) {
	t.Parallel()
	for _, pref := range []struct {
		name string
		pref rtsp.TransportPreference
	}{
		{"PreferUDP", rtsp.PreferUDP},
		{"PreferUDPThenTCP", rtsp.PreferUDPThenTCP},
	} {
		t.Run(pref.name, func(t *testing.T) {
			teardownCh := make(chan string, 1)
			s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
				serve(t, sc, methodOptions, 200, "OK", keepaliveHeader(), nil)
				serve(t, sc, methodDescribe, 200, "OK", sdpHeaders(""), []byte(opusSDP))

				udpReq, udpTransport := readSetup(t, sc)
				if udpReq == nil {
					return
				}
				if !isUDPProposal(udpTransport) {
					t.Errorf("SETUP proposed %q, want a UDP client_port proposal", udpTransport)
				}
				// 200 OK with a Session id (state created) but a Transport with no
				// usable server_port: accepted yet unusable.
				h := rtsp.Header{}
				h.Set("Transport", "RTP/AVP;unicast;client_port=1000-1001")
				h.Set("Session", sessionValue(testSessionID, testTimeoutS))
				_ = sc.Respond(udpReq, 200, "OK", h, nil)

				// The very next request must be the TEARDOWN of the accepted
				// session, never a TCP SETUP: captureTeardown fails if it is not.
				captureTeardown(t, sc, teardownCh)
				drainRequests(sc)
			}})

			c := dialTransport(t, s.URL("/stream"), pref.pref, nil)
			defer closeAndWait(t, c)

			tracks, err := c.Describe(context.Background())
			if err != nil {
				t.Fatalf("Describe: %v", err)
			}
			serr := c.Setup(context.Background(), tracks[0], rtsp.SetupOptions{})
			if !errors.Is(serr, rtsp.ErrUDPSetupRejected) {
				t.Fatalf("Setup = %v, want ErrUDPSetupRejected", serr)
			}
			assertTeardown(t, teardownCh, tracks[0].Control, testSessionID)
			if got := c.SessionInfo().Transport; got != "" {
				t.Errorf("SessionInfo().Transport = %q, want empty after a rejected UDP Setup", got)
			}
		})
	}
}

// TestFallbackPreferUDPNoFallbackOn461 covers PreferUDP: a 461 yields
// ErrUDPSetupRejected with no TCP retry. The server confirms the client sends
// nothing more after the 461 (the connection just closes on Close, since a 461
// leaves no session to TEARDOWN).
func TestFallbackPreferUDPNoFallbackOn461(t *testing.T) {
	t.Parallel()
	afterReject := make(chan string, 1)
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		serve(t, sc, methodOptions, 200, "OK", keepaliveHeader(), nil)
		serve(t, sc, methodDescribe, 200, "OK", sdpHeaders(""), []byte(opusSDP))

		udpReq, udpTransport := readSetup(t, sc)
		if udpReq == nil {
			return
		}
		if !isUDPProposal(udpTransport) {
			t.Errorf("SETUP proposed %q, want a UDP client_port proposal", udpTransport)
		}
		_ = sc.Respond(udpReq, 461, "Unsupported Transport", nil, nil)

		// Correct behavior sends no further request: the read blocks until Close
		// drops the connection (EOF). A TCP retry would surface here as a SETUP.
		if req, err := sc.ReadRequest(); err != nil {
			afterReject <- ""
		} else {
			afterReject <- req.Method
		}
	}})

	c := dialTransport(t, s.URL("/stream"), rtsp.PreferUDP, nil)

	tracks, err := c.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	serr := c.Setup(context.Background(), tracks[0], rtsp.SetupOptions{})
	if !errors.Is(serr, rtsp.ErrUDPSetupRejected) {
		t.Fatalf("Setup = %v, want ErrUDPSetupRejected", serr)
	}
	if got := c.SessionInfo().Transport; got != "" {
		t.Errorf("SessionInfo().Transport = %q, want empty after a rejected UDP Setup", got)
	}

	closeAndWait(t, c)
	if got := <-afterReject; got != "" {
		t.Errorf("server saw a %s after the 461; PreferUDP must not retry over TCP", got)
	}
}

// TestFallbackPreferTCPIgnoresUDP covers PreferTCP (the zero value): the SETUP
// proposal is the TCP-interleaved profile, never a UDP client_port.
func TestFallbackPreferTCPIgnoresUDP(t *testing.T) {
	t.Parallel()
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		serve(t, sc, methodOptions, 200, "OK", keepaliveHeader(), nil)
		serve(t, sc, methodDescribe, 200, "OK", sdpHeaders(""), []byte(opusSDP))

		req, transport := readSetup(t, sc)
		if req == nil {
			return
		}
		if want := rtsp.BuildTransport(0, 1); transport != want {
			t.Errorf("SETUP proposed %q, want the TCP-interleaved %q", transport, want)
		}
		if strings.Contains(transport, "client_port") {
			t.Errorf("SETUP proposed %q, want no UDP client_port under PreferTCP", transport)
		}
		_ = sc.Respond(req, 200, "OK", setupHeaders(0, 1, testSessionID, testTimeoutS), nil)

		serve(t, sc, methodPlay, 200, "OK", playSessionHeaders(), nil)
		drainRequests(sc)
	}})

	c := dialTransport(t, s.URL("/stream"), rtsp.PreferTCP, nil)
	defer closeAndWait(t, c)

	tracks, err := c.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if err := c.Setup(context.Background(), tracks[0], rtsp.SetupOptions{}); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := c.Play(context.Background()); err != nil {
		t.Fatalf("Play: %v", err)
	}
	if got := c.SessionInfo().Transport; got != wantTransportTCP {
		t.Errorf("SessionInfo().Transport = %q, want TCP", got)
	}
}

// TestFallbackSessionWidePin covers the session-wide pin: under PreferUDPThenTCP
// the first track falls back to TCP, and the second track's SETUP then proposes
// TCP directly with no UDP attempt, verified on the wire.
func TestFallbackSessionWidePin(t *testing.T) {
	t.Parallel()
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		serve(t, sc, methodOptions, 200, "OK", keepaliveHeader(), nil)
		serve(t, sc, methodDescribe, 200, "OK", sdpHeaders(""), []byte(fallbackTwoAudioSDP))

		// Track 0: UDP proposal rejected 461, then re-proposed TCP interleaved.
		udpReq, udpTransport := readSetup(t, sc)
		if udpReq == nil {
			return
		}
		if !isUDPProposal(udpTransport) {
			t.Errorf("track 0 first SETUP proposed %q, want a UDP client_port proposal", udpTransport)
		}
		_ = sc.Respond(udpReq, 461, "Unsupported Transport", nil, nil)

		tcpReq, tcpTransport := readSetup(t, sc)
		if tcpReq == nil {
			return
		}
		if want := rtsp.BuildTransport(0, 1); tcpTransport != want {
			t.Errorf("track 0 fallback SETUP proposed %q, want %q", tcpTransport, want)
		}
		_ = sc.Respond(tcpReq, 200, "OK", setupHeaders(0, 1, testSessionID, testTimeoutS), nil)

		// Track 1: the pin is TCP now, so it must propose TCP directly, never UDP.
		req1, transport1 := readSetup(t, sc)
		if req1 == nil {
			return
		}
		if isUDPProposal(transport1) {
			t.Errorf("track 1 SETUP proposed %q, want TCP directly: the session is pinned TCP", transport1)
		}
		if want := rtsp.BuildTransport(2, 3); transport1 != want {
			t.Errorf("track 1 SETUP proposed %q, want the TCP-interleaved %q", transport1, want)
		}
		_ = sc.Respond(req1, 200, "OK", setupHeaders(2, 3, testSessionID, testTimeoutS), nil)

		serve(t, sc, methodPlay, 200, "OK", playSessionHeaders(), nil)
		drainRequests(sc)
	}})

	c := dialTransport(t, s.URL("/stream"), rtsp.PreferUDPThenTCP, nil)
	defer closeAndWait(t, c)

	tracks, err := c.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if len(tracks) != 2 {
		t.Fatalf("track count = %d, want 2", len(tracks))
	}
	for i, tr := range tracks {
		if err := c.Setup(context.Background(), tr, rtsp.SetupOptions{}); err != nil {
			t.Fatalf("Setup track %d: %v", i, err)
		}
	}
	if err := c.Play(context.Background()); err != nil {
		t.Fatalf("Play: %v", err)
	}

	info := c.SessionInfo()
	if info.Transport != wantTransportTCP {
		t.Errorf("SessionInfo().Transport = %q, want TCP", info.Transport)
	}
	if len(info.Channels) != 2 {
		t.Fatalf("Channels = %+v, want two pairs", info.Channels)
	}
	if info.Channels[0] != (rtsp.ChannelPair{TrackID: 0, RTP: 0, RTCP: 1}) {
		t.Errorf("Channels[0] = %+v, want {TrackID:0 RTP:0 RTCP:1}", info.Channels[0])
	}
	if info.Channels[1] != (rtsp.ChannelPair{TrackID: 1, RTP: 2, RTCP: 3}) {
		t.Errorf("Channels[1] = %+v, want {TrackID:1 RTP:2 RTCP:3}", info.Channels[1])
	}
}
