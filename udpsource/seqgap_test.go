package udpsource

import (
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/rtsp/rtp"
)

// These tests cover the per-frame SeqGap carry across a packet that delivers no
// frame on a single-frame codec (Opus/G.711/L16), the udpsource analogue of the
// rtsp fix in PR #106. A packet lost immediately before a malformed single-frame
// packet is counted in the aggregate TrackStats.SeqGaps by processRTP, but must
// also surface on the next delivered frame's Frame.SeqGap instead of vanishing.

// TestRTPOpusStrandedGapSurfacesOnNextFrame drives a valid Opus frame, then a
// malformed (empty-payload) packet preceded by a loss, then a valid frame. The
// stranded gap must appear on the next delivered frame, not be dropped.
func TestRTPOpusStrandedGapSurfacesOnNextFrame(t *testing.T) {
	var col collector
	c := openOK(t, Config{
		Mode: ModeRTP, PayloadType: 111, Codec: audiostream.CodecOpus{},
		ClockRate: 48000, OnFrame: col.onFrame,
	})
	defer func() { _ = c.Close() }()

	conn := senderFor(t, c)
	sendAndSettle(t, c, conn, rtpPacket(111, 100, 0, 1, []byte{0x78, 1, 2, 3}))    // valid
	sendAndSettle(t, c, conn, rtpPacket(111, 102, 960, 1, nil))                    // empty -> malformed; seq 101 lost -> gap 1
	sendAndSettle(t, c, conn, rtpPacket(111, 103, 1920, 1, []byte{0x78, 4, 5, 6})) // valid
	waitCount(t, &col, 2, 2*time.Second)

	frames := col.snapshot()
	if len(frames) != 2 {
		t.Fatalf("delivered %d frames, want 2 (the empty-payload packet must not yield a frame)", len(frames))
	}
	if frames[0].SeqGap != 0 {
		t.Errorf("first frame SeqGap = %d, want 0", frames[0].SeqGap)
	}
	if frames[1].SeqGap != 1 {
		t.Errorf("gap stranded on the malformed packet did not carry: SeqGap = %d, want 1", frames[1].SeqGap)
	}
	if ts := c.Stats().Tracks[0]; ts.SeqGaps != 1 {
		t.Errorf("TrackStats.SeqGaps = %d, want 1", ts.SeqGaps)
	}
	if ts := c.Stats().Tracks[0]; ts.Malformed == 0 {
		t.Errorf("Malformed = 0, want >0 for the empty-payload packet")
	}
}

// TestRTPL16StrandedGapSurfacesOnNextFrame is the L16 analogue: a sub-frame
// payload (fewer bytes than one whole sample-frame) yields no frame, so a loss
// stranded on it must carry to the next valid frame.
func TestRTPL16StrandedGapSurfacesOnNextFrame(t *testing.T) {
	var col collector
	c := openOK(t, Config{
		Mode: ModeRTP, PayloadType: 96, Codec: audiostream.CodecL16{ClockRate: 44100, Channels: 2},
		ClockRate: 44100, Channels: 2, OnFrame: col.onFrame, // frameBytes = 4
	})
	defer func() { _ = c.Close() }()

	conn := senderFor(t, c)
	sendAndSettle(t, c, conn, rtpPacket(96, 100, 0, 1, []byte{0x12, 0x34, 0x56, 0x78})) // one whole stereo frame, valid
	sendAndSettle(t, c, conn, rtpPacket(96, 102, 0, 1, []byte{0x12, 0x34}))             // 2 bytes < 4 -> usable 0 -> malformed; seq 101 lost -> gap 1
	sendAndSettle(t, c, conn, rtpPacket(96, 103, 0, 1, []byte{0x11, 0x22, 0x33, 0x44})) // valid
	waitCount(t, &col, 2, 2*time.Second)

	frames := col.snapshot()
	if len(frames) != 2 {
		t.Fatalf("delivered %d frames, want 2 (the sub-frame packet must not yield a frame)", len(frames))
	}
	if frames[1].SeqGap != 1 {
		t.Errorf("gap stranded on the sub-frame packet did not carry: SeqGap = %d, want 1", frames[1].SeqGap)
	}
	if ts := c.Stats().Tracks[0]; ts.SeqGaps != 1 {
		t.Errorf("TrackStats.SeqGaps = %d, want 1", ts.SeqGaps)
	}
	if ts := c.Stats().Tracks[0]; ts.Malformed == 0 {
		t.Errorf("Malformed = 0, want >0 for the sub-frame packet")
	}
}

