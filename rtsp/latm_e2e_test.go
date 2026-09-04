package rtsp_test

import (
	"bytes"
	"strings"
	"testing"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/internal/testserver"
	"github.com/tphakala/go-audio-stream/rtsp"
)

// eventFrame and eventUpdate are the latmEvent.kind values the LATM delivery
// tests record, so the ordered-event assertions read as words rather than bare
// string literals.
const (
	eventFrame  = "frame"
	eventUpdate = "update"
)

// latmV5Payload is the in-band AudioMuxElement reuse vector (depacket/latm V5)
// paired with latmV4Payload: useSameStreamMux 1, reusing V4's retained
// StreamMuxConfig, PayloadLengthInfo 03, payload AA BB CC. It carries no config
// of its own, so it decodes only while the depacketizer still holds V4's config.
var latmV5Payload = []byte{0x81, 0xD5, 0x5D, 0xE6, 0x00}

// latmV2Payload is the out-of-band AudioMuxElement vector (depacket/latm V2)
// paired with latmOutOfBandSDP's config= (V1): a bare byte-aligned
// AudioMuxElement, PayloadLengthInfo 03, payload AA BB CC. An out-of-band track
// decodes it using the ASC already resolved at Describe.
var latmV2Payload = []byte{0x03, 0xAA, 0xBB, 0xCC}

// latmDeliverClient dials an in-band LATM client whose OnFrame and OnCodecUpdate
// both feed one ordered channel, so a test can assert the interleaving of codec
// updates and frames exactly as Client.process produced them. Each event's data
// field carries the ASC for an update and the access-unit bytes for a frame. The
// events buffer is generous so the reader goroutine never blocks.
func latmDeliverClient(t *testing.T, url string, pref rtsp.TransportPreference) (client *rtsp.Client, events chan latmEvent) {
	t.Helper()
	events = make(chan latmEvent, 16)
	c, err := rtsp.Dial(t.Context(), rtsp.Config{
		URL:       url,
		Timeout:   testTimeout,
		Transport: pref,
		OnFrame: func(f audiostream.Frame) {
			events <- latmEvent{kind: eventFrame, data: bytes.Clone(f.Data), seqGap: f.SeqGap}
		},
		OnCodecUpdate: func(u audiostream.CodecUpdate) {
			latm, _ := u.Codec.(audiostream.CodecMP4ALATM)
			events <- latmEvent{kind: eventUpdate, data: bytes.Clone(latm.AudioSpecificConfig)}
		},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	return c, events
}

// TestIntegrationUDPLATMOnCodecUpdate is the UDP-transport companion to
// TestOnCodecUpdateEndToEnd, which drives the same in-band LATM path over a
// TCP-interleaved connection. Both funnel through Client.process, but only the
// TCP path had a dedicated end-to-end test; this covers the UDP datagram path,
// asserting OnCodecUpdate fires with the resolved ASC before the first frame.
func TestIntegrationUDPLATMOnCodecUpdate(t *testing.T) {
	t.Parallel()

	tracksOut := make(chan []testserver.UDPTrack, 1)
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		if _, err := sc.Handshake(testserver.HandshakeConfig{
			SDP:            latmInBandSDP,
			UDP:            true,
			ServerRTPBase:  nextUDPServerBase(),
			SessionID:      testSessionID,
			SessionTimeout: testTimeoutS,
		}); err != nil {
			t.Errorf("Handshake: %v", err)
			close(tracksOut)
			return
		}
		tracksOut <- sc.UDPTracks()
		drainRequests(sc)
	}})

	c, events := latmDeliverClient(t, s.URL("/stream"), rtsp.PreferUDP)
	defer closeAndWait(t, c)

	describeSetupPlay(t, c, nil)
	if got := c.SessionInfo().Transport; got != wantTransportUDP {
		t.Fatalf("SessionInfo().Transport = %q, want UDP", got)
	}

	track := wantTrack1(t, tracksOut)
	if err := track.InjectRTPSequence([][]byte{
		buildRTPPacket(ptLATM, 1, 1024, 0xA1B2C3D4, true, latmV4Payload),
	}); err != nil {
		t.Fatalf("InjectRTPSequence: %v", err)
	}

	first := recvLATMEvent(t, events)
	if first.kind != eventUpdate {
		t.Fatalf("first event = %q, want update to precede the frame", first.kind)
	}
	if want := []byte{0x12, 0x10}; !bytes.Equal(first.data, want) {
		t.Errorf("OnCodecUpdate ASC = % x, want % x", first.data, want)
	}
	second := recvLATMEvent(t, events)
	if second.kind != eventFrame {
		t.Errorf("second event = %q, want frame", second.kind)
	}
	if want := []byte{0xAA, 0xBB, 0xCC}; !bytes.Equal(second.data, want) {
		t.Errorf("first frame Data = % x, want % x", second.data, want)
	}
}

