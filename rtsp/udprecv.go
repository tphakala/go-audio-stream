package rtsp

import (
	"fmt"
	"net"
	"time"

	"github.com/tphakala/go-audio-stream/rtsp/rtp"
)

// runRTPReceiver is one track's RTP receive goroutine in UDP transport mode.
// It reads datagrams from the track's RTP socket, resequences them through the
// track's Reorderer, and drains released packets through c.process in sequence
// order, so UDP media reaches OnFrame through exactly the same pipeline the TCP
// interleaved reader uses.
//
// Every datagram, parsed or not, stamps the wire and liveness fields BEFORE the
// Reorderer, so a dropped, late, or malformed datagram still counts as wire
// traffic and proves the peer is alive. A change of SSRC flushes and resets the
// Reorderer (its 16-bit sequence space restarts with the new source) and lets
// the normal path re-baseline. The Reorderer retains the slice it is handed, so
// each datagram is copied before Push and re-parsed on release, keeping every
// Released.Payload a self-contained RTP packet.
//
// It exits deterministically at shutdown: the socket close plus the immediate
// read deadline initiateShutdown arms unblock a parked ReadFromUDP, and the
// deadline-arming helper returns false once shutdown has begun. A read timeout
// while playing funnels audiostream.ErrReadTimeout (the read-idle watchdog, D5);
// a read that fails after shutdown began keeps the recorded terminal cause; any
// other read error funnels ErrConnectionClosed. A panic anywhere in the loop is
// funneled into shutdown rather than crashing the host process.
func (c *Client) runRTPReceiver(tr *track, m *mediaSockets) {
	defer c.udpWG.Done()
	defer c.recoverReceiver()

	buf := make([]byte, maxDatagramSize)
	var reorder rtp.Reorderer
	var released []rtp.Released
	var lastSSRC uint32
	var haveSSRC bool

	for {
		// Anchor the watchdog to this track's last accepted-from-peer datagram,
		// so foreign or malformed traffic that ReadFromUDP returns but the loop
		// drops cannot refresh it.
		if !c.armMediaReadDeadline(m.rtpConn, true, tr.lastFrameUnixNano.Load()) {
			return // shutdown has begun.
		}
		n, addr, err := m.rtpConn.ReadFromUDP(buf)
		if err != nil {
			c.classifyMediaReadErr(err)
			return
		}
		if !fromPeer(addr, m.rtpPeer.IP) {
			// Dropped before any accounting or processing: the socket is bound to
			// the wildcard address, so any host can target this ephemeral port,
			// and an off-path forgery must not count as bandwidth, keep the
			// watchdog alive, reach the Reorderer, or trip an SSRC reset (or
			// inject counterfeit audio) inside process.
			continue
		}
		now := time.Now()
		// Wire and liveness evidence for every datagram, stamped before the
		// Reorderer so a datagram that is later dropped, held, or rejected still
		// counts as bandwidth and as proof the track is producing. c.lastFrameAt
		// feeds the control connection's read-idle watchdog, which sees no media
		// of its own in UDP mode.
		c.lastFrameAt.Store(now.UnixNano())
		tr.lastFrameUnixNano.Store(now.UnixNano())
		tr.wireBytes.Add(uint64(n))

		datagram := buf[:n]
		pkt, perr := rtp.ParsePacket(datagram)
		if perr != nil {
			tr.malformed.Add(1)
			continue
		}

		if haveSSRC && pkt.Header.SSRC != lastSSRC {
			// A new source restarts the sequence space, so the old source's
			// buffered packets must drain in order and the Reorderer reset
			// before the new packet establishes a fresh release point. process
			// re-baselines when Observe reports the SSRC change on release.
			released = reorder.Flush(released[:0])
			c.drainReleased(tr, released, now)
			reorder.Reset()
		}
		lastSSRC = pkt.Header.SSRC
		haveSSRC = true

		// The Reorderer retains the slice, and datagram aliases the reused read
		// buffer, so hand Push a copy; re-parsing it on release keeps
		// Released.Payload a self-contained RTP packet.
		cp := make([]byte, n)
		copy(cp, datagram)
		// Bracket Push with its late counter so a duplicate or too-late datagram
		// the Reorderer drops inside Push, before Stream.Observe ever sees it,
		// still counts as a duplicate, giving UDP the same TrackStats.Duplicates
		// meaning as the TCP path, where Observe counts duplicates directly. The
		// delta accumulates in reorderDrops rather than duplicates because
		// publishRRSnapshot overwrites duplicates from the stream's own counter
		// on every released packet; Stats sums the two. Read Late immediately
		// before each Push: reorder.Reset on an SSRC change (above) zeroes it, so
		// a cached running baseline would go stale across a reset.
		lateBefore := reorder.Stats().Late
		released = reorder.Push(pkt.Header.SequenceNumber, cp, released[:0])
		// Push only ever increments Late, so lateAfter >= lateBefore holds today.
		// Compare rather than subtract-then-test: a plain d := after - before with
		// a d > 0 guard is not underflow-safe, since a decrease would wrap the
		// uint64 to a huge positive that passes the guard and inflates the count.
		// Guarding on lateAfter > lateBefore keeps it correct even if a future
		// change ever let Late fall between the two reads.
		if lateAfter := reorder.Stats().Late; lateAfter > lateBefore {
			tr.reorderDrops.Add(lateAfter - lateBefore)
		}
		c.drainReleased(tr, released, now)
	}
}