// TestRTPG711CarriesGapAndCounts covers G.711's per-frame SeqGap. G.711 has no
// reachable no-frame malformed path in udpsource: an empty payload decodes to a
// zero-length frame that is delivered and drains its own gap, and a
// too-small-destination or unknown-law error cannot occur here (ensurePCMBuf
// always sizes the destination and the law is fixed at Open). So G.711 never
// strands a gap; this asserts it still reports its own observed gap on the next
// frame, matching the single-frame contract.
func TestRTPG711CarriesGapAndCounts(t *testing.T) {
	var col collector
	c := openOK(t, Config{
		Mode: ModeRTP, PayloadType: 0, Codec: audiostream.CodecG711{Law: audiostream.MuLaw},
		ClockRate: 8000, Channels: 1, OnFrame: col.onFrame,
	})
	defer func() { _ = c.Close() }()

	conn := senderFor(t, c)
	sendAndSettle(t, c, conn, rtpPacket(0, 10, 0, 1, []byte{0x2A}))   // valid
	sendAndSettle(t, c, conn, rtpPacket(0, 13, 160, 1, []byte{0x2B})) // seq 11,12 lost -> gap 2
	waitCount(t, &col, 2, 2*time.Second)

	frames := col.snapshot()
	if len(frames) != 2 {
		t.Fatalf("delivered %d frames, want 2", len(frames))
	}
	if frames[1].SeqGap != 2 {
		t.Errorf("second frame SeqGap = %d, want 2", frames[1].SeqGap)
	}
	if ts := c.Stats().Tracks[0]; ts.SeqGaps != 2 {
		t.Errorf("TrackStats.SeqGaps = %d, want 2", ts.SeqGaps)
	}
}

// TestRTPSingleFrameAccumulatesStrandedGaps confirms that gaps stranded on
// several consecutive no-frame packets sum and drain together on the next
// delivered frame.
func TestRTPSingleFrameAccumulatesStrandedGaps(t *testing.T) {
	var col collector
	c := openOK(t, Config{
		Mode: ModeRTP, PayloadType: 111, Codec: audiostream.CodecOpus{},
		ClockRate: 48000, OnFrame: col.onFrame,
	})
	defer func() { _ = c.Close() }()

	conn := senderFor(t, c)
	sendAndSettle(t, c, conn, rtpPacket(111, 100, 0, 1, []byte{0x78, 1}))    // valid
	sendAndSettle(t, c, conn, rtpPacket(111, 103, 960, 1, nil))              // malformed; seq 101,102 lost -> gap 2
	sendAndSettle(t, c, conn, rtpPacket(111, 107, 1920, 1, nil))             // malformed; seq 104,105,106 lost -> gap 3
	sendAndSettle(t, c, conn, rtpPacket(111, 108, 2880, 1, []byte{0x78, 2})) // valid
	waitCount(t, &col, 2, 2*time.Second)

	frames := col.snapshot()
	if len(frames) != 2 {
		t.Fatalf("delivered %d frames, want 2", len(frames))
	}
	if frames[1].SeqGap != 5 {
		t.Errorf("accumulated stranded gap did not sum: SeqGap = %d, want 5", frames[1].SeqGap)
	}
	if ts := c.Stats().Tracks[0]; ts.SeqGaps != 5 {
		t.Errorf("TrackStats.SeqGaps = %d, want 5", ts.SeqGaps)
	}
}

