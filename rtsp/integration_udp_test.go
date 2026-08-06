package rtsp_test

import (
	"bytes"
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/internal/testserver"
	"github.com/tphakala/go-audio-stream/rtsp"
	"github.com/tphakala/go-audio-stream/rtsp/rtp"
)

// udpBasePortCounter hands out a fresh base port to each UDP integration
// test's testserver, so sequential tests never contend for the same OS UDP
// port even if a previous test's socket close is still draining on a loaded
// runner. testserver's own acceptUDPSetup retries on a collision too, so
// this is belt and braces rather than the only guard against flakiness.
var udpBasePortCounter atomic.Int32

// nextUDPServerBase returns a fresh base port for a UDP testserver's server
// socket pair (RTP at the base, RTCP at base+1).
func nextUDPServerBase() int {
	return 41000 + int(udpBasePortCounter.Add(1))*10
}

// aacFrameSamples is the arbitrary per-frame sample-count spacing this
// file's synthesized AAC datagrams use for the RTP timestamp, so PTS
// advances by a fixed, predictable amount (aacFrameSamples/16000 s, matching
// aacSDP's clock rate) from one synthesized frame to the next.
const aacFrameSamples = 1024

// aacAU returns a small, distinct 4-byte access unit for index i, so a test
// can assert a decoded frame's content matches its own place in the
// sequence rather than merely counting frames.
func aacAU(i int) []byte {
	return []byte{byte(i >> 8), byte(i), 0xAA, 0xBB}
}

// aacUDPDatagram builds one AAC-hbr RTP datagram carrying aacAU(i) at the
// given sequence number. The RTP timestamp advances by aacFrameSamples*i,
// keyed to i rather than to the packet actually sent, so the resulting PTS
// reflects the frame's place in the sequence even across a deliberately
// reordered or lossy delivery.
func aacUDPDatagram(seq uint16, i int, ssrc uint32) []byte {
	ts := uint32(i) * aacFrameSamples //nolint:gosec // i is a small test-local loop index, never near overflow.
	return buildRTPPacket(ptAAC, seq, ts, ssrc, true, aacHbrPayload(aacAU(i)))
}

// playAndInjectUDP scripts a UDP handshake for hcfg, drives the real client
// into the playing state under pref, and returns the negotiated UDPTracks on
// tracksCh once the handshake completes.
//
// Unlike playAndInject's TCP-interleaved counterpart, injection needs no
// release gate here: UDP datagrams travel on their own sockets, entirely
// independent of the control connection drainRequests keeps alive, so a test
// may inject from its own goroutine at any point after receiving tracksCh,
// including concurrently with drainRequests.
func playAndInjectUDP(t *testing.T, hcfg *testserver.HandshakeConfig, pref rtsp.TransportPreference,
) (client *rtsp.Client, frameCh <-chan audiostream.Frame, tracksCh <-chan []testserver.UDPTrack) {
	t.Helper()
	if hcfg.SessionID == "" {
		hcfg.SessionID = testSessionID
	}
	if hcfg.SessionTimeout == 0 {
		hcfg.SessionTimeout = testTimeoutS
	}
	frames := make(chan audiostream.Frame, 256)
	tracksOut := make(chan []testserver.UDPTrack, 1)
	cfg := *hcfg
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		if _, err := sc.Handshake(cfg); err != nil {
			t.Errorf("Handshake: %v", err)
			close(tracksOut)
			return
		}
		tracksOut <- sc.UDPTracks()
		drainRequests(sc)
	}})

	c := dialTransport(t, s.URL("/stream"), pref, frames)
	describeSetupPlay(t, c, nil)
	return c, frames, tracksOut
}

// wantTrack1 fetches the single expected UDPTrack from tracksCh, failing the
// test if the handshake did not negotiate exactly one.
func wantTrack1(t *testing.T, tracksCh <-chan []testserver.UDPTrack) testserver.UDPTrack {
	t.Helper()
	tracks := <-tracksCh
	if len(tracks) != 1 {
		t.Fatalf("UDPTracks: got %d, want 1", len(tracks))
	}
	return tracks[0]
}

