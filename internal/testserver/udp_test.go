package testserver

import (
	"bytes"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tphakala/go-audio-stream/rtsp"
)

// serverRTPBaseUDP is the fixed base port these tests' servers bind their RTP
// socket at (RTCP at +1). acceptUDPSetup retries at higher ports on a bind
// collision, so an occasionally busy port on the test host does not make
// these flaky; the tests below use distinct offsets from each other so they
// do not contend even when run back to back.
const serverRTPBaseUDP = 47100

// udpHandshakeResult carries a completed UDP Handshake's outcome from the
// server's Handle goroutine to the test goroutine over a channel, which is
// what gives the test a documented happens-before edge on the negotiated
// state (a shared variable written by one goroutine and read by another
// would not, without one).
type udpHandshakeResult struct {
	sc    *ServerConn
	pairs []ChannelPair
}

// bindFakeClientUDPPair binds a consecutive UDP port pair on 127.0.0.1,
// standing in for the real client's openMediaSockets, and registers both
// sockets for cleanup on t.
func bindFakeClientUDPPair(t *testing.T) (rtpConn, rtcpConn *net.UDPConn) {
	t.Helper()
	rtpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenUDP client RTP: %v", err)
	}
	t.Cleanup(func() { _ = rtpConn.Close() })
	clientRTPPort := rtpConn.LocalAddr().(*net.UDPAddr).Port
	rtcpConn, err = net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: clientRTPPort + 1})
	if err != nil {
		t.Fatalf("ListenUDP client RTCP: %v", err)
	}
	t.Cleanup(func() { _ = rtcpConn.Close() })
	return rtpConn, rtcpConn
}

// clientPort returns conn's bound local UDP port.
func clientPort(conn *net.UDPConn) int {
	return conn.LocalAddr().(*net.UDPAddr).Port
}

// readUDPWithDeadline reads one datagram off conn within timeout, failing
// the test on any error or timeout.
func readUDPWithDeadline(t *testing.T, conn *net.UDPConn, timeout time.Duration) []byte {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, 1500)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read datagram: %v", err)
	}
	return buf[:n]
}

// TestServerHandshakeUDP drives a fake client through a UDP SETUP and PLAY,
// asserting the server_port response, then exercises InjectRTP,
// InjectRTPSequence, InjectRTCP, and WaitClientRTCP against a fake client
// socket pair standing in for the real rtsp.Client.
func TestServerHandshakeUDP(t *testing.T) {
	t.Parallel()
	resultCh := make(chan udpHandshakeResult, 1)
	done := make(chan struct{})
	var doneOnce sync.Once
	closeDone := func() { doneOnce.Do(func() { close(done) }) }
	// Registered before any client-side exchange, so a Fatalf anywhere below
	// (which runtime.Goexit's out of this test) still unblocks the handler
	// goroutine rather than leaving it parked on <-done forever, which would
	// hang the whole package in t.Cleanup(s.stop).
	t.Cleanup(closeDone)

	s := New(t, Options{Handle: func(sc *ServerConn) {
		pairs, err := sc.Handshake(HandshakeConfig{
			SDP:           aacSDP,
			SessionID:     "sess-udp",
			UDP:           true,
			ServerRTPBase: serverRTPBaseUDP,
		})
		if err != nil {
			t.Errorf("Handshake: %v", err)
			resultCh <- udpHandshakeResult{}
			return
		}
		resultCh <- udpHandshakeResult{sc: sc, pairs: pairs}
		<-done // keep the connection, and its UDP sockets, alive for injection.
	}})

	c := dialPlain(t, s, "/stream")
	base := s.URL("/stream")
	clientOptionsDescribe(t, c, base)

	clientRTP, clientRTCP := bindFakeClientUDPPair(t)
	clientRTPPort := clientPort(clientRTP)

	h := rtsp.Header{}
	h.Set("Transport", rtsp.BuildTransportUDP(clientRTPPort, clientRTPPort+1))
	c.send("SETUP", base, h, nil)
	resp, err := c.readResponse()
	if err != nil {
		t.Fatalf("read SETUP response: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("SETUP: got %d, want 200", resp.StatusCode)
	}
	th, terr := rtsp.ParseTransport(resp.Header.Get("Transport"))
	if terr != nil {
		t.Fatalf("parse SETUP transport: %v", terr)
	}
	rtpPort, rtcpPort, ok := th.ServerPorts()
	if !ok {
		t.Fatalf("SETUP response carried no usable server_port: %q", resp.Header.Get("Transport"))
	}
	if rtcpPort != rtpPort+1 {
		t.Fatalf("server_port pair not consecutive: %d, %d", rtpPort, rtcpPort)
	}
	if th.ClientRTPPort != clientRTPPort || th.ClientRTCPPort != clientRTPPort+1 {
		t.Errorf("echoed client_port = %d-%d, want %d-%d", th.ClientRTPPort, th.ClientRTCPPort, clientRTPPort, clientRTPPort+1)
	}
	serverRTPAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: rtpPort}
	serverRTCPAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: rtcpPort}

	c.send("PLAY", base, nil, nil)
	resp, err = c.readResponse()
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("PLAY: resp=%+v err=%v", resp, err)
	}

	res := <-resultCh
	if res.sc == nil {
		t.Fatal("Handshake failed; see prior error")
	}
	defer closeDone()

	if len(res.pairs) != 1 || res.pairs[0] != (ChannelPair{}) {
		t.Errorf("pairs = %+v, want one zero ChannelPair for a UDP track", res.pairs)
	}
	tracks := res.sc.UDPTracks()
	if len(tracks) != 1 {
		t.Fatalf("UDPTracks: got %d, want 1", len(tracks))
	}
	track := &tracks[0]

	// Hole-punch, exactly as the real client's mediaSockets.holePunch does: a
	// zero-length RTP datagram and an RTCP datagram, so InjectRTP/InjectRTCP
	// learn where to send and WaitClientRTCP has something to return.
	if _, err := clientRTP.WriteToUDP(nil, serverRTPAddr); err != nil {
		t.Fatalf("client RTP hole punch: %v", err)
	}
	rtcpPunch := []byte{0x80, 0xc9, 0x00, 0x01, 0xde, 0xad, 0xbe, 0xef}
	if _, err := clientRTCP.WriteToUDP(rtcpPunch, serverRTCPAddr); err != nil {
		t.Fatalf("client RTCP hole punch: %v", err)
	}

	if got, ok := track.WaitClientRTCP(2 * time.Second); !ok || !bytes.Equal(got, rtcpPunch) {
		t.Errorf("WaitClientRTCP = (% x, %v), want (% x, true)", got, ok, rtcpPunch)
	}

	rtpDatagrams := [][]byte{
		{0x80, 0x60, 0x00, 0x01, 0, 0, 0, 1, 0, 0, 0, 1, 0xAA},
		{0x80, 0x60, 0x00, 0x02, 0, 0, 0, 2, 0, 0, 0, 1, 0xBB},
	}
	if err := track.InjectRTPSequence(rtpDatagrams); err != nil {
		t.Fatalf("InjectRTPSequence: %v", err)
	}
	for i, want := range rtpDatagrams {
		if got := readUDPWithDeadline(t, clientRTP, 2*time.Second); !bytes.Equal(got, want) {
			t.Errorf("datagram %d = % x, want % x", i, got, want)
		}
	}

	rtcpDatagram := []byte{0x80, 0xc8, 0x00, 0x06, 1, 2, 3, 4}
	if err := track.InjectRTCP(rtcpDatagram); err != nil {
		t.Fatalf("InjectRTCP: %v", err)
	}
	if got := readUDPWithDeadline(t, clientRTCP, 2*time.Second); !bytes.Equal(got, rtcpDatagram) {
		t.Errorf("RTCP datagram = % x, want % x", got, rtcpDatagram)
	}
}