// TestRTPSingleFrameSSRCResetClearsPendingGap confirms a gap left pending by a
// malformed packet on the old source does not bleed onto the new source's first
// frame after an SSRC change.
func TestRTPSingleFrameSSRCResetClearsPendingGap(t *testing.T) {
	var col collector
	c := openOK(t, Config{
		Mode: ModeRTP, PayloadType: 111, Codec: audiostream.CodecOpus{},
		ClockRate: 48000, OnFrame: col.onFrame,
	})
	defer func() { _ = c.Close() }()

	conn := senderFor(t, c)
	sendAndSettle(t, c, conn, rtpPacket(111, 100, 0, 0xAAAA, []byte{0x78, 1})) // valid, source A
	sendAndSettle(t, c, conn, rtpPacket(111, 103, 960, 0xAAAA, nil))           // malformed; seq 101,102 lost -> gap 2 pending on A
	sendAndSettle(t, c, conn, rtpPacket(111, 50, 0, 0xBBBB, []byte{0x78, 2}))  // source B: SSRC reset clears the pending gap
	waitCount(t, &col, 2, 2*time.Second)

	frames := col.snapshot()
	if len(frames) != 2 {
		t.Fatalf("delivered %d frames, want 2", len(frames))
	}
	if frames[1].SeqGap != 0 {
		t.Errorf("first frame from the new source reported SeqGap %d, want 0 (the old source's pending gap must not bleed across an SSRC reset)", frames[1].SeqGap)
	}
	if ts := c.Stats().Tracks[0]; ts.SSRCResets != 1 {
		t.Errorf("SSRCResets = %d, want 1", ts.SSRCResets)
	}
}

// TestRTPNilOnFrameCountsSeqGapsBounded confirms a nil-callback stream still
// counts SeqGaps aggregately and stays alive across a malformed packet; the
// precise no-leak drain under a nil callback is asserted by the direct-call
// TestDeliverRTPSingleFrameNilOnFrameDrains below, since the socket harness has
// no frames to read Frame.SeqGap from.
func TestRTPNilOnFrameCountsSeqGapsBounded(t *testing.T) {
	c := openOK(t, Config{
		Mode: ModeRTP, PayloadType: 111, Codec: audiostream.CodecOpus{}, ClockRate: 48000,
	}) // OnFrame nil
	defer func() { _ = c.Close() }()

	conn := senderFor(t, c)
	sendAndSettle(t, c, conn, rtpPacket(111, 100, 0, 1, []byte{0x78, 1})) // valid
	sendAndSettle(t, c, conn, rtpPacket(111, 102, 960, 1, nil))           // malformed; seq 101 lost -> gap 1
	sendAndSettle(t, c, conn, rtpPacket(111, 103, 1920, 1, []byte{0x78})) // valid

	ts := c.Stats().Tracks[0]
	if ts.SeqGaps != 1 {
		t.Errorf("TrackStats.SeqGaps = %d, want 1 with a nil callback", ts.SeqGaps)
	}
	if ts.Packets < 2 {
		t.Errorf("Packets = %d, want >= 2 (valid packets counted with a nil callback)", ts.Packets)
	}
}

