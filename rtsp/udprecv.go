package rtsp

import (
	"errors"
	"fmt"
	"net"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
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
		if !c.armMediaReadDeadline(m.rtpConn, true) {
			return // shutdown has begun.
		}
		n, _, err := m.rtpConn.ReadFromUDP(buf)
		if err != nil {
			c.classifyMediaReadErr(err)
			return
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
		released = reorder.Push(pkt.Header.SequenceNumber, cp, released[:0])
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
		if !c.armMediaReadDeadline(m.rtcpConn, false) {
			return // shutdown has begun.
		}
		n, _, err := m.rtcpConn.ReadFromUDP(buf)
		if err != nil {
			return
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
// per discard track, one per socket, so a socket close unblocks each. Like the
// RTP receiver it stops once shutdown arms the immediate deadline, and RTCP is
// advisory so a read error just ends the goroutine.
func (c *Client) runDiscardReceiver(tr *track, conn *net.UDPConn, isRTCP bool) {
	defer c.udpWG.Done()
	defer c.recoverReceiver()

	buf := make([]byte, maxDatagramSize)
	for {
		if !c.armMediaReadDeadline(conn, false) {
			return // shutdown has begun.
		}
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
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

// armMediaReadDeadline sets a media socket's read deadline for the next read,
// under deadlineMu so it can never race past a shutdown, mirroring the control
// connection's armReadDeadline. It reports whether the caller should proceed
// with the read: false once shutdown has begun, when it installs an immediate
// deadline so a parked read returns at once. When watch is set (the RTP socket)
// and the session is playing with ReadIdle > 0, it arms the read-idle watchdog
// at now+ReadIdle; the RTCP and discard sockets pass watch false, since RTCP is
// sparse and a watchdog there would fire on a healthy stream.
func (c *Client) armMediaReadDeadline(conn *net.UDPConn, watch bool) bool {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	if c.shuttingDown {
		_ = conn.SetReadDeadline(time.Now())
		return false
	}
	var deadline time.Time
	if watch && c.playing.Load() && c.cfg.ReadIdle > 0 {
		deadline = time.Now().Add(c.cfg.ReadIdle)
	}
	_ = conn.SetReadDeadline(deadline)
	return true
}

// classifyMediaReadErr funnels the terminal cause of an RTP-socket read error,
// mirroring classifyReadErr but funneling directly since the receive goroutine
// has no framing loop to return through. When shutdown is already in progress
// the recorded cause wins and this is a no-op. Otherwise a watchdog timeout
// while playing is audiostream.ErrReadTimeout, and any other error is
// ErrConnectionClosed wrapping the cause.
func (c *Client) classifyMediaReadErr(err error) {
	select {
	case <-c.closing:
		return // shutdown already funneled; the recorded termErr is the cause.
	default:
	}
	var nerr net.Error
	if c.playing.Load() && errors.As(err, &nerr) && nerr.Timeout() {
		c.initiateShutdown(audiostream.ErrReadTimeout)
		return
	}
	c.initiateShutdown(fmt.Errorf("%w: %w", ErrConnectionClosed, err))
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