// TestIntegrationUDPHappyPathAAC covers the primary UDP path end to end: a
// real rtsp.Client negotiates RTP/AVP unicast UDP, and a sequence of AAC-hbr
// RTP datagrams injected in order arrives as decoded access units in order,
// with increasing PTS and no reported loss.
func TestIntegrationUDPHappyPathAAC(t *testing.T) {
	const n = 5
	ssrc := uint32(0x11220001)
	datagrams := make([][]byte, n)
	for i := range datagrams {
		datagrams[i] = aacUDPDatagram(uint16(1000+i), i, ssrc) //nolint:gosec // i < n is tiny; no overflow.
	}

	c, frames, tracksCh := playAndInjectUDP(t,
		&testserver.HandshakeConfig{SDP: aacSDP, UDP: true, ServerRTPBase: nextUDPServerBase()},
		rtsp.PreferUDP)
	defer closeAndWait(t, c)

	if got := c.SessionInfo().Transport; got != "UDP" {
		t.Fatalf("SessionInfo().Transport = %q, want UDP", got)
	}

	track := wantTrack1(t, tracksCh)
	if err := track.InjectRTPSequence(datagrams); err != nil {
		t.Fatalf("InjectRTPSequence: %v", err)
	}

	var lastPTS time.Duration
	for i := 0; i < n; i++ {
		f := recvFrame(t, frames)
		if want := aacAU(i); !bytes.Equal(f.Data, want) {
			t.Errorf("frame %d Data = % x, want % x", i, f.Data, want)
		}
		if f.SeqGap != 0 {
			t.Errorf("frame %d SeqGap = %d, want 0", i, f.SeqGap)
		}
		switch {
		case i == 0 && f.PTS != 0:
			t.Errorf("frame 0 PTS = %v, want 0 (baseline)", f.PTS)
		case i > 0 && f.PTS <= lastPTS:
			t.Errorf("frame %d PTS = %v, want > previous frame's %v", i, f.PTS, lastPTS)
		}
		lastPTS = f.PTS
	}
}

// TestIntegrationUDPReordering injects the happy-path sequence with two
// packets swapped on the wire and asserts the client's Reorderer resequences
// them: frames still arrive in ascending sequence order, matching the
// content each access unit was built with, and nothing is counted as
// malformed or lost.
func TestIntegrationUDPReordering(t *testing.T) {
	const n = 6
	ssrc := uint32(0x11220002)
	datagrams := make([][]byte, n)
	for i := range datagrams {
		datagrams[i] = aacUDPDatagram(uint16(2000+i), i, ssrc) //nolint:gosec // i < n is tiny; no overflow.
	}
	// Scramble the wire order; the packets themselves still carry ascending
	// sequence numbers, so a correct client resequences them regardless of
	// arrival order.
	sent := append([][]byte(nil), datagrams...)
	sent[2], sent[3] = sent[3], sent[2]

	c, frames, tracksCh := playAndInjectUDP(t,
		&testserver.HandshakeConfig{SDP: aacSDP, UDP: true, ServerRTPBase: nextUDPServerBase()},
		rtsp.PreferUDP)
	defer closeAndWait(t, c)

	track := wantTrack1(t, tracksCh)
	if err := track.InjectRTPSequence(sent); err != nil {
		t.Fatalf("InjectRTPSequence: %v", err)
	}

	for i := 0; i < n; i++ {
		f := recvFrame(t, frames)
		if want := aacAU(i); !bytes.Equal(f.Data, want) {
			t.Errorf("frame %d Data = % x, want % x (sequence order, not send order)", i, f.Data, want)
		}
		if f.SeqGap != 0 {
			t.Errorf("frame %d SeqGap = %d, want 0 (reordered, not lost)", i, f.SeqGap)
		}
	}
	st := waitForStats(t, c, 0, func(ts audiostream.TrackStats) bool { return ts.Packets >= n })
	if st.Malformed != 0 {
		t.Errorf("Malformed = %d, want 0", st.Malformed)
	}
}

// lossTrailingPackets is how many consecutive packets must follow the
// omitted one to force the client's reorder window (rtp.MaxReorderWindow)
// open. The Reorderer only declares the loss once a later packet's sequence
// number is at least MaxReorderWindow ahead of the missing one, so a genuine
// end-to-end test of that path has to send at least that many packets after
// the gap, not fewer.
const lossTrailingPackets = rtp.MaxReorderWindow

