package rtsp_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/internal/testserver"
)

// buildRTPPacketCSRC is buildRTPPacket with a CSRC list, so a test can prove
// PayloadBytes strips the true (CC-extended) header rather than a fixed 12
// bytes: each CSRC adds 4 bytes to the header that WireBytes counts and
// PayloadBytes does not.
func buildRTPPacketCSRC(pt uint8, seq uint16, ts, ssrc uint32, marker bool, csrc []uint32, payload []byte) []byte {
	h := make([]byte, 12, 12+len(csrc)*4+len(payload))
	h[0] = 0x80 | byte(len(csrc)) // version 2, CC = len(csrc)
	h[1] = pt
	if marker {
		h[1] |= 0x80
	}
	binary.BigEndian.PutUint16(h[2:], seq)
	binary.BigEndian.PutUint32(h[4:], ts)
	binary.BigEndian.PutUint32(h[8:], ssrc)
	for _, c := range csrc {
		h = binary.BigEndian.AppendUint32(h, c)
	}
	return append(h, payload...)
}

// PayloadBytes strips the full RTP header (CSRC list included), so it counts
// only the codec payload; WireBytes counts every byte on the RTP channel: the
// interleaved framing header plus the whole RTP frame. The third packet carries
// one CSRC, so its real header is 16 bytes, which pins that PayloadBytes strips
// the true header rather than a fixed 12.
func TestActiveByteAccountingPayloadAndWire(t *testing.T) {
	p1 := []byte{0x11, 0x22}
	p2 := []byte{0x33, 0x44, 0x55}
	p3 := []byte{0x66, 0x77, 0x88, 0x99}
	f1 := buildRTPPacket(ptOpus, 1, 960, 0x01, false, p1)
	f2 := buildRTPPacket(ptOpus, 2, 1920, 0x01, false, p2)
	f3 := buildRTPPacketCSRC(ptOpus, 3, 2880, 0x01, false, []uint32{0xDEADBEEF}, p3)

	wantPayload := uint64(len(p1) + len(p2) + len(p3))
	wantWire := uint64((4 + len(f1)) + (4 + len(f2)) + (4 + len(f3)))

	c, _ := playAndInject(t, &testserver.HandshakeConfig{SDP: opusSDP}, nil,
		func(sc *testserver.ServerConn, pairs []testserver.ChannelPair) {
			_ = sc.InjectFrame(pairs[0].RTP, f1)
			_ = sc.InjectFrame(pairs[0].RTP, f2)
			_ = sc.InjectFrame(pairs[0].RTP, f3)
		})
	defer closeAndWait(t, c)

	// Packets only advances, so the predicate cannot approve a torn snapshot;
	// once it reaches 3 every byte counter for those packets has settled.
	st := waitForStats(t, c, 0, func(ts audiostream.TrackStats) bool { return ts.Packets == 3 })
	if st.PayloadBytes != wantPayload {
		t.Errorf("PayloadBytes = %d, want %d (the CSRC-extended header must be stripped)", st.PayloadBytes, wantPayload)
	}
	if st.WireBytes != wantWire {
		t.Errorf("WireBytes = %d, want %d (interleaved header + full RTP frame per packet)", st.WireBytes, wantWire)
	}
	if st.WireBytes <= st.PayloadBytes {
		t.Errorf("WireBytes %d must exceed PayloadBytes %d by the header and framing overhead", st.WireBytes, st.PayloadBytes)
	}
	if st.LastFrameAt.IsZero() {
		t.Error("LastFrameAt is zero after frames arrived")
	}
	if snap := c.Stats(); st.LastFrameAt.After(snap.CapturedAt) {
		t.Errorf("LastFrameAt %v is after CapturedAt %v (negative age)", st.LastFrameAt, snap.CapturedAt)
	}
}

// A discard track never parses its frames, so PayloadBytes stays zero while
// WireBytes and Packets count the RTP-channel frames. An RTCP compound on the
// discard pair's RTCP channel must move none of them: before the
// isRTCP-before-discard restructure it was miscounted as media on the discard
// branch.
func TestDiscardByteAccountingAndRTCPNotCounted(t *testing.T) {
	// audioVideoSDP: track 0 audio (AAC, active), track 1 video (discarded).
	v1 := buildRTPPacket(ptH264, 1, 90000, 0x0F, true, []byte{0xde, 0xad, 0xbe, 0xef})
	v2 := buildRTPPacket(ptH264, 2, 93600, 0x0F, true, []byte{0xca, 0xfe})
	// A well-formed 8-byte Receiver Report compound on the discard track's RTCP
	// channel.
	rtcp := []byte{0x80, 0xC9, 0x00, 0x01, 0xCA, 0xFE, 0xBA, 0xBE}
	// The trailing audio frame is the ordered sync point: once it is delivered,
	// every earlier video and RTCP frame on the same TCP connection has been
	// processed, and track 1's synchronous counters have settled.
	au := []byte{0x01, 0x02}

	wantWire := uint64((4 + len(v1)) + (4 + len(v2)))

	c, frames := playAndInject(t, &testserver.HandshakeConfig{SDP: audioVideoSDP},
		func(i int) bool { return i == 1 }, // discard the video track
		func(sc *testserver.ServerConn, pairs []testserver.ChannelPair) {
			_ = sc.InjectFrame(pairs[1].RTP, v1)
			_ = sc.InjectFrame(pairs[1].RTCP, rtcp)
			_ = sc.InjectFrame(pairs[1].RTP, v2)
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptAAC, 1, 8000, 0xA0, true, aacHbrPayload(au)))
		})
	defer closeAndWait(t, c)

	f := recvFrame(t, frames)
	if f.TrackID != 0 || !bytes.Equal(f.Data, au) {
		t.Fatalf("sync frame = track %d data % x, want track 0 data % x", f.TrackID, f.Data, au)
	}
	st := c.Stats().Tracks[1]
	if st.Packets != 2 {
		t.Errorf("discard track Packets = %d, want 2 (the RTCP compound must not be counted)", st.Packets)
	}
	if st.PayloadBytes != 0 {
		t.Errorf("discard track PayloadBytes = %d, want 0 (a discard track is never parsed)", st.PayloadBytes)
	}
	if st.WireBytes != wantWire {
		t.Errorf("discard track WireBytes = %d, want %d (RTP frames only, RTCP excluded)", st.WireBytes, wantWire)
	}
	if st.LastFrameAt.IsZero() {
		t.Error("discard track LastFrameAt is zero, want the arrival of the last video frame")
	}
	if snap := c.Stats(); st.LastFrameAt.After(snap.CapturedAt) {
		t.Errorf("discard track LastFrameAt %v is after CapturedAt %v (negative age)", st.LastFrameAt, snap.CapturedAt)
	}
}