// drainReleased folds a run of Reorderer-released packets into the pipeline in
// sequence order. Each Released.Payload is a self-contained copy of a full RTP
// datagram, so it is re-parsed here; a copy that somehow fails to parse is
// counted as malformed and skipped rather than ending the session.
func (c *Client) drainReleased(tr *track, released []rtp.Released, now time.Time) {
	for i := range released {
		pkt, err := rtp.ParsePacket(released[i].Payload)
		if err != nil {
			tr.malformed.Add(1)
			continue
		}
		c.process(tr, pkt, now)
	}
}

// runRTCPReceiver is one track's RTCP receive goroutine in UDP transport mode.
// It reads RTCP datagrams and hands each to handleRTCP with a freshly stamped
// receive time, which records the latest Sender Report into the track's atomic
// RR-snapshot fields and publishes the sender-clock mapping. The baseSet gate
// handleRTCP reads is race-free because it is an atomic.Bool the RTP receive
// goroutine writes.
//
// RTCP is advisory, so this goroutine never funnels shutdown: a read error is
// almost always the socket closing at teardown, and a transient RTCP hiccup
// must not end a session whose media is arriving fine. It exits on any read
// error or once shutdown has begun.
func (c *Client) runRTCPReceiver(tr *track, m *mediaSockets) {
	defer c.udpWG.Done()
	defer c.recoverReceiver()

	buf := make([]byte, maxDatagramSize)
	for {
		// RTCP is unwatched (watch false), so the anchor is unused; pass 0.
		if !c.armMediaReadDeadline(m.rtcpConn, false, 0) {
			return // shutdown has begun.
		}
		n, addr, err := m.rtcpConn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if !fromPeer(addr, m.rtcpPeer.IP) {
			// An RTCP datagram from a host other than the negotiated peer is a
			// forgery targeting the wildcard-bound port; drop it so it cannot
			// steer the sender-clock mapping or the RR snapshot.
			continue
		}
		c.handleRTCP(tr, buf[:n], time.Now())
	}
}

// runDiscardReceiver drains one socket of a discard track, counting and
// dropping without depacketizing. On the RTP socket (isRTCP false) it counts
// wireBytes for every datagram and, applying handleInterleaved's discard shape
// check (a full RTP header and version 2), counts a packet, so discard-track
// counts stay consistent between TCP and UDP; the media-liveness stamps advance
// too, matching the TCP discard branch. On the RTCP socket (isRTCP true) it
// drains and attributes nothing, matching the TCP discard path. Play starts two
// per discard track, one per socket, so a socket close unblocks each. It stops
// once shutdown arms the immediate deadline. The RTP socket (isRTCP false) arms
// the ReadIdle watchdog and funnels a read error through classifyMediaReadErr,
// matching runRTPReceiver, so a session of only discard tracks still detects a
// dead peer and a genuine socket error is not swallowed; the RTCP socket
// (isRTCP true) is advisory, so it neither arms the watchdog nor funnels: a read
// error just ends the goroutine.
//
// peerIP is the negotiated media source for this socket (m.rtpPeer.IP or
// m.rtcpPeer.IP). A datagram from any other host is dropped before accounting,
// so an off-path attacker cannot inflate even a discard track's counters.
func (c *Client) runDiscardReceiver(tr *track, conn *net.UDPConn, peerIP net.IP, isRTCP bool) {
	defer c.udpWG.Done()
	defer c.recoverReceiver()

	buf := make([]byte, maxDatagramSize)
	for {
		// The RTP socket (watch true) anchors to accepted media; the RTCP socket
		// is unwatched and ignores the anchor.
		if !c.armMediaReadDeadline(conn, !isRTCP, tr.lastFrameUnixNano.Load()) {
			return // shutdown has begun.
		}
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			if !isRTCP {
				// The RTP socket mirrors runRTPReceiver: a watchdog timeout while
				// playing funnels ErrReadTimeout and any other non-shutdown error
				// funnels ErrConnectionClosed. RTCP is advisory, so it returns
				// silently.
				c.classifyMediaReadErr(err)
			}
			return
		}
		if !fromPeer(addr, peerIP) {
			continue // not from the negotiated peer; drop without accounting.
		}
		if isRTCP {
			// A discard track's RTCP is not media, is attributed to nothing, and
			// is not processed, exactly as the TCP discard path leaves it.
			continue
		}
		now := time.Now()
		c.lastFrameAt.Store(now.UnixNano())
		tr.lastFrameUnixNano.Store(now.UnixNano())
		tr.wireBytes.Add(uint64(n))
		// Shape-gated so a peer cannot inflate the packet count with garbage on
		// a discarded channel; no parse, matching the TCP discard branch.
		if n >= rtp.HeaderSize && buf[0]>>6 == rtpVersion {
			tr.packets.Add(1)
		}
	}
}