// TestRTPReorderStrandedGapClearedOnSSRCReset exercises the SeqGap accumulator
// on the Config.Reorder path (handleRTPReordered -> drainReleased -> processRTP),
// not just the immediate path. A gap stranded on a malformed packet released from
// the reorder buffer must be cleared by the following SSRC reset, exactly as on
// the immediate path. This mirrors TestReorderSSRCChangeFlushesInOrder but makes
// the old source's second packet malformed so it strands a gap, then asserts the
// gap does not bleed onto the new source's first frame. It guards against a future
// change that reads up.Gap directly on the reordered path, bypassing pendingGap.
func TestRTPReorderStrandedGapClearedOnSSRCReset(t *testing.T) {
	var col collector
	c := openOK(t, Config{
		Mode: ModeRTP, PayloadType: 96, Codec: audiostream.CodecOpus{},
		ClockRate: 48000, Reorder: true, OnFrame: col.onFrame,
	})
	defer func() { _ = c.Close() }()

	conn := senderFor(t, c)
	sendAndSettle(t, c, conn, rtpPacket(96, 10, 1000, 0xAAAA, []byte{0x78, 1}))  // source A: released, frame
	sendAndSettle(t, c, conn, rtpPacket(96, 12, 2920, 0xAAAA, nil))              // source A: empty -> malformed; seq 11 lost -> gap 1 stranded
	sendAndSettle(t, c, conn, rtpPacket(96, 100, 7000, 0xBBBB, []byte{0x78, 2})) // source B: flushes A, SSRC reset clears the pending gap
	waitCount(t, &col, 2, 2*time.Second)

	frames := col.snapshot()
	if len(frames) != 2 {
		t.Fatalf("delivered %d frames, want 2 (the empty-payload packet from source A must not yield a frame)", len(frames))
	}
	if frames[1].SeqGap != 0 {
		t.Errorf("first frame from the new source reported SeqGap %d, want 0 (a gap stranded on the reorder path must not bleed across an SSRC reset)", frames[1].SeqGap)
	}
	if ts := c.Stats().Tracks[0]; ts.SSRCResets != 1 {
		t.Errorf("SSRCResets = %d, want 1", ts.SSRCResets)
	}
}

// --- direct-call unit tests --------------------------------------------------
//
// deliverRTP is unexported and reachable in-package, so these call it directly
// with a synthetic rtp.Update to observe the pendingGap drain/retain invariant
// precisely, including under a nil callback where no frame is emitted to read.

// TestDeliverRTPSingleFrameMalformedRetains confirms a malformed single-frame
// packet retains the pending gap and the next valid frame drains it.
func TestDeliverRTPSingleFrameMalformedRetains(t *testing.T) {
	var got audiostream.Frame
	n := 0
	c := &Client{kind: kindOpus, cfg: Config{ClockRate: 48000, OnFrame: func(f audiostream.Frame) { got = f; n++ }}}
	now := time.Unix(1, 0)

	c.deliverRTP(rtp.Packet{Header: rtp.Header{Timestamp: 0}, Payload: nil}, rtp.Update{Gap: 2}, now) // malformed, retains gap 2
	if n != 0 {
		t.Fatalf("malformed packet delivered %d frames, want 0", n)
	}
	c.deliverRTP(rtp.Packet{Header: rtp.Header{Timestamp: 960}, Payload: []byte{0x78, 1}}, rtp.Update{Gap: 0}, now)
	if n != 1 {
		t.Fatalf("valid packet delivered %d frames, want 1", n)
	}
	if got.SeqGap != 2 {
		t.Errorf("drained SeqGap = %d, want 2", got.SeqGap)
	}
	if c.pendingGap != 0 {
		t.Errorf("pendingGap = %d after drain, want 0", c.pendingGap)
	}
}

// TestDeliverRTPSingleFrameNilOnFrameDrains confirms the drain runs even under a
// nil callback, so a nil-callback stream of valid packets cannot accumulate an
// unbounded pending gap.
func TestDeliverRTPSingleFrameNilOnFrameDrains(t *testing.T) {
	c := &Client{kind: kindOpus, cfg: Config{ClockRate: 48000}} // nil OnFrame
	now := time.Unix(1, 0)

	c.deliverRTP(rtp.Packet{Header: rtp.Header{Timestamp: 0}, Payload: nil}, rtp.Update{Gap: 4}, now) // malformed, retains gap 4
	if c.pendingGap != 4 {
		t.Fatalf("pendingGap = %d after a malformed packet, want 4 retained", c.pendingGap)
	}
	c.deliverRTP(rtp.Packet{Header: rtp.Header{Timestamp: 960}, Payload: []byte{0x78, 1}}, rtp.Update{Gap: 0}, now) // valid, drains under nil callback
	if c.pendingGap != 0 {
		t.Errorf("pendingGap = %d after a valid packet with a nil callback, want 0 (the drain must run even without a callback)", c.pendingGap)
	}
}