// TestServerHandshakeUDPRejects covers RejectUDP: the first SETUP proposes
// UDP and is answered 461, and the immediate retry (the same track, now
// TCP-interleaved) is handled exactly like an interleaved-only handshake.
// UDPTracks stays empty since no UDP track was ever bound.
func TestServerHandshakeUDPRejects(t *testing.T) {
	t.Parallel()
	resultCh := make(chan udpHandshakeResult, 1)
	s := New(t, Options{Handle: func(sc *ServerConn) {
		pairs, err := sc.Handshake(HandshakeConfig{
			SDP:           aacSDP,
			SessionID:     "sess-udp-reject",
			UDP:           true,
			RejectUDP:     true,
			ServerRTPBase: serverRTPBaseUDP + 20,
		})
		if err != nil {
			t.Errorf("Handshake: %v", err)
			resultCh <- udpHandshakeResult{}
			return
		}
		resultCh <- udpHandshakeResult{sc: sc, pairs: pairs}
	}})

	c := dialPlain(t, s, "/stream")
	base := s.URL("/stream")
	clientOptionsDescribe(t, c, base)

	clientRTP, _ := bindFakeClientUDPPair(t)
	clientRTPPort := clientPort(clientRTP)

	h := rtsp.Header{}
	h.Set("Transport", rtsp.BuildTransportUDP(clientRTPPort, clientRTPPort+1))
	c.send("SETUP", base, h, nil)
	resp, err := c.readResponse()
	if err != nil {
		t.Fatalf("read UDP SETUP response: %v", err)
	}
	if resp.StatusCode != 461 {
		t.Fatalf("UDP SETUP: got %d, want 461", resp.StatusCode)
	}

	h2 := rtsp.Header{}
	h2.Set("Transport", rtsp.BuildTransport(0, 1))
	c.send("SETUP", base, h2, nil)
	resp, err = c.readResponse()
	if err != nil {
		t.Fatalf("read fallback SETUP response: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("fallback SETUP: got %d, want 200", resp.StatusCode)
	}
	tr, terr := rtsp.ParseTransport(resp.Header.Get("Transport"))
	if terr != nil || !tr.Interleaved || tr.RTPChannel != 0 || tr.RTCPChannel != 1 {
		t.Fatalf("fallback SETUP transport = %q, want interleaved 0-1", resp.Header.Get("Transport"))
	}

	c.send("PLAY", base, nil, nil)
	resp, err = c.readResponse()
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("PLAY: resp=%+v err=%v", resp, err)
	}

	res := <-resultCh
	if res.sc == nil {
		t.Fatal("Handshake failed; see prior error")
	}
	if len(res.pairs) != 1 || res.pairs[0] != (ChannelPair{RTP: 0, RTCP: 1}) {
		t.Errorf("pairs = %+v, want [{0 1}]", res.pairs)
	}
	if got := res.sc.UDPTracks(); len(got) != 0 {
		t.Errorf("UDPTracks after a rejected UDP SETUP: got %d, want 0", len(got))
	}
}