// TestIntegrationUDPLoss omits one packet from the middle of a sequence and
// sends enough trailing packets to force the reorder window to release the
// held-back tail, asserting the first frame released after the gap reports
// SeqGap == 1 and every later one reports no further gap.
func TestIntegrationUDPLoss(t *testing.T) {
	ssrc := uint32(0x11220003)
	total := 1 + lossTrailingPackets
	// i = 0 is sent, i = 1 is the deliberately omitted packet, i = 2..total
	// follow consecutively so the last one forces the window open.
	datagrams := make([][]byte, 0, total)
	for i := 0; i <= total; i++ {
		if i == 1 {
			continue
		}
		datagrams = append(datagrams, aacUDPDatagram(uint16(3000+i), i, ssrc)) //nolint:gosec // i <= total is tiny; no overflow.
	}

	c, frames, tracksCh := playAndInjectUDP(t,
		&testserver.HandshakeConfig{SDP: aacSDP, UDP: true, ServerRTPBase: nextUDPServerBase()},
		rtsp.PreferUDP)
	defer closeAndWait(t, c)

	track := wantTrack1(t, tracksCh)
	if err := track.InjectRTPSequence(datagrams); err != nil {
		t.Fatalf("InjectRTPSequence: %v", err)
	}

	// Frame for i=0 releases immediately: it establishes the release point
	// and carries no gap of its own.
	f0 := recvFrame(t, frames)
	if !bytes.Equal(f0.Data, aacAU(0)) {
		t.Fatalf("frame 0 Data = % x, want % x", f0.Data, aacAU(0))
	}
	if f0.SeqGap != 0 {
		t.Errorf("frame 0 SeqGap = %d, want 0", f0.SeqGap)
	}

	// Frames for i=2..total release together, in one burst, once the window
	// forces it. The first of them (i=2) must report the single lost packet
	// (i=1); every later one must report no further gap.
	for idx, i := 0, 2; i <= total; idx, i = idx+1, i+1 {
		f := recvFrame(t, frames)
		if want := aacAU(i); !bytes.Equal(f.Data, want) {
			t.Fatalf("frame for i=%d Data = % x, want % x", i, f.Data, want)
		}
		wantGap := 0
		if idx == 0 {
			wantGap = 1
		}
		if f.SeqGap != wantGap {
			t.Errorf("frame for i=%d SeqGap = %d, want %d", i, f.SeqGap, wantGap)
		}
	}
}

// TestIntegrationUDPRTCPReceiverReportEmitted asserts the server receives at
// least one client RTCP Receiver Report over the UDP RTCP socket within a
// generous window. The client hole-punches with a real Receiver Report
// synchronously inside Setup (well before Play), so this also proves the
// hole-punch/keepalive RR path end to end.
func TestIntegrationUDPRTCPReceiverReportEmitted(t *testing.T) {
	c, _, tracksCh := playAndInjectUDP(t,
		&testserver.HandshakeConfig{SDP: aacSDP, UDP: true, ServerRTPBase: nextUDPServerBase()},
		rtsp.PreferUDP)
	defer closeAndWait(t, c)

	track := wantTrack1(t, tracksCh)
	// The window is generous only to absorb scheduler jitter under -race on a
	// loaded runner; the client hole-punches synchronously inside Setup, well
	// before Play even starts, so the report should already be waiting.
	datagram, ok := track.WaitClientRTCP(5 * time.Second)
	if !ok {
		t.Fatal("no client RTCP datagram received within 5s")
	}
	rr, err := parseReceiverReport(datagram)
	if err != nil {
		t.Fatalf("parseReceiverReport: %v", err)
	}
	if rr.ReporterSSRC == 0 {
		t.Errorf("ReporterSSRC = 0, want a non-zero client SSRC")
	}
}

