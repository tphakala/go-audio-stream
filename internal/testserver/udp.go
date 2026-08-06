package testserver

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/tphakala/go-audio-stream/rtsp"
)

// Tuning constants for the UDP scripting surface.
const (
	// maxUDPDatagram bounds the read buffer for one inbound RTP or RTCP
	// datagram, matching the client's own maxDatagramSize.
	maxUDPDatagram = 65535
	// defaultUDPWaitTimeout bounds how long InjectRTP, InjectRTPSequence, and
	// InjectRTCP wait for the client's media address to be learned before
	// giving up, so a client bug (no hole punch, nothing sent) fails the test
	// with a clear error instead of hanging the package.
	defaultUDPWaitTimeout = 10 * time.Second
	// maxServerUDPBindRetries bounds acceptUDPSetup's retry loop when
	// cfg.ServerRTPBase (plus the per-track offset) collides with a port
	// already bound on this host, mirroring the client's own
	// openMediaSockets retry.
	maxServerUDPBindRetries = 5
)

// udpEndpoint is one server-side UDP socket with a background reader that
// learns the client's address from the first inbound datagram and forwards
// every datagram it reads (including that first one) to recv, so a test can
// drain them. It runs until conn is closed, which happens at test cleanup, so
// a read error there is expected and carries nothing to report.
type udpEndpoint struct {
	conn *net.UDPConn

	known chan struct{} // closed exactly once, when peer is first set.
	mu    sync.Mutex
	peer  *net.UDPAddr

	recv chan []byte
}

// newUDPEndpoint starts the background reader and returns the endpoint.
func newUDPEndpoint(conn *net.UDPConn) *udpEndpoint {
	e := &udpEndpoint{conn: conn, known: make(chan struct{}), recv: make(chan []byte, 64)}
	go e.readLoop()
	return e
}

// readLoop is the endpoint's single reader goroutine.
func (e *udpEndpoint) readLoop() {
	buf := make([]byte, maxUDPDatagram)
	for {
		n, addr, err := e.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		payload := append([]byte(nil), buf[:n]...)
		e.mu.Lock()
		if e.peer == nil {
			e.peer = addr
			close(e.known)
		}
		e.mu.Unlock()
		select {
		case e.recv <- payload:
		default:
			// A test that never drains recv is not asserting on datagram
			// content; dropping here keeps the reader live for the next
			// datagram rather than blocking forever on a full channel.
		}
	}
}