// TestServerHandshakeUDPNonProposalFallsThrough covers handshakeSetupUDP's
// "not a UDP proposal" branch (the one carrying the deliberate nolint:nilerr):
// under a UDP HandshakeConfig with RejectUDP off, a SETUP that carries no
// client_port is not a UDP request, so the server falls through to the
// interleaved branch and binds no UDP track, exactly as an interleaved-only
// handshake would.
func TestServerHandshakeUDPNonProposalFallsThrough(t *testing.T) {
	t.Parallel()
	resultCh := make(chan udpHandshakeResult, 1)
	s := New(t, Options{Handle: func(sc *ServerConn) {
		pairs, err := sc.Handshake(HandshakeConfig{
			SDP:           aacSDP,
			SessionID:     "sess-udp-nonproposal",
			UDP:           true,
			ServerRTPBase: serverRTPBaseUDP + 40,
		})
		if err != nil {
			t.Errorf("Handshake: %v", err)
			resultCh <- udpHandshakeResult{}
			return
		}
		resultCh <- udpHandshakeResult{sc: sc, pairs: pairs}
	}})

	c := dialPlain(t, s, "/stream")
	base := s.URL("/stream")
	clientOptionsDescribe(t, c, base)

	// A TCP-interleaved SETUP (no client_port) under a UDP config is not a UDP
	// proposal, so the server must fall through to the interleaved branch.
	h := rtsp.Header{}
	h.Set("Transport", rtsp.BuildTransport(0, 1))
	c.send("SETUP", base, h, nil)
	resp, err := c.readResponse()
	if err != nil {
		t.Fatalf("read SETUP response: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("SETUP: got %d, want 200", resp.StatusCode)
	}
	tr, terr := rtsp.ParseTransport(resp.Header.Get("Transport"))
	if terr != nil || !tr.Interleaved || tr.RTPChannel != 0 || tr.RTCPChannel != 1 {
		t.Fatalf("SETUP transport = %q, want interleaved 0-1", resp.Header.Get("Transport"))
	}

	c.send("PLAY", base, nil, nil)
	resp, err = c.readResponse()
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("PLAY: resp=%+v err=%v", resp, err)
	}

	res := <-resultCh
	if res.sc == nil {
		t.Fatal("Handshake failed; see prior error")
	}
	if len(res.pairs) != 1 || res.pairs[0] != (ChannelPair{RTP: 0, RTCP: 1}) {
		t.Errorf("pairs = %+v, want [{0 1}]", res.pairs)
	}
	if got := res.sc.UDPTracks(); len(got) != 0 {
		t.Errorf("UDPTracks after a non-UDP SETUP: got %d, want 0", len(got))
	}
}

// TestServerHandshakeUDPRejectsUnsetServerRTPBase asserts acceptUDPSetup fails
// fast with an explicit config error when ServerRTPBase is unset (zero) on a
// UDP HandshakeConfig, rather than burning through its bind retries against
// privileged ports (an unset base makes the first attempt bind RTCP on port 1).
// The error must name ServerRTPBase so the misconfiguration is obvious.
func TestServerHandshakeUDPRejectsUnsetServerRTPBase(t *testing.T) {
	t.Parallel()
	errCh := make(chan error, 1)
	s := New(t, Options{Handle: func(sc *ServerConn) {
		_, err := sc.Handshake(HandshakeConfig{
			SDP:       aacSDP,
			SessionID: "sess-udp-badbase",
			UDP:       true,
			// ServerRTPBase deliberately left unset (0).
		})
		errCh <- err
	}})

	c := dialPlain(t, s, "/stream")
	base := s.URL("/stream")
	clientOptionsDescribe(t, c, base)

	clientRTP, _ := bindFakeClientUDPPair(t)
	h := rtsp.Header{}
	h.Set("Transport", rtsp.BuildTransportUDP(clientPort(clientRTP), clientPort(clientRTP)+1))
	c.send("SETUP", base, h, nil)
	// The server rejects the SETUP with a config error before binding and closes
	// the connection, so the client's response read would fail; the authoritative
	// signal is the Handshake error below, not a client-visible response.

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Handshake succeeded with an unset ServerRTPBase, want a config error")
		}
		if !strings.Contains(err.Error(), "ServerRTPBase") {
			t.Errorf("Handshake error = %v, want it to name ServerRTPBase", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Handshake did not return within 2s")
	}
}
