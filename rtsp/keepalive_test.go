package rtsp_test

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/internal/testserver"
	"github.com/tphakala/go-audio-stream/rtsp"
	"github.com/tphakala/go-audio-stream/rtsp/rtp"
)

// rrCadence mirrors the client's fixed 5s RTCP Receiver Report interval; the
// package constant rtcpReportInterval is unexported, so the timing assertions
// use this local copy with a generous margin.
const rrCadence = 5 * time.Second

// observeKeepaliveMethod drives a full handshake advertising public, then
// returns the method token of the first keepalive the client sends after PLAY.
func observeKeepaliveMethod(t *testing.T, public []string) string {
	t.Helper()
	got := make(chan string, 1)
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		if _, err := sc.Handshake(testserver.HandshakeConfig{
			SDP:            opusSDP,
			SessionID:      testSessionID,
			SessionTimeout: 2, // keepalive interval = sessionTimeout/2 = 1s
			PublicMethods:  public,
		}); err != nil {
			return
		}
		req, err := sc.ReadRequest()
		if err != nil {
			return
		}
		got <- req.Method
		_ = sc.Respond(req, 200, "OK", nil, nil)
		drainRequests(sc)
	}})
	c := dialIdle(t, s.URL("/stream"))
	defer closeAndWait(t, c)
	describeSetupPlay(t, c, nil)
	select {
	case m := <-got:
		return m
	case <-time.After(4 * time.Second):
		t.Fatal("no keepalive within 4s")
		return ""
	}
}

func TestKeepaliveGetParameter(t *testing.T) {
	m := observeKeepaliveMethod(t, []string{methodOptions, methodDescribe, methodSetup, methodPlay, methodGetParameter})
	if m != methodGetParameter {
		t.Errorf("keepalive method = %q, want GET_PARAMETER", m)
	}
}

func TestKeepaliveOptionsFallback(t *testing.T) {
	m := observeKeepaliveMethod(t, []string{methodOptions, methodDescribe, methodSetup, methodPlay})
	if m != methodOptions {
		t.Errorf("keepalive method = %q, want OPTIONS", m)
	}
}

func TestKeepaliveFireAndForget(t *testing.T) {
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		if _, err := sc.Handshake(testserver.HandshakeConfig{
			SDP:            opusSDP,
			SessionID:      testSessionID,
			SessionTimeout: 2, // keepalive interval = 1s
		}); err != nil {
			return
		}
		// Read the keepalives but never reply: liveness is judged by frames,
		// never by a keepalive reply, so the session must stay up.
		for {
			if _, err := sc.ReadRequest(); err != nil {
				return
			}
		}
	}})
	// ReadIdle is disabled, so only a real fault ends the session.
	c := dialIdle(t, s.URL("/stream"))
	describeSetupPlay(t, c, nil)

	// Wait out several keepalive intervals; the session must still be alive.
	// Wait with a cancelling context returns DeadlineExceeded only if the
	// session had not already terminated on its own; a false timeout from the
	// unanswered keepalives would surface a different error here. The cancel
	// also funnels shutdown, so Close afterward is a no-op cleanup.
	waitCtx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	if err := c.Wait(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Wait = %v, want still-alive; unanswered keepalives must not end the session", err)
	}
	// Join the reader rather than returning while it is still running its
	// TEARDOWN/close/wg.Wait sequence. closeAndWait is not usable here: the
	// cancel above already funneled shutdown with DeadlineExceeded, and the
	// first cause wins, so Wait reports that rather than the ErrClosed the
	// shared helper asserts.
	if err := c.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
	joinCtx, joinCancel := context.WithTimeout(context.Background(), testTimeout)
	defer joinCancel()
	if err := c.Wait(joinCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Wait after Close = %v, want the original DeadlineExceeded", err)
	}
}

// parseReceiverReport extracts the reporter SSRC and reception report blocks
// from a marshaled RTCP Receiver Report so a test can assert on them without a
// public unmarshal helper.
func parseReceiverReport(payload []byte) (rtp.ReceiverReport, error) {
	if len(payload) < 8 {
		return rtp.ReceiverReport{}, fmt.Errorf("receiver report too short: %d bytes", len(payload))
	}
	if payload[1] != 201 {
		return rtp.ReceiverReport{}, fmt.Errorf("payload type = %d, want 201 (RR)", payload[1])
	}
	rr := rtp.ReceiverReport{ReporterSSRC: binary.BigEndian.Uint32(payload[4:8])}
	count := int(payload[0] & 0x1f)
	off := 8
	for i := 0; i < count; i++ {
		if off+24 > len(payload) {
			return rtp.ReceiverReport{}, errors.New("receiver report block truncated")
		}
		rr.Blocks = append(rr.Blocks, rtp.ReportBlock{
			SSRC:             binary.BigEndian.Uint32(payload[off : off+4]),
			HighestSequence:  binary.BigEndian.Uint32(payload[off+8 : off+12]),
			LastSR:           binary.BigEndian.Uint32(payload[off+16 : off+20]),
			DelaySinceLastSR: binary.BigEndian.Uint32(payload[off+20 : off+24]),
		})
		off += 24
	}
	return rr, nil
}