// fromPeer reports whether a received datagram's source address is the
// negotiated media peer. The UDP media sockets bind the wildcard address, so any
// host on the network can send to the client's ephemeral RTP/RTCP port; only
// datagrams whose source IP matches the resolved peer are treated as media. The
// match is on IP alone, not port: a server may legitimately send media from a
// source port other than the server_port it advertised, and resolveServerPeers
// already fixed the media source IP to the control-connection peer (for a
// unicast session the media source is always that peer). A nil addr (not
// expected from a successful ReadFromUDP) is treated as foreign rather than
// dereferenced.
func fromPeer(addr *net.UDPAddr, peerIP net.IP) bool {
	return addr != nil && addr.IP.Equal(peerIP)
}

// armMediaReadDeadline sets a media socket's read deadline for the next read,
// under deadlineMu so it can never race past a shutdown, mirroring the control
// connection's armReadDeadline. It reports whether the caller should proceed
// with the read: false once shutdown has begun, when it installs an immediate
// deadline so a parked read returns at once. When watch is set (any RTP socket,
// including a discard track's) and the session is playing with ReadIdle > 0, it
// arms the read-idle watchdog at anchorUnixNano+ReadIdle; the RTCP sockets pass
// watch false, since RTCP is sparse and a watchdog there would fire on a healthy
// stream.
//
// anchorUnixNano is the wall-clock time (UnixNano) of the last datagram this
// socket accepted from the negotiated peer (tr.lastFrameUnixNano), NOT the
// current time. Anchoring the deadline to accepted media, exactly as the
// control connection's armReadDeadline anchors to c.lastFrameAt, means a
// foreign-source or malformed datagram (which ReadFromUDP still returns, then
// the receiver drops) cannot refresh the watchdog: an off-path attacker who
// learns the client_port cannot hold a dead session open by sending one
// datagram per ReadIdle. The caller seeds tr.lastFrameUnixNano before the first
// read (see startUDPReceivers), so the first armed deadline is not a 1970 time.
func (c *Client) armMediaReadDeadline(conn *net.UDPConn, watch bool, anchorUnixNano int64) bool {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	if c.shuttingDown {
		_ = conn.SetReadDeadline(time.Now())
		return false
	}
	var deadline time.Time
	if watch && c.playing.Load() && c.cfg.ReadIdle > 0 {
		deadline = time.Unix(0, anchorUnixNano).Add(c.cfg.ReadIdle)
	}
	_ = conn.SetReadDeadline(deadline)
	return true
}

// classifyMediaReadErr funnels the terminal cause of an RTP-socket read error
// directly through initiateShutdown, since the receive goroutine has no
// framing loop to return through: classifyReadErr does the classification
// (the recorded cause when shutdown is already in progress, ErrReadTimeout on
// a watchdog timeout while playing, or ErrConnectionClosed otherwise), and
// initiateShutdown is a guaranteed no-op when shutdown has already begun.
func (c *Client) classifyMediaReadErr(err error) {
	c.initiateShutdown(c.classifyReadErr(err))
}

// recoverReceiver turns a panic in a UDP receive goroutine into a fatal
// shutdown cause, so a bug on the receive path ends the session cleanly instead
// of crashing the host process. It is the receive-side counterpart of
// recoverReader.
func (c *Client) recoverReceiver() {
	if r := recover(); r != nil {
		c.initiateShutdown(fmt.Errorf("rtsp: udp receiver panic: %v", r))
	}
}