// TestOnCodecUpdateReannouncesAfterSSRCChange drives the in-band re-announce
// round trip through Client.process: an SSRC change clears the retained
// StreamMuxConfig and its snapshot (resetDepacketizer(true)), so a following
// config-bearing packet must re-fire OnCodecUpdate rather than treat the config
// as already known. The direct-call regression only asserts the snapshot nils;
// this asserts the callback fires again end to end.
func TestOnCodecUpdateReannouncesAfterSSRCChange(t *testing.T) {
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		pairs, err := sc.Handshake(testserver.HandshakeConfig{
			SDP:            latmInBandSDP,
			SessionID:      testSessionID,
			SessionTimeout: testTimeoutS,
		})
		if err != nil {
			return
		}
		// First source resolves the in-band ASC from its inline config.
		_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptLATM, 1, 0, 0xA1B2C3D4, true, latmV4Payload))
		// A new source (different SSRC) re-announces the same config. The SSRC
		// change clears the retained config, so this V4 must re-parse it and
		// re-fire OnCodecUpdate.
		_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptLATM, 2, 1024, 0xBEEFCAFE, true, latmV4Payload))
		drainRequests(sc)
	}})

	c, events := latmDeliverClient(t, s.URL("/stream"), rtsp.PreferTCP)
	defer closeAndWait(t, c)

	describeSetupPlay(t, c, nil)

	// Both sources deliver the same access unit under the same ASC; the codec
	// update precedes the frame each time.
	want := []string{eventUpdate, eventFrame, eventUpdate, eventFrame}
	for i, kind := range want {
		ev := recvLATMEvent(t, events)
		if ev.kind != kind {
			t.Fatalf("event %d = %q, want %q", i, ev.kind, kind)
		}
		if ev.kind == eventUpdate {
			if wantASC := []byte{0x12, 0x10}; !bytes.Equal(ev.data, wantASC) {
				t.Errorf("event %d ASC = % x, want % x", i, ev.data, wantASC)
			}
		}
	}
}

// TestInBandConfigSurvivesGapEndToEnd drives a real RTP sequence gap through
// Client.process and asserts an in-band LATM track keeps decoding. LATM does not
// fragment across packets, so a gap leaves no reassembly state; the gap path
// calls resetDepacketizer(false), which must NOT drop the retained
// StreamMuxConfig. The direct-call regression exercises resetDepacketizer(false)
// on the track; this exercises the whole reader -> process -> deliver path,
// where the gap is detected by rtp.Stream rather than injected.
func TestInBandConfigSurvivesGapEndToEnd(t *testing.T) {
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		pairs, err := sc.Handshake(testserver.HandshakeConfig{
			SDP:            latmInBandSDP,
			SessionID:      testSessionID,
			SessionTimeout: testTimeoutS,
		})
		if err != nil {
			return
		}
		// V4 carries the config inline (useSameStreamMux 0) and resolves it.
		_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptLATM, 1, 0, 0xA1B2C3D4, true, latmV4Payload))
		// Sequence 3 skips 2: a real gap. V5 reuses the retained config
		// (useSameStreamMux 1) and must still decode, proving the config survived
		// the gap-path resetDepacketizer(false).
		_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptLATM, 3, 1024, 0xA1B2C3D4, true, latmV5Payload))
		drainRequests(sc)
	}})

	c, events := latmDeliverClient(t, s.URL("/stream"), rtsp.PreferTCP)
	defer closeAndWait(t, c)

	describeSetupPlay(t, c, nil)

	// update, then the V4 frame, then the V5 frame decoded under the surviving
	// config. If the config had been dropped, V5 would be malformed and the
	// second frame would never arrive (the recv would time out).
	if ev := recvLATMEvent(t, events); ev.kind != eventUpdate {
		t.Fatalf("first event = %q, want update", ev.kind)
	}
	f1 := recvLATMEvent(t, events)
	if f1.kind != eventFrame {
		t.Fatalf("second event = %q, want frame", f1.kind)
	}
	if want := []byte{0xAA, 0xBB, 0xCC}; !bytes.Equal(f1.data, want) {
		t.Errorf("V4 frame Data = % x, want % x", f1.data, want)
	}
	f2 := recvLATMEvent(t, events)
	if f2.kind != eventFrame {
		t.Fatalf("third event = %q, want the V5 frame (config must survive the gap)", f2.kind)
	}
	if want := []byte{0xAA, 0xBB, 0xCC}; !bytes.Equal(f2.data, want) {
		t.Errorf("V5 frame Data = % x, want % x (config must survive the gap)", f2.data, want)
	}
	// Pin that a gap was actually observed end to end: seq 1 -> 3 skips one, so
	// the V5 frame must report SeqGap 1. Without this, contiguous sequence
	// numbers would silently stop exercising the resetDepacketizer(false) path.
	if f2.seqGap != 1 {
		t.Errorf("V5 frame SeqGap = %d, want 1 (the seq 1->3 gap must reach the frame)", f2.seqGap)
	}
}

