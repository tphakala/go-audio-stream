package rtsp

import (
	"errors"
	"net"
	"testing"
	"time"
)

// testUDPSourceIP is the resolveServerPeers "source" parameter value shared
// across the tests below.
const testUDPSourceIP = "192.0.2.10"

// openMediaSockets must bind a real, usable client_port pair: RTP on an
// ephemeral port and RTCP immediately above it, both actually bound rather
// than merely recorded.
func TestUDPOpenMediaSocketsBindsConsecutivePorts(t *testing.T) {
	t.Parallel()
	m, err := openMediaSockets()
	if err != nil {
		t.Fatalf("openMediaSockets: %v", err)
	}
	defer func() { _ = m.Close() }()

	if m.clientRTPPort <= 0 || m.clientRTPPort > 65534 {
		t.Fatalf("clientRTPPort = %d, want a bindable port with room for RTP+1", m.clientRTPPort)
	}
	if m.clientRTCPPort != m.clientRTPPort+1 {
		t.Fatalf("clientRTCPPort = %d, want %d (RTP+1)", m.clientRTCPPort, m.clientRTPPort+1)
	}

	rtpAddr, ok := m.rtpConn.LocalAddr().(*net.UDPAddr)
	if !ok || rtpAddr.Port != m.clientRTPPort {
		t.Errorf("rtpConn.LocalAddr() = %v, want a *net.UDPAddr bound to port %d", m.rtpConn.LocalAddr(), m.clientRTPPort)
	}
	rtcpAddr, ok := m.rtcpConn.LocalAddr().(*net.UDPAddr)
	if !ok || rtcpAddr.Port != m.clientRTCPPort {
		t.Errorf("rtcpConn.LocalAddr() = %v, want a *net.UDPAddr bound to port %d", m.rtcpConn.LocalAddr(), m.clientRTCPPort)
	}
}