// A malformed frame on the active track still counts as wire traffic and
// liveness (WireBytes and LastFrameAt advance), but contributes no PayloadBytes
// and is not counted as an accepted Packet; Malformed explains the delta.
func TestMalformedActiveByteAccounting(t *testing.T) {
	p1 := []byte{0x11, 0x22}
	p2 := []byte{0x33, 0x44}
	f1 := buildRTPPacket(ptOpus, 1, 960, 0x01, false, p1)
	// Too short to be an RTP packet (< 12 bytes): rejected by ParsePacket.
	bad := []byte{0x80, 0x60, 0x00}
	f2 := buildRTPPacket(ptOpus, 2, 1920, 0x01, false, p2)

	wantPayload := uint64(len(p1) + len(p2))
	wantWire := uint64((4 + len(f1)) + (4 + len(bad)) + (4 + len(f2)))

	c, _ := playAndInject(t, &testserver.HandshakeConfig{SDP: opusSDP}, nil,
		func(sc *testserver.ServerConn, pairs []testserver.ChannelPair) {
			_ = sc.InjectFrame(pairs[0].RTP, f1)
			_ = sc.InjectFrame(pairs[0].RTP, bad)
			_ = sc.InjectFrame(pairs[0].RTP, f2)
		})
	defer closeAndWait(t, c)

	// Both predicates only advance, so the snapshot cannot be torn.
	st := waitForStats(t, c, 0, func(ts audiostream.TrackStats) bool {
		return ts.Packets == 2 && ts.Malformed == 1
	})
	if st.PayloadBytes != wantPayload {
		t.Errorf("PayloadBytes = %d, want %d (the malformed frame contributes no payload)", st.PayloadBytes, wantPayload)
	}
	if st.WireBytes != wantWire {
		t.Errorf("WireBytes = %d, want %d (the malformed frame still counts as wire bytes)", st.WireBytes, wantWire)
	}
	if st.LastFrameAt.IsZero() {
		t.Error("LastFrameAt is zero after frames arrived")
	}
}

// CapturedAt is stamped when a Stats read completes: it is never zero after a
// call, never runs backward between two snapshots, and is never earlier than
// any track's LastFrameAt (which would be a negative age).
func TestStatsCapturedAt(t *testing.T) {
	c, _ := playAndInject(t, &testserver.HandshakeConfig{SDP: opusSDP}, nil,
		func(sc *testserver.ServerConn, pairs []testserver.ChannelPair) {
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptOpus, 1, 960, 0x01, false, []byte{0x11, 0x22}))
		})
	defer closeAndWait(t, c)

	waitForStats(t, c, 0, func(ts audiostream.TrackStats) bool { return ts.Packets == 1 })
	first := c.Stats()
	if first.CapturedAt.IsZero() {
		t.Fatal("CapturedAt is zero after a Stats call")
	}
	second := c.Stats()
	if second.CapturedAt.Before(first.CapturedAt) {
		t.Error("second snapshot CapturedAt is before the first")
	}
	if d := second.CapturedAt.Sub(first.CapturedAt); d < 0 {
		t.Errorf("CapturedAt delta = %v, want >= 0", d)
	}
	for id, ts := range second.Tracks {
		if !ts.LastFrameAt.IsZero() && ts.LastFrameAt.After(second.CapturedAt) {
			t.Errorf("track %d LastFrameAt %v is after CapturedAt %v (negative age)", id, ts.LastFrameAt, second.CapturedAt)
		}
	}
}

// LastFrameAt is the zero Time (never the 1970 epoch) until the first frame on
// the track's RTP channel arrives, and every counter is zero alongside it.
func TestLastFrameAtZeroBeforeFrame(t *testing.T) {
	c, _ := playAndInject(t, &testserver.HandshakeConfig{SDP: opusSDP}, nil,
		func(sc *testserver.ServerConn, pairs []testserver.ChannelPair) {
			// Inject nothing: the track is set up but no media has arrived.
		})
	defer closeAndWait(t, c)

	st := c.Stats().Tracks[0]
	if !st.LastFrameAt.IsZero() {
		t.Errorf("LastFrameAt = %v, want the zero Time before any frame", st.LastFrameAt)
	}
	if st.Packets != 0 || st.WireBytes != 0 || st.PayloadBytes != 0 {
		t.Errorf("counters = {Packets:%d WireBytes:%d PayloadBytes:%d}, want all zero before any frame", st.Packets, st.WireBytes, st.PayloadBytes)
	}
}