// assertOutOfBandLATMWarnsOnce drives an out-of-band MP4A-LATM track whose
// StreamMuxConfig latm.New rejects (sdp) through a full Describe/Setup/Play
// cycle with a capturing logger, and asserts the invalid-config warning fires
// exactly once at Describe. The Setup-time "latm fmtp invalid" warning must not
// fire, because Setup adopts Describe's parse result instead of re-parsing;
// pinning that message's absence survives a future reword of the Describe-time
// message that a bare count would not. The track falls back to raw either way.
func assertOutOfBandLATMWarnsOnce(t *testing.T, sdp string) {
	t.Helper()
	logger, snapshot := newCapturingLogger()

	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		if _, err := sc.Handshake(testserver.HandshakeConfig{
			SDP:            sdp,
			SessionID:      testSessionID,
			SessionTimeout: testTimeoutS,
		}); err != nil {
			t.Errorf("Handshake: %v", err)
			return
		}
		drainRequests(sc)
	}})

	c, err := rtsp.Dial(t.Context(), rtsp.Config{
		URL:     s.URL("/stream"),
		Timeout: testTimeout,
		Logger:  logger,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer closeAndWait(t, c)

	describeSetupPlay(t, c, nil)

	got := 0
	for _, m := range snapshot() {
		if strings.Contains(m, "latm") && strings.Contains(m, "invalid") {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("out-of-band LATM invalid-config warnings = %d, want 1; messages: %v", got, snapshot())
	}
	if loggedContains(snapshot(), "latm fmtp invalid") {
		t.Error("Setup-time 'latm fmtp invalid' warning fired; an invalid out-of-band config must be reported once at Describe, not re-logged at Setup")
	}
}

// TestLATMOutOfBandMalformedConfigLogsOnce covers the out-of-band double-parse
// fix for a non-empty but unparseable StreamMuxConfig (the parseStreamMuxConfig
// error branch): it is parsed once at Describe (resolveLATMASC) and adopted at
// Setup rather than re-parsed, so its warning fires once across the whole cycle
// rather than once at Describe and again at Setup.
func TestLATMOutOfBandMalformedConfigLogsOnce(t *testing.T) {
	assertOutOfBandLATMWarnsOnce(t, latmOutOfBandMalformedSDP)
}

// TestOutOfBandLATMSuccessEndToEnd drives a VALID out-of-band MP4A-LATM track
// through the real Describe -> resolveTracks -> Setup -> newTrack ->
// configureLATM wiring and confirms the depacketizer adopted from Describe
// decodes a frame, and that OnCodecUpdate does NOT fire (the config is already
// known at Describe). TestConfigureLATMReusesDescribeDepacketizer hand-builds
// the describedTrack and so bypasses the resolveTracks threading; a regression
// dropping latmDepack on the success path would silently degrade every
// out-of-band track to raw, which only this end-to-end test would catch.
func TestOutOfBandLATMSuccessEndToEnd(t *testing.T) {
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		pairs, err := sc.Handshake(testserver.HandshakeConfig{
			SDP:            latmOutOfBandSDP,
			SessionID:      testSessionID,
			SessionTimeout: testTimeoutS,
		})
		if err != nil {
			return
		}
		_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptLATM, 1, 0, 0xA1B2C3D4, true, latmV2Payload))
		drainRequests(sc)
	}})

	c, events := latmDeliverClient(t, s.URL("/stream"), rtsp.PreferTCP)
	defer closeAndWait(t, c)

	describeSetupPlay(t, c, nil)

	// Out-of-band: the ASC was resolved at Describe, so OnCodecUpdate must not
	// fire and the first (and only) event is the decoded frame. An in-band track
	// would emit an update first, so asserting the first event is the frame
	// proves the adopted depacketizer already held the config.
	ev := recvLATMEvent(t, events)
	if ev.kind != eventFrame {
		t.Fatalf("first event = %q, want frame (out-of-band must not fire OnCodecUpdate)", ev.kind)
	}
	if want := []byte{0xAA, 0xBB, 0xCC}; !bytes.Equal(ev.data, want) {
		t.Errorf("frame Data = % x, want % x", ev.data, want)
	}
}

// TestLATMOutOfBandEmptyConfigLogsOnce covers the dropped len(StreamMuxConfig)==0
// early-out in resolveLATMASC: an out-of-band track whose config= is absent has
// an empty StreamMuxConfig, which latm.New rejects via its empty-config guard, a
// different branch than the malformed-parse case. Describe must still succeed,
// the track falls back to raw, and the invalid-config warning fires exactly once
// at Describe rather than being deferred to Setup.
func TestLATMOutOfBandEmptyConfigLogsOnce(t *testing.T) {
	assertOutOfBandLATMWarnsOnce(t, latmOutOfBandNoConfigSDP)
}