// Close must be safe to call twice: the second call is a no-op, not a panic
// or a second error from closing an already-closed socket.
func TestUDPMediaSocketsCloseIsIdempotent(t *testing.T) {
	t.Parallel()
	m, err := openMediaSockets()
	if err != nil {
		t.Fatalf("openMediaSockets: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Errorf("second Close: %v, want nil (idempotent)", err)
	}
}

func TestUDPResolveServerPeersUsesSourceWhenPresent(t *testing.T) {
	t.Parallel()
	m := &mediaSockets{}
	th := TransportHeader{
		HasServerPort:  true,
		ServerRTPPort:  6000,
		ServerRTCPPort: 6001,
		Source:         testUDPSourceIP,
	}
	if err := m.resolveServerPeers(th, net.ParseIP("198.51.100.1")); err != nil {
		t.Fatalf("resolveServerPeers: %v", err)
	}
	if m.rtpPeer == nil || m.rtpPeer.IP.String() != testUDPSourceIP || m.rtpPeer.Port != 6000 {
		t.Errorf("rtpPeer = %+v, want 192.0.2.10:6000", m.rtpPeer)
	}
	if m.rtcpPeer == nil || m.rtcpPeer.IP.String() != testUDPSourceIP || m.rtcpPeer.Port != 6001 {
		t.Errorf("rtcpPeer = %+v, want 192.0.2.10:6001", m.rtcpPeer)
	}
}

func TestUDPResolveServerPeersFallsBackToControlPeerIP(t *testing.T) {
	t.Parallel()
	m := &mediaSockets{}
	th := TransportHeader{HasServerPort: true, ServerRTPPort: 7000, ServerRTCPPort: 7001}
	controlIP := net.ParseIP("203.0.113.5")
	if err := m.resolveServerPeers(th, controlIP); err != nil {
		t.Fatalf("resolveServerPeers: %v", err)
	}
	if m.rtpPeer == nil || !m.rtpPeer.IP.Equal(controlIP) || m.rtpPeer.Port != 7000 {
		t.Errorf("rtpPeer = %+v, want %s:7000", m.rtpPeer, controlIP)
	}
	if m.rtcpPeer == nil || !m.rtcpPeer.IP.Equal(controlIP) || m.rtcpPeer.Port != 7001 {
		t.Errorf("rtcpPeer = %+v, want %s:7001", m.rtcpPeer, controlIP)
	}
}

func TestUDPResolveServerPeersRejectsMissingServerPort(t *testing.T) {
	t.Parallel()
	m := &mediaSockets{}
	th := TransportHeader{}
	if err := m.resolveServerPeers(th, net.ParseIP("203.0.113.5")); !errors.Is(err, ErrUDPSetupRejected) {
		t.Errorf("resolveServerPeers = %v, want ErrUDPSetupRejected", err)
	}
}

// holePunch must send natPunchCount zero-length datagrams on the RTP socket
// and at least one non-empty datagram (the caller-supplied RR) on the RTCP
// socket, both to the resolved peers.
func TestUDPHolePunchSendsDatagramsToResolvedPeers(t *testing.T) {
	t.Parallel()
	rtpPeer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenUDP rtp peer: %v", err)
	}
	defer func() { _ = rtpPeer.Close() }()
	rtcpPeer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenUDP rtcp peer: %v", err)
	}
	defer func() { _ = rtcpPeer.Close() }()

	m, err := openMediaSockets()
	if err != nil {
		t.Fatalf("openMediaSockets: %v", err)
	}
	defer func() { _ = m.Close() }()
	m.rtpPeer, _ = rtpPeer.LocalAddr().(*net.UDPAddr)
	m.rtcpPeer, _ = rtcpPeer.LocalAddr().(*net.UDPAddr)

	rr := []byte{0x81, 0xc9, 0x00, 0x01} // stand-in RR bytes; content is opaque to holePunch.
	m.holePunch(rr, nil)

	buf := make([]byte, maxDatagramSize)
	for i := 0; i < natPunchCount; i++ {
		if err := rtpPeer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatalf("SetReadDeadline: %v", err)
		}
		n, _, rerr := rtpPeer.ReadFromUDP(buf)
		if rerr != nil {
			t.Fatalf("RTP datagram %d: %v", i, rerr)
		}
		if n != 0 {
			t.Errorf("RTP datagram %d length = %d, want 0", i, n)
		}
	}

	if err := rtcpPeer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	n, _, rerr := rtcpPeer.ReadFromUDP(buf)
	if rerr != nil {
		t.Fatalf("RTCP datagram: %v", rerr)
	}
	if n == 0 {
		t.Error("RTCP datagram length = 0, want a non-empty RR datagram")
	}
}