// readReceiverReport reads client wire units until an interleaved Receiver
// Report arrives on the given RTCP channel.
func readReceiverReport(sc *testserver.ServerConn, rtcpChannel int) (rtp.ReceiverReport, error) {
	for {
		_, frame, err := sc.ReadAny()
		if err != nil {
			return rtp.ReceiverReport{}, err
		}
		if frame != nil && frame.Channel == rtcpChannel {
			return parseReceiverReport(frame.Payload)
		}
	}
}

func TestRTCPReceiverReportsEmitted(t *testing.T) {
	type result struct {
		rr  rtp.ReceiverReport
		err error
	}
	res := make(chan result, 1)
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		pairs, err := sc.Handshake(testserver.HandshakeConfig{
			SDP:            opusSDP,
			SessionID:      testSessionID,
			SessionTimeout: testTimeoutS,
		})
		if err != nil {
			return
		}
		// Prime the reader's RR snapshot with two RTP packets so the emitted
		// report carries a plausible highest sequence.
		_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptOpus, 100, 960, 0x0BADF00D, false, []byte{0x78, 0x01}))
		_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptOpus, 101, 1920, 0x0BADF00D, false, []byte{0x78, 0x02}))
		rr, rerr := readReceiverReport(sc, pairs[0].RTCP)
		res <- result{rr: rr, err: rerr}
		drainRequests(sc)
	}})
	c := dialIdle(t, s.URL("/stream"))
	defer closeAndWait(t, c)
	describeSetupPlay(t, c, nil)

	select {
	case r := <-res:
		if r.err != nil {
			t.Fatalf("reading receiver report: %v", r.err)
		}
		if len(r.rr.Blocks) != 1 {
			t.Fatalf("RR blocks = %d, want 1", len(r.rr.Blocks))
		}
		if r.rr.ReporterSSRC == 0 {
			t.Errorf("ReporterSSRC = 0, want a non-zero client SSRC")
		}
		if r.rr.Blocks[0].HighestSequence != 101 {
			t.Errorf("HighestSequence = %d, want 101", r.rr.Blocks[0].HighestSequence)
		}
		// The block must name the source it reports on. This was 0 for every
		// session in which the camera never changed SSRC and sent no Sender
		// Report, because the SSRC was only captured on a change.
		if got := r.rr.Blocks[0].SSRC; got != 0x0BADF00D {
			t.Errorf("Blocks[0].SSRC = %#x, want the sender SSRC 0x0BADF00D", got)
		}
		// No Sender Report was injected, so there is no delay to report.
		if got := r.rr.Blocks[0].LastSR; got != 0 {
			t.Errorf("LastSR = %#x, want 0 with no Sender Report received", got)
		}
		if got := r.rr.Blocks[0].DelaySinceLastSR; got != 0 {
			t.Errorf("DelaySinceLastSR = %d, want 0 with no Sender Report received", got)
		}
	case <-time.After(2 * rrCadence):
		t.Fatal("no receiver report emitted within two RR intervals")
	}
}

