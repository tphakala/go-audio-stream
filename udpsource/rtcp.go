package udpsource

import (
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/rtsp/rtp"
)

// rtcpReader is the receive goroutine for the separate RTCP socket
// (Config.RTCPListenAddr). It reads RTCP datagrams and feeds each to handleRTCP
// until the socket is closed by shutdown. RTCP is advisory, so a read error just
// ends this goroutine and never funnels shutdown: the media stream governs the
// session's life. It arms no read-idle deadline; Sender Reports are irregular
// and their absence is not a media stall. The SourceIP filter applies here too,
// mirroring the media path.
func (c *Client) rtcpReader() {
	defer c.rtcpWG.Done()
	defer c.recoverRTCP()
	buf := make([]byte, c.cfg.readBufSize)
	for {
		select {
		case <-c.closing:
			return
		default:
		}
		n, addr, err := c.rtcpConn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if c.srcIP != nil && !addr.IP.Equal(c.srcIP) {
			continue
		}
		c.handleRTCP(buf[:n], time.Now())
	}
}

// recoverRTCP turns a panic on the RTCP path into a logged non-event. Unlike the
// reader's recover it does not funnel shutdown: an advisory diagnostic path must
// never crash the process or end the media session. It guards both the
// separate-socket receive goroutine and each handleRTCP call (so the mux path,
// which runs handleRTCP on the reader goroutine, does not reach the reader's
// shutdown-funnelling recover).
func (c *Client) recoverRTCP() {
	if r := recover(); r != nil && c.cfg.Logger != nil {
		c.cfg.Logger.Error("udpsource: RTCP receiver panic", "recovered", r)
	}
}

// isRTCP reports whether a datagram on a shared RTP/RTCP socket is RTCP, per RFC
// 5761. A compound RTCP packet begins with a Sender or Receiver Report (RFC
// 3550), so the packet-type byte is 200 (SR) or 201 (RR). Config validation
// forbids RTCPMux with a media PayloadType in the reserved range 64-95, so a
// media RTP packet's second byte (marker | payloadType) can never reach 200-201
// and be misread as RTCP.
func isRTCP(datagram []byte) bool {
	return len(datagram) >= 2 && (datagram[1] == rtp.PTSenderReport || datagram[1] == rtp.PTReceiverReport)
}

// handleRTCP parses an RTCP compound packet and, when it carries a Sender Report
// for this stream's media source, publishes the RTP-to-wall-clock correspondence
// to TrackStats.SenderClock. It is a faithful port of the rtsp UDP path's Sender
// Report handling. RTCP is advisory: a parse error, an empty compound, a report
// for a different source, or a compound arriving before the media source is
// identified all leave the mapping unchanged, and nothing here ever ends the
// session or touches media delivery. Safe to call from either the RTCP goroutine
// (separate socket) or the reader goroutine (mux); it recovers from its own panic
// so that on the mux path a parse panic cannot reach the reader's
// shutdown-funnelling recover and end the media session.
func (c *Client) handleRTCP(datagram []byte, now time.Time) {
	defer c.recoverRTCP()
	reports, err := rtp.ParseCompound(datagram)
	if err != nil || len(reports) == 0 {
		return
	}
	// Serialize the source match and the publish with processRTP's SSRC-reset
	// identity swap: the parse above stays outside the lock (it is the expensive
	// part and touches no shared state), and rtcpMu makes the read-match-store below
	// indivisible so a report cannot be published against a source identity that a
	// concurrent reset is mid-way through replacing.
	c.rtcpMu.Lock()
	defer c.rtcpMu.Unlock()
	if !c.baseSet.Load() {
		// The RTP stream has not identified its media source yet, so there is no
		// SSRC to match a report against. A mixer or translator compound can carry a
		// contributing source's report, and publishing it would expose a foreign
		// wall clock; wait for the first accepted RTP packet.
		return
	}
	want := c.mediaSSRC.Load()
	var sr rtp.SenderReport
	found := false
	for i := range reports {
		if reports[i].SSRC == want {
			sr, found = reports[i], true
			break
		}
	}
	if !found {
		// No report describes the media source (only contributing sources of a
		// mixer, say). Record nothing rather than adopt a foreign mapping.
		return
	}
	if sr.NTPTimestamp == 0 {
		// RFC 3550 section 6.4.1: a sender with no wall clock sends an all-zero NTP
		// timestamp, which maps nothing. Clear any prior correspondence rather than
		// keep extrapolating a stale pair.
		c.srClock.Store(nil)
		return
	}
	c.srClock.Store(&audiostream.SenderClock{
		RTPTime:    sr.RTPTimestamp,
		NTPTime:    rtp.NTPTime(sr.NTPTimestamp),
		ReceivedAt: now,
		ClockRate:  c.cfg.ClockRate,
		Valid:      true,
	})
}
