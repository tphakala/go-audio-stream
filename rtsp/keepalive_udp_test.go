package rtsp

import (
	"encoding/binary"
	"net"
	"testing"
	"time"
)

// rtcpReportIntervalUDP must scale rtcpReportInterval by a factor in
// [0.5, 1.5] per RFC 3550 section 6.3.1, and the randomization must actually
// vary the result rather than always returning the same value.
func TestKeepaliveUDPReportIntervalWithinRFC3550Bounds(t *testing.T) {
	t.Parallel()
	const lo = rtcpReportInterval / 2
	const hi = rtcpReportInterval + rtcpReportInterval/2

	seen := make(map[time.Duration]bool)
	for range 200 {
		got := rtcpReportIntervalUDP()
		if got < lo || got > hi {
			t.Fatalf("rtcpReportIntervalUDP() = %v, want within [%v, %v]", got, lo, hi)
		}
		seen[got] = true
	}
	if len(seen) < 2 {
		t.Error("rtcpReportIntervalUDP() returned the same value every time, want randomization applied")
	}
}

// decodeUDPReceiverReport pulls the reporter SSRC and first block's SSRC and
// highest sequence out of a marshaled RTCP Receiver Report, matching the wire
// layout rtp.ReceiverReport.Marshal produces (RFC 3550 section 6.4.2). It is a
// direct byte assertion rather than a shared parser: keepalive_test.go's
// parseReceiverReport lives in the external rtsp_test package and is not
// reachable from here.
func decodeUDPReceiverReport(t *testing.T, payload []byte) (reporterSSRC, blockSSRC, highestSeq uint32) {
	t.Helper()
	if len(payload) < 8+24 {
		t.Fatalf("receiver report too short: %d bytes", len(payload))
	}
	if payload[0]>>6 != 2 {
		t.Fatalf("RTCP version = %d, want 2", payload[0]>>6)
	}
	if payload[1] != 201 {
		t.Fatalf("RTCP packet type = %d, want 201 (Receiver Report)", payload[1])
	}
	if payload[0]&0x1f != 1 {
		t.Fatalf("report count = %d, want 1", payload[0]&0x1f)
	}
	reporterSSRC = binary.BigEndian.Uint32(payload[4:8])
	blockSSRC = binary.BigEndian.Uint32(payload[8:12])
	highestSeq = binary.BigEndian.Uint32(payload[16:20])
	return reporterSSRC, blockSSRC, highestSeq
}

// sendReceiverReportsUDP must emit one RTCP Receiver Report datagram per
// non-discard track, addressed to that track's resolved RTCP peer, built from
// the same atomic snapshot the interleaved sendReceiverReports reads.
func TestKeepaliveUDPSendReceiverReportsEmitsDatagram(t *testing.T) {
	t.Parallel()
	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenUDP rtcp peer: %v", err)
	}
	defer func() { _ = peer.Close() }()

	m, err := openMediaSockets()
	if err != nil {
		t.Fatalf("openMediaSockets: %v", err)
	}
	defer func() { _ = m.Close() }()
	m.rtcpPeer, _ = peer.LocalAddr().(*net.UDPAddr)

	tr := &track{id: 0}
	tr.senderSSRC.Store(0x0BADF00D)
	tr.rrHighestSeq.Store(101)

	c := &Client{
		cfg:          Config{Timeout: 2 * time.Second},
		tracks:       []*track{tr},
		reporterSSRC: 0xCAFEBABE,
		media:        map[int]*mediaSockets{0: m},
	}

	c.sendReceiverReportsUDP(time.Now())

	if err := peer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, maxDatagramSize)
	n, _, rerr := peer.ReadFromUDP(buf)
	if rerr != nil {
		t.Fatalf("reading receiver report datagram: %v", rerr)
	}

	reporterSSRC, blockSSRC, highestSeq := decodeUDPReceiverReport(t, buf[:n])
	if reporterSSRC != 0xCAFEBABE {
		t.Errorf("ReporterSSRC = %#x, want %#x", reporterSSRC, uint32(0xCAFEBABE))
	}
	if blockSSRC != 0x0BADF00D {
		t.Errorf("block SSRC = %#x, want %#x", blockSSRC, uint32(0x0BADF00D))
	}
	if highestSeq != 101 {
		t.Errorf("HighestSequence = %d, want 101", highestSeq)
	}
}

// A discard track must not receive a Receiver Report over UDP, matching the
// interleaved path's rule.
func TestKeepaliveUDPSendReceiverReportsSkipsDiscardTracks(t *testing.T) {
	t.Parallel()
	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenUDP rtcp peer: %v", err)
	}
	defer func() { _ = peer.Close() }()

	m, err := openMediaSockets()
	if err != nil {
		t.Fatalf("openMediaSockets: %v", err)
	}
	defer func() { _ = m.Close() }()
	m.rtcpPeer, _ = peer.LocalAddr().(*net.UDPAddr)

	tr := &track{id: 0, discard: true}

	c := &Client{
		tracks: []*track{tr},
		media:  map[int]*mediaSockets{0: m},
	}

	c.sendReceiverReportsUDP(time.Now())

	if err := peer.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, maxDatagramSize)
	if _, _, rerr := peer.ReadFromUDP(buf); rerr == nil {
		t.Error("sendReceiverReportsUDP sent a datagram for a discard track, want none")
	}
}