// waitPeer blocks until the client's address is learned from the first
// inbound datagram, or timeout elapses.
func (e *udpEndpoint) waitPeer(timeout time.Duration) (*net.UDPAddr, error) {
	select {
	case <-e.known:
	case <-time.After(timeout):
		return nil, fmt.Errorf("testserver: no client datagram received within %v", timeout)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.peer, nil
}

// waitDatagram blocks for the next inbound datagram, or timeout.
func (e *udpEndpoint) waitDatagram(timeout time.Duration) ([]byte, bool) {
	select {
	case d := <-e.recv:
		return d, true
	case <-time.After(timeout):
		return nil, false
	}
}

// UDPTrack is one negotiated UDP media track on the server side: the
// server's RTP and RTCP sockets, and the client media address each learns
// from the first datagram it reads.
type UDPTrack struct {
	rtp  *udpEndpoint
	rtcp *udpEndpoint
}

// InjectRTP sends one RTP datagram (already marshaled) to the track's
// learned client RTP address. The client address is learned from the
// hole-punch datagram or the first client RTCP RR; InjectRTP blocks until it
// is known, up to defaultUDPWaitTimeout.
func (t *UDPTrack) InjectRTP(datagram []byte) error {
	peer, err := t.rtp.waitPeer(defaultUDPWaitTimeout)
	if err != nil {
		return err
	}
	_, err = t.rtp.conn.WriteToUDP(datagram, peer)
	return err
}

// InjectRTPSequence sends the given RTP datagrams to the track's client RTP
// address in the given order, back to back with no pacing. A test may
// deliberately scramble the order to exercise reordering, or omit entries to
// exercise loss.
func (t *UDPTrack) InjectRTPSequence(datagrams [][]byte) error {
	peer, err := t.rtp.waitPeer(defaultUDPWaitTimeout)
	if err != nil {
		return err
	}
	for i, d := range datagrams {
		if _, err := t.rtp.conn.WriteToUDP(d, peer); err != nil {
			return fmt.Errorf("testserver: InjectRTPSequence datagram %d: %w", i, err)
		}
	}
	return nil
}

// InjectRTCP sends one RTCP datagram to the track's learned client RTCP
// address, blocking until it is known, up to defaultUDPWaitTimeout.
func (t *UDPTrack) InjectRTCP(datagram []byte) error {
	peer, err := t.rtcp.waitPeer(defaultUDPWaitTimeout)
	if err != nil {
		return err
	}
	_, err = t.rtcp.conn.WriteToUDP(datagram, peer)
	return err
}

// WaitClientRTCP blocks until an RTCP datagram from the client arrives on
// this track's RTCP socket (the hole-punch Receiver Report, or a later
// keepalive one), or timeout elapses. It returns the datagram bytes and
// true, or nil and false on timeout. It is the scripting surface a test uses
// to assert the client is actually emitting RTCP over UDP.
func (t *UDPTrack) WaitClientRTCP(timeout time.Duration) ([]byte, bool) {
	return t.rtcp.waitDatagram(timeout)
}

// UDPTracks returns the per-track UDP endpoints negotiated by Handshake, in
// SETUP order, for a UDP HandshakeConfig. Empty for an interleaved
// handshake, and for any track whose UDP SETUP was rejected (cfg.RejectUDP)
// and fell back to interleaved.
func (sc *ServerConn) UDPTracks() []UDPTrack {
	return sc.udpTracks
}

// acceptUDPSetup binds a server UDP socket pair for track i, starting at
// cfg.ServerRTPBase+2*i and retrying at higher even ports on a bind
// collision (mirroring the client's own openMediaSockets retry), answers req
// with the assigned server_port, and returns the UDPTrack recording it. Both
// sockets are registered for cleanup on sc.t.
//
//nolint:gocritic // hugeParam: value receiver matches ServerPorts and InterleavedChannels in rtsp/transport.go: TransportHeader is a small stateless header value, not a hot-path allocation.
func (sc *ServerConn) acceptUDPSetup(cfg *HandshakeConfig, i int, req *rtsp.Request, th rtsp.TransportHeader) (UDPTrack, error) {
	base := cfg.ServerRTPBase + 2*i
	var rtpConn, rtcpConn *net.UDPConn
	var serverRTP int
	var lastErr error
	for attempt := 0; attempt < maxServerUDPBindRetries; attempt++ {
		port := base + 2*attempt
		rc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
		if err != nil {
			lastErr = err
			continue
		}
		rcc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port + 1})
		if err != nil {
			_ = rc.Close()
			lastErr = err
			continue
		}
		rtpConn, rtcpConn, serverRTP = rc, rcc, port
		break
	}
	if rtpConn == nil {
		return UDPTrack{}, fmt.Errorf("testserver: bind server UDP socket pair near port %d: %w", base, lastErr)
	}
	sc.t.Cleanup(func() { _ = rtpConn.Close() })
	sc.t.Cleanup(func() { _ = rtcpConn.Close() })

	track := UDPTrack{rtp: newUDPEndpoint(rtpConn), rtcp: newUDPEndpoint(rtcpConn)}

	h := rtsp.Header{}
	h.Set("Transport", fmt.Sprintf("RTP/AVP;unicast;client_port=%d-%d;server_port=%d-%d",
		th.ClientRTPPort, th.ClientRTCPPort, serverRTP, serverRTP+1))
	h.Set("Session", sessionHeader(cfg))
	if err := sc.Respond(req, rtsp.StatusOK, "OK", h, nil); err != nil {
		return UDPTrack{}, err
	}
	return track, nil
}