// TestIntegrationUDPFallbackToTCP covers RejectUDP under PreferUDPThenTCP:
// the server declines the UDP SETUP, the session falls back to and pins TCP
// interleaved transport, and interleaved frames flow exactly as in the phase
// 1 path. No UDPTrack is ever negotiated.
func TestIntegrationUDPFallbackToTCP(t *testing.T) {
	au := []byte{0x01, 0x02, 0x03, 0x04}
	frames := make(chan audiostream.Frame, 8)
	udpTracksCh := make(chan int, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(closeRelease)

	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		pairs, err := sc.Handshake(testserver.HandshakeConfig{
			SDP:           aacSDP,
			SessionID:     testSessionID,
			UDP:           true,
			RejectUDP:     true,
			ServerRTPBase: nextUDPServerBase(),
		})
		if err != nil {
			close(udpTracksCh)
			return
		}
		udpTracksCh <- len(sc.UDPTracks())
		<-release
		_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptAAC, 1000, 5000, 0x99887766, true, aacHbrPayload(au)))
		drainRequests(sc)
	}})

	c := dialTransport(t, s.URL("/stream"), rtsp.PreferUDPThenTCP, frames)
	describeSetupPlay(t, c, nil)
	udpTracksSeen, ok := <-udpTracksCh
	closeRelease()
	defer closeAndWait(t, c)
	if !ok {
		t.Fatal("Handshake failed; see prior error")
	}
	if udpTracksSeen != 0 {
		t.Errorf("UDPTracks after a rejected UDP SETUP: got %d, want 0", udpTracksSeen)
	}

	if got := c.SessionInfo().Transport; got != wantTransportTCP {
		t.Fatalf("SessionInfo().Transport = %q, want %q", got, wantTransportTCP)
	}
	f := recvFrame(t, frames)
	if !bytes.Equal(f.Data, au) {
		t.Errorf("Data = % x, want % x", f.Data, au)
	}
}

// TestIntegrationUDPCleanShutdownNoLeak asserts that after Close, the phase 1
// goroutine-leak helper reports no residual UDP receive goroutines: the RTP
// and RTCP receivers started by Play join cleanly on teardown, exactly like
// the TCP reader does.
//
// It cannot reuse playAndInjectUDP's bundled Describe+Setup+Play: unlike a
// TCP testserver.Handle, this test double's UDP scripting starts its own
// background goroutines (the udpEndpoint readers acceptUDPSetup launches) as
// a side effect of SETUP, and those live for the whole test by design (a
// test may inject at any point up to teardown), never joining before
// t.Cleanup. The baseline the leak check must settle back to therefore has
// to be captured after Setup (once those exist) but before Play, which is
// the point the CLIENT's own UDP receive goroutines start; capturing it any
// earlier would count this test double's goroutines as a false leak.
func TestIntegrationUDPCleanShutdownNoLeak(t *testing.T) {
	frames := make(chan audiostream.Frame, 8)
	tracksOut := make(chan []testserver.UDPTrack, 1)
	cfg := testserver.HandshakeConfig{
		SDP:            aacSDP,
		SessionID:      testSessionID,
		SessionTimeout: testTimeoutS,
		UDP:            true,
		ServerRTPBase:  nextUDPServerBase(),
	}
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		if _, err := sc.Handshake(cfg); err != nil {
			t.Errorf("Handshake: %v", err)
			close(tracksOut)
			return
		}
		tracksOut <- sc.UDPTracks()
		drainRequests(sc)
	}})

	c := dialTransport(t, s.URL("/stream"), rtsp.PreferUDP, frames)
	tracks, err := c.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if len(tracks) != 1 {
		t.Fatalf("Describe: got %d tracks, want 1", len(tracks))
	}
	if err := c.Setup(context.Background(), tracks[0], rtsp.SetupOptions{}); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	baseline := runtime.NumGoroutine()

	if err := c.Play(context.Background()); err != nil {
		t.Fatalf("Play: %v", err)
	}

	track := wantTrack1(t, tracksOut)
	if err := track.InjectRTP(aacUDPDatagram(1, 0, 0x11220099)); err != nil {
		t.Fatalf("InjectRTP: %v", err)
	}
	_ = recvFrame(t, frames)

	closeAndWait(t, c)
	assertNoGoroutineLeak(t, baseline)
}