func TestReadIdleWatchdog(t *testing.T) {
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		if _, err := sc.Handshake(testserver.HandshakeConfig{
			SDP:            opusSDP,
			SessionID:      testSessionID,
			SessionTimeout: testTimeoutS,
		}); err != nil {
			return
		}
		// Inject nothing and keep the connection open so the read-idle
		// watchdog, not an EOF, ends the session.
		for {
			if _, err := sc.ReadRequest(); err != nil {
				return
			}
		}
	}})
	// Capture the baseline with the server's accept loop already running, so
	// only the client's and one handler's goroutines are counted.
	baseline := runtime.NumGoroutine()
	c, err := rtsp.Dial(context.Background(), rtsp.Config{
		URL:      s.URL("/stream"),
		Timeout:  testTimeout,
		ReadIdle: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	describeSetupPlay(t, c, nil)

	if err := c.Wait(context.Background()); !errors.Is(err, audiostream.ErrReadTimeout) {
		t.Fatalf("Wait = %v, want ErrReadTimeout", err)
	}
	// Close after the watchdog already ended the session: the first cause wins,
	// so Wait keeps reporting ErrReadTimeout rather than ErrClosed.
	if err := c.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
	if err := c.Wait(context.Background()); !errors.Is(err, audiostream.ErrReadTimeout) {
		t.Errorf("Wait after Close = %v, want the original ErrReadTimeout", err)
	}
	assertNoGoroutineLeak(t, baseline)
}

func TestWatchdogNotTrippedByKeepaliveReply(t *testing.T) {
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		if _, err := sc.Handshake(testserver.HandshakeConfig{
			SDP:            opusSDP,
			SessionID:      testSessionID,
			SessionTimeout: 2, // keepalive interval = 1s
		}); err != nil {
			return
		}
		// Reply to every keepalive but never send audio. The replies are bytes
		// on the wire, yet the watchdog measures frames, so it still trips.
		for {
			req, err := sc.ReadRequest()
			if err != nil {
				return
			}
			if err := sc.Respond(req, 200, "OK", nil, nil); err != nil {
				return
			}
		}
	}})
	c, err := rtsp.Dial(context.Background(), rtsp.Config{
		URL:      s.URL("/stream"),
		Timeout:  testTimeout,
		ReadIdle: 1500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	describeSetupPlay(t, c, nil)

	if err := c.Wait(context.Background()); !errors.Is(err, audiostream.ErrReadTimeout) {
		t.Fatalf("Wait = %v, want ErrReadTimeout despite keepalive replies", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
}

// buildSenderReport assembles a minimal RTCP Sender Report (RFC 3550 section
// 6.4.1): version 2, no report blocks, followed by the 20-byte sender info.
func buildSenderReport(ssrc uint32, ntp uint64, rtpTime uint32) []byte {
	buf := []byte{0x80, 200, 0x00, 0x06} // V=2, RC=0, PT=200 (SR), length 6 words
	buf = binary.BigEndian.AppendUint32(buf, ssrc)
	buf = binary.BigEndian.AppendUint64(buf, ntp)
	buf = binary.BigEndian.AppendUint32(buf, rtpTime)
	buf = binary.BigEndian.AppendUint32(buf, 0) // packet count
	buf = binary.BigEndian.AppendUint32(buf, 0) // octet count
	return buf
}

// A Sender Report arriving on the track's RTCP channel is folded into the next
// Receiver Report: its source, the middle 32 bits of its NTP timestamp as LSR,
// and a non-zero delay since it was received. Without this the whole RTCP
// ingress half of the feature was unexecuted by any test.
func TestSenderReportFeedsTheNextReceiverReport(t *testing.T) {
	const srSSRC = 0xFEEDFACE
	// Middle 32 bits of this NTP timestamp: seconds 0x11223344, fraction
	// 0x55667788, so LSR is the low 16 of the seconds and the high 16 of the
	// fraction.
	const ntp = uint64(0x11223344)<<32 | 0x55667788
	const wantLSR = uint32(0x33445566)

	type result struct {
		rr  rtp.ReceiverReport
		err error
	}
	res := make(chan result, 1)
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		pairs, err := sc.Handshake(testserver.HandshakeConfig{
			SDP:            opusSDP,
			SessionID:      testSessionID,
			SessionTimeout: testTimeoutS,
		})
		if err != nil {
			return
		}
		// An RTP packet first, so the track has observed a media SSRC, then the
		// Sender Report for that same source on the RTCP channel.
		_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptOpus, 7, 960, srSSRC, false, []byte{0x78, 0x01}))
		_ = sc.InjectFrame(pairs[0].RTCP, buildSenderReport(srSSRC, ntp, 960))
		rr, rerr := readReceiverReport(sc, pairs[0].RTCP)
		res <- result{rr: rr, err: rerr}
		drainRequests(sc)
	}})
	c := dialIdle(t, s.URL("/stream"))
	defer closeAndWait(t, c)
	describeSetupPlay(t, c, nil)

	select {
	case r := <-res:
		if r.err != nil {
			t.Fatalf("reading receiver report: %v", r.err)
		}
		if len(r.rr.Blocks) != 1 {
			t.Fatalf("RR blocks = %d, want 1", len(r.rr.Blocks))
		}
		if got := r.rr.Blocks[0].SSRC; got != srSSRC {
			t.Errorf("Blocks[0].SSRC = %#x, want %#x", got, uint32(srSSRC))
		}
		if got := r.rr.Blocks[0].LastSR; got != wantLSR {
			t.Errorf("LastSR = %#x, want the middle 32 NTP bits %#x", got, wantLSR)
		}
		// The report is emitted at least one RTCP interval after the SR was
		// received, so the delay must be a real, non-zero value.
		if got := r.rr.Blocks[0].DelaySinceLastSR; got == 0 {
			t.Error("DelaySinceLastSR = 0, want the elapsed delay since the Sender Report")
		}
	case <-time.After(2 * rrCadence):
		t.Fatal("no receiver report emitted within two RR intervals")
	}
}