// initiateShutdown must arm an immediate read deadline on every registered
// media socket, per D3, so a receive goroutine blocked in ReadFromUDP
// unblocks promptly instead of waiting out ReadIdle or the teardown deadline.
func TestUDPInitiateShutdownArmsMediaSocketReadDeadlines(t *testing.T) {
	t.Parallel()
	m, err := openMediaSockets()
	if err != nil {
		t.Fatalf("openMediaSockets: %v", err)
	}
	defer func() { _ = m.Close() }()

	local, remote := net.Pipe()
	defer func() { _ = remote.Close() }()

	c := &Client{
		conn:    local,
		closing: make(chan struct{}),
		media:   map[int]*mediaSockets{0: m},
	}

	started := make(chan struct{})
	readErr := make(chan error, 1)
	go func() {
		close(started)
		buf := make([]byte, maxDatagramSize)
		_, _, rerr := m.rtpConn.ReadFromUDP(buf)
		readErr <- rerr
	}()
	<-started
	// Give the goroutine a moment to actually reach the blocking read before
	// the deadline is armed, so the assertion exercises the interrupt rather
	// than a read that had not started yet.
	time.Sleep(20 * time.Millisecond)

	c.initiateShutdown(errors.New("test shutdown"))

	select {
	case rerr := <-readErr:
		var ne net.Error
		if !errors.As(rerr, &ne) || !ne.Timeout() {
			t.Errorf("ReadFromUDP returned %v, want a timeout error", rerr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ReadFromUDP did not return after initiateShutdown; the media socket read deadline was not armed")
	}
}

// publishUDPTrack is the coordination point setupUDP calls on success: it
// must record the track, pin the session to UDP, and register the socket
// pair under mediaMu, all WITHOUT installing an interleaved channel table.
func TestUDPPublishUDPTrackRegistersMediaAndPinsTransport(t *testing.T) {
	t.Parallel()
	c := &Client{state: stateDescribed, cfg: Config{Transport: PreferUDP}}
	m, err := openMediaSockets()
	if err != nil {
		t.Fatalf("openMediaSockets: %v", err)
	}
	defer func() { _ = m.Close() }()

	tr := &track{id: 0}
	if err := c.publishUDPTrack(tr, 0, m); err != nil {
		t.Fatalf("publishUDPTrack: %v", err)
	}
	if !c.udpPinned.Load() {
		t.Error("udpPinned = false, want true")
	}
	if c.transport != PreferUDP {
		t.Errorf("transport = %v, want PreferUDP", c.transport)
	}
	if c.channels.Load() != nil {
		t.Error("publishUDPTrack must not install an interleaved channel table")
	}
	if len(c.tracks) != 1 || c.tracks[0] != tr {
		t.Errorf("tracks = %v, want [tr]", c.tracks)
	}
	if c.state != stateSetup {
		t.Errorf("state = %v, want setup", c.state)
	}

	c.mediaMu.Lock()
	got := c.media[0]
	c.mediaMu.Unlock()
	if got != m {
		t.Errorf("media[0] = %v, want %v", got, m)
	}
}

// A publish arriving after shutdown must not resurrect the state, must not
// register the socket pair, and must close it: publishUDPTrack is the last
// chance to release sockets that will now never be used by a receive
// goroutine.
func TestUDPPublishUDPTrackRefusesAfterShutdownAndClosesSockets(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("terminal")
	c := &Client{state: stateDescribed, termErr: sentinel}
	m, err := openMediaSockets()
	if err != nil {
		t.Fatalf("openMediaSockets: %v", err)
	}

	if err := c.publishUDPTrack(&track{id: 0}, 0, m); !errors.Is(err, sentinel) {
		t.Errorf("publishUDPTrack after shutdown = %v, want the terminal cause", err)
	}
	if len(c.media) != 0 {
		t.Error("a refused publish must not register the socket pair")
	}
	if c.state == stateSetup {
		t.Error("a refused publish must not resurrect the state")
	}
	// mediaSockets.Close is idempotent (sync.Once), so a second call through
	// it cannot prove anything; closing the raw conn directly is what shows
	// publishUDPTrack actually closed it rather than leaking the fd.
	if err := m.rtpConn.Close(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("rtpConn Close after publishUDPTrack = %v, want net.ErrClosed (already closed)", err)
	}
}

// fakeAddr is a net.Addr backed by nothing but a host:port string, used to
// exercise remoteIP's SplitHostPort fallback for an address type that is not
// *net.TCPAddr (the shape of tls.Conn.RemoteAddr() on some platforms).
type fakeAddr string

func (a fakeAddr) Network() string { return "tcp" }
func (a fakeAddr) String() string  { return string(a) }

func TestUDPRemoteIPExtractsFromTCPAddr(t *testing.T) {
	t.Parallel()
	addr := &net.TCPAddr{IP: net.ParseIP("192.0.2.55"), Port: 554}
	if got := remoteIP(addr); !got.Equal(net.ParseIP("192.0.2.55")) {
		t.Errorf("remoteIP(%v) = %v, want 192.0.2.55", addr, got)
	}
}

func TestUDPRemoteIPFallsBackToHostPortParsing(t *testing.T) {
	t.Parallel()
	addr := fakeAddr("203.0.113.9:554")
	if got := remoteIP(addr); !got.Equal(net.ParseIP("203.0.113.9")) {
		t.Errorf("remoteIP(%v) = %v, want 203.0.113.9", addr, got)
	}
}

func TestUDPRemoteIPNilAddr(t *testing.T) {
	t.Parallel()
	if got := remoteIP(nil); got != nil {
		t.Errorf("remoteIP(nil) = %v, want nil", got)
	}
}