// TestIntegrationUDPControlWatchdogSurvivesIdleControl is the regression test
// for the live-interop bug where the phase-1 control-connection read-idle
// watchdog killed a healthy UDP session at exactly ReadIdle. In UDP mode media
// arrives on the per-track UDP sockets, not the control connection, so the
// control read must NOT be given a ReadIdle deadline; the RTP-socket watchdog in
// runRTPReceiver handles media liveness instead. With the bug present the
// session died at ReadIdle regardless of UDP media; here a small ReadIdle is set
// and RTP datagrams are injected one at a time with a real gap between them, so
// the stream spans several ReadIdle windows while the control connection stays
// quiet (testTimeoutS is 90s, so no keepalive fires in this sub-second window).
// Receiving every frame proves the session stayed alive across those windows,
// and the backgrounded Wait must not return ErrReadTimeout.
func TestIntegrationUDPControlWatchdogSurvivesIdleControl(t *testing.T) {
	const readIdle = 150 * time.Millisecond
	const n = 12                      // 12 datagrams
	const gap = 40 * time.Millisecond // ~440ms of injection, several ReadIdle windows
	ssrc := uint32(0x11220003)

	frames := make(chan audiostream.Frame, 256)
	tracksOut := make(chan []testserver.UDPTrack, 1)
	cfg := testserver.HandshakeConfig{
		SDP:            aacSDP,
		SessionID:      testSessionID,
		SessionTimeout: testTimeoutS,
		UDP:            true,
		ServerRTPBase:  nextUDPServerBase(),
	}
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		if _, err := sc.Handshake(cfg); err != nil {
			t.Errorf("Handshake: %v", err)
			close(tracksOut)
			return
		}
		tracksOut <- sc.UDPTracks()
		drainRequests(sc)
	}})

	// Dial directly so ReadIdle can be set (dialTransport does not expose it).
	c, err := rtsp.Dial(context.Background(), rtsp.Config{
		URL:       s.URL("/stream"),
		Timeout:   testTimeout,
		Transport: rtsp.PreferUDP,
		ReadIdle:  readIdle,
		OnFrame: func(f audiostream.Frame) {
			cp := f
			cp.Data = append([]byte(nil), f.Data...)
			frames <- cp
		},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	describeSetupPlay(t, c, nil)
	if got := c.SessionInfo().Transport; got != "UDP" {
		t.Fatalf("SessionInfo().Transport = %q, want UDP", got)
	}
	track := wantTrack1(t, tracksOut)

	// Background Wait so a spurious watchdog ErrReadTimeout surfaces the instant
	// it funnels rather than being missed.
	waitErr := make(chan error, 1)
	go func() { waitErr <- c.Wait(context.Background()) }()

	// Inject datagrams one at a time with a real gap between them, so the stream
	// spans several ReadIdle windows while the control connection stays quiet.
	// Receiving every frame is the proof: with the control watchdog still active
	// the session would have been torn down around the first ReadIdle window and
	// the later frames would never arrive.
	for i := 0; i < n; i++ {
		if err := track.InjectRTP(aacUDPDatagram(uint16(2000+i), i, ssrc)); err != nil { //nolint:gosec // i < n is tiny.
			t.Fatalf("InjectRTP %d: %v", i, err)
		}
		if got := recvFrame(t, frames); !bytes.Equal(got.Data, aacAU(i)) {
			t.Errorf("frame %d Data = % x, want % x", i, got.Data, aacAU(i))
		}
		select {
		case err := <-waitErr:
			t.Fatalf("session ended after %d frames: %v (ErrReadTimeout means the control watchdog still fired in UDP mode)", i+1, err)
		default:
		}
		if i < n-1 {
			time.Sleep(gap)
		}
	}

	// Every frame arrived across roughly n*gap of wall-clock time, several
	// ReadIdle windows, so the control-connection watchdog is off in UDP mode.
	// The session is still alive right after the last frame; assert that before
	// tearing down. (Once injection stops the RTP-socket media watchdog will
	// legitimately end the session after ReadIdle; that is the separate
	// media-liveness path, not the control-watchdog bug under test, so this
	// check must run before that window elapses.)
	select {
	case err := <-waitErr:
		t.Fatalf("session ended just after the stream: %v, want it still alive", err)
	default:
	}

	// Tear down explicitly. The terminal cause may be ErrClosed or, if the media
	// watchdog wins the race now that injection has stopped, ErrReadTimeout;
	// both are correct here, so assert only that Wait returns.
	_ = c.Close()
	select {
	case <-waitErr:
	case <-time.After(testTimeout):
		t.Fatal("Wait did not return after Close")
	}
}
