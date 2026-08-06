package rtsp

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
)

// Tuning constants for UDP transport.
const (
	// maxDatagramSize bounds a single RTP or RTCP UDP datagram. It is the
	// maximum theoretical UDP payload size; the receive goroutines
	// (runRTPReceiver, runRTCPReceiver, and runDiscardReceiver) use it to size
	// their reusable read buffers.
	maxDatagramSize = 65535
	// natPunchCount is the number of best-effort datagrams holePunch sends
	// on each socket to open the return path through a NAT or firewall.
	natPunchCount = 2
	// maxSocketBindRetries bounds openMediaSockets' retry loop when the
	// RTP+1 RTCP port collides with a socket already bound to it.
	maxSocketBindRetries = 5
)

// ErrUDPSocket wraps a failure to open the local UDP socket pair a track
// needs before SETUP can propose RTP/AVP unicast.
var ErrUDPSocket = errors.New("rtsp: could not open UDP media socket pair")

// mediaSockets holds one track's UDP socket pair and the resolved server
// peers, in UDP transport mode. It is created during Setup and owned by the
// receive goroutines after Play; Close drops both sockets and unblocks their
// reads. All fields are set once at construction (rtpConn, rtcpConn,
// clientRTPPort and clientRTCPPort by openMediaSockets; rtpPeer and rtcpPeer
// by resolveServerPeers) and never mutated afterward.
type mediaSockets struct {
	rtpConn  *net.UDPConn
	rtcpConn *net.UDPConn
	rtpPeer  *net.UDPAddr
	rtcpPeer *net.UDPAddr

	clientRTPPort  int
	clientRTCPPort int

	closeOnce sync.Once
	closeErr  error
}

// openMediaSockets binds a UDP socket pair on the local unspecified address,
// choosing a consecutive client_port pair (RTP, then RTCP at RTP+1) from the
// OS ephemeral range with a bounded number of retries. It returns the pair
// with the chosen client ports recorded, or a wrapped ErrUDPSocket on
// failure. The server peers are filled in later by resolveServerPeers once
// SETUP returns.
func openMediaSockets() (*mediaSockets, error) {
	var lastErr error
	for attempt := 0; attempt < maxSocketBindRetries; attempt++ {
		rtpConn, err := net.ListenUDP("udp", nil)
		if err != nil {
			lastErr = err
			continue
		}
		rtpAddr, ok := rtpConn.LocalAddr().(*net.UDPAddr)
		if !ok {
			// Unreachable for a socket this same call just opened; kept as a
			// defensive bound on the loop rather than a panic.
			_ = rtpConn.Close()
			lastErr = fmt.Errorf("unexpected local address type %T", rtpConn.LocalAddr())
			continue
		}
		if rtpAddr.Port >= maxUDPPort {
			// No room for RTP+1; retry with a fresh ephemeral port rather
			// than wrapping into an invalid or unintended port number.
			_ = rtpConn.Close()
			lastErr = fmt.Errorf("ephemeral port %d leaves no room for client_port+1", rtpAddr.Port)
			continue
		}
		rtcpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: rtpAddr.IP, Port: rtpAddr.Port + 1})
		if err != nil {
			_ = rtpConn.Close()
			lastErr = err
			continue
		}
		return &mediaSockets{
			rtpConn:        rtpConn,
			rtcpConn:       rtcpConn,
			clientRTPPort:  rtpAddr.Port,
			clientRTCPPort: rtpAddr.Port + 1,
		}, nil
	}
	return nil, fmt.Errorf("%w: %w", ErrUDPSocket, lastErr)
}

// resolveServerPeers records the server RTP and RTCP UDP addresses from the
// SETUP Transport response server_port, always paired with the
// control-connection peer IP. It returns ErrUDPSetupRejected when the response
// carries no usable server_port, or when controlPeerIP is nil.
//
// M6 is unicast only, so the media source is always the control-connection
// peer. The Transport source= parameter is a multicast-origin hint (multicast
// is deferred) and is deliberately ignored: for a unicast session it names
// nothing but a reflection target, so honoring a server-supplied source= would
// let a malicious or MITM server aim this client's hole-punch datagrams and
// periodic RTCP Receiver Reports at an arbitrary victim IP.
//
// controlPeerIP is the sole media-source IP, so a nil one (an unparseable
// control-connection remote address) is fatal: it would record nil peer IPs,
// making fromPeer drop all inbound media and holePunch/RTCP writes target the
// wildcard address. Rejecting here routes setupUDP through its existing
// resolveServerPeers-rejection path, which tears the accepted stream down.
//
//nolint:gocritic // value receiver matches ServerPorts and InterleavedChannels in transport.go: TransportHeader is a small stateless header value, not a hot-path allocation.
func (m *mediaSockets) resolveServerPeers(th TransportHeader, controlPeerIP net.IP) error {
	if controlPeerIP == nil {
		return ErrUDPSetupRejected
	}
	rtpPort, rtcpPort, ok := th.ServerPorts()
	if !ok {
		return ErrUDPSetupRejected
	}
	m.rtpPeer = &net.UDPAddr{IP: controlPeerIP, Port: rtpPort}
	m.rtcpPeer = &net.UDPAddr{IP: controlPeerIP, Port: rtcpPort}
	return nil
}

// holePunch sends natPunchCount best-effort datagrams to open the return
// path: a zero-length datagram on the RTP socket and the caller-supplied RTCP
// Receiver Report rr on the RTCP socket, to the resolved server peers.
// Errors are logged and ignored: a NAT that needs no punching, or one this
// cannot punch, must not fail Setup over it. It is called after
// resolveServerPeers and before Play.
func (m *mediaSockets) holePunch(rr []byte, logger *slog.Logger) {
	for i := 0; i < natPunchCount; i++ {
		if _, err := m.rtpConn.WriteToUDP(nil, m.rtpPeer); err != nil {
			logWarn(logger, "UDP hole punch: RTP datagram failed", "error", err)
		}
		if _, err := m.rtcpConn.WriteToUDP(rr, m.rtcpPeer); err != nil {
			logWarn(logger, "UDP hole punch: RTCP datagram failed", "error", err)
		}
	}
}

// Close drops both UDP sockets. It is idempotent and safe from any goroutine;
// closing a socket unblocks a receive goroutine parked in ReadFromUDP.
func (m *mediaSockets) Close() error {
	m.closeOnce.Do(func() {
		rtpErr := m.rtpConn.Close()
		rtcpErr := m.rtcpConn.Close()
		if rtpErr != nil {
			m.closeErr = rtpErr
			return
		}
		m.closeErr = rtcpErr
	})
	return m.closeErr
}

// remoteIP extracts the IP address from a control connection's remote
// address, for resolveServerPeers' controlPeerIP fallback when a SETUP
// response carries no source parameter. It returns nil when addr is nil or
// carries no parseable host, which is not expected from a live TCP or TLS
// connection's RemoteAddr but is handled rather than assumed away.
func remoteIP(addr net.Addr) net.IP {
	if addr == nil {
		return nil
	}
	if tcpAddr, ok := addr.(*net.TCPAddr); ok {
		return tcpAddr.IP
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}
