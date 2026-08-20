package udpsource

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
)

// buildSR builds a minimal RTCP Sender Report datagram (version 2, no padding,
// zero report blocks): a 4-byte header, the sender SSRC, and the 20-byte sender
// info block, for a total of 28 bytes. There is no SenderReport.Marshal in the
// rtp package, so tests hand-build the wire form.
func buildSR(ssrc uint32, ntp uint64, rtpTS, pktCount, octetCount uint32) []byte {
	b := make([]byte, 28)
	b[0] = 0x80                           // version 2, padding 0, report count 0
	b[1] = 200                            // PT = Sender Report
	binary.BigEndian.PutUint16(b[2:4], 6) // length in 32-bit words minus 1 (7 words)
	binary.BigEndian.PutUint32(b[4:8], ssrc)
	binary.BigEndian.PutUint64(b[8:16], ntp)
	binary.BigEndian.PutUint32(b[16:20], rtpTS)
	binary.BigEndian.PutUint32(b[20:24], pktCount)
	binary.BigEndian.PutUint32(b[24:28], octetCount)
	return b
}

// ntpEpochOffset is the seconds from the NTP epoch (1900) to the Unix epoch
// (1970), the same constant rtp.NTPTime decodes against.
const ntpEpochOffset = 2208988800

// ntpAt encodes a whole-second Unix time as a 64-bit NTP timestamp with a zero
// fraction, so rtp.NTPTime round-trips it back to time.Unix(sec, 0).
func ntpAt(unixSec int64) uint64 {
	return uint64(unixSec+ntpEpochOffset) << 32
}

// senderForAddr dials an arbitrary UDP address so a test can push datagrams to a
// socket other than the media socket (the separate RTCP socket).
func senderForAddr(t *testing.T, addr string) *net.UDPConn {
	t.Helper()
	uaddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatalf("resolve %q: %v", addr, err)
	}
	conn, err := net.DialUDP("udp", nil, uaddr)
	if err != nil {
		t.Fatalf("dial %q: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// waitSenderClock retransmits the Sender Report (via send) and polls Stats until
// the sender-clock mapping is Valid or the deadline passes. Retransmitting makes
// the assertion robust against the media/RTCP goroutine ordering on the
// separate-socket path (the first accepted RTP packet must set baseSet before an
// SR is honored) and mirrors a real sender emitting reports periodically.
func waitSenderClock(t *testing.T, c *Client, send func(), within time.Duration) audiostream.SenderClock {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		send()
		if sc := c.Stats().Tracks[0].SenderClock; sc.Valid {
			return sc
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("SenderClock did not become valid within %v", within)
	return audiostream.SenderClock{}
}

// --- unit tests: handleRTCP mapping (white-box, no sockets) -----------------

// newRTCPClient builds a bare Client wired only enough for handleRTCP: a clock
// rate, an identified media source, and the base-set publish gate.
func newRTCPClient(clockRate int, mediaSSRC uint32, baseSet bool) *Client {
	c := &Client{cfg: Config{Mode: ModeRTP, ClockRate: clockRate}}
	c.mediaSSRC.Store(mediaSSRC)
	c.baseSet.Store(baseSet)
	return c
}

func TestHandleRTCPValidSRPublishesSenderClock(t *testing.T) {
	const ssrc, clockRate = uint32(0xAABBCCDD), 90000
	const srRTP = uint32(12345)
	c := newRTCPClient(clockRate, ssrc, true)
	now := time.Now()
	c.handleRTCP(buildSR(ssrc, ntpAt(1_000_000_000), srRTP, 10, 2000), now)

	sc := c.srClock.Load()
	if sc == nil || !sc.Valid {
		t.Fatalf("SenderClock not published: %+v", sc)
	}
	if sc.RTPTime != srRTP {
		t.Errorf("RTPTime = %d, want %d", sc.RTPTime, srRTP)
	}
	if want := time.Unix(1_000_000_000, 0); !sc.NTPTime.Equal(want) {
		t.Errorf("NTPTime = %v, want %v", sc.NTPTime.UTC(), want.UTC())
	}
	if sc.ClockRate != clockRate {
		t.Errorf("ClockRate = %d, want %d", sc.ClockRate, clockRate)
	}
	// WallClock extrapolation: a frame one second of ticks ahead maps one second
	// past the report's NTP instant.
	if got, want := sc.WallClock(srRTP+uint32(clockRate)), time.Unix(1_000_000_001, 0); !got.Equal(want) {
		t.Errorf("WallClock(+1s) = %v, want %v", got.UTC(), want.UTC())
	}
}

func TestHandleRTCPZeroNTPClearsMapping(t *testing.T) {
	const ssrc = uint32(0x11223344)
	c := newRTCPClient(90000, ssrc, true)
	c.srClock.Store(&audiostream.SenderClock{RTPTime: 1, Valid: true})
	// RFC 3550 6.4.1: an all-zero NTP timestamp maps nothing and clears any prior
	// correspondence.
	c.handleRTCP(buildSR(ssrc, 0, 999, 1, 1), time.Now())
	if sc := c.srClock.Load(); sc != nil {
		t.Fatalf("mapping not cleared on zero NTP: %+v", sc)
	}
}

func TestHandleRTCPSelectsReportByMediaSSRC(t *testing.T) {
	const media, foreign = uint32(0x0A0A0A0A), uint32(0x0B0B0B0B)
	const mediaRTP = uint32(7777)
	c := newRTCPClient(90000, media, true)
	// A compound leading with a foreign contributing source's report, then the
	// media source's. The media source's report must win, not reports[0].
	compound := append(buildSR(foreign, ntpAt(500_000_000), 1, 1, 1), buildSR(media, ntpAt(1_000_000_000), mediaRTP, 2, 2)...)
	c.handleRTCP(compound, time.Now())
	sc := c.srClock.Load()
	if sc == nil || sc.RTPTime != mediaRTP {
		t.Fatalf("selected wrong report: %+v (want RTPTime %d)", sc, mediaRTP)
	}
}

func TestHandleRTCPBeforeBaseSetIgnored(t *testing.T) {
	const ssrc = uint32(0xDEADBEEF)
	c := newRTCPClient(90000, 0, false) // baseSet false: source not identified yet
	c.handleRTCP(buildSR(ssrc, ntpAt(1_000_000_000), 5, 1, 1), time.Now())
	if sc := c.srClock.Load(); sc != nil {
		t.Fatalf("published before base set: %+v", sc)
	}
}

func TestHandleRTCPNoMatchingSSRCIgnored(t *testing.T) {
	c := newRTCPClient(90000, 0x01010101, true)
	before := c.srClock.Load()
	c.handleRTCP(buildSR(0x02020202, ntpAt(1_000_000_000), 5, 1, 1), time.Now())
	if after := c.srClock.Load(); after != before {
		t.Fatalf("published a foreign-source mapping: %+v", after)
	}
}

func TestHandleRTCPMalformedIgnored(t *testing.T) {
	c := newRTCPClient(90000, 0x01010101, true)
	// Neither garbage nor a too-short buffer must panic or publish.
	c.handleRTCP([]byte{0x00}, time.Now())
	c.handleRTCP([]byte("not an rtcp packet at all"), time.Now())
	if sc := c.srClock.Load(); sc != nil {
		t.Fatalf("published from malformed RTCP: %+v", sc)
	}
}

// --- config validation ------------------------------------------------------

func TestOpenRTCPValidation(t *testing.T) {
	base := func() Config {
		return Config{Mode: ModeRTP, PayloadType: 96, Codec: audiostream.CodecUnknown{RTPMap: rtpMap90k}, ClockRate: 90000, ListenAddr: loopbackAddr}
	}
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"mux and listen addr exclusive", func(c *Config) { c.RTCPMux = true; c.RTCPListenAddr = loopbackAddr }},
		{"mux reserved payload type", func(c *Config) { c.RTCPMux = true; c.PayloadType = 72 }},
		{"bad listen addr", func(c *Config) { c.RTCPListenAddr = "not-an-address" }},
		{"rtcp under modepcm", func(c *Config) {
			*c = Config{Mode: ModePCM, RTCPMux: true, ListenAddr: loopbackAddr, Format: PCMFormat{SampleRate: 48000, Channels: 1}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mutate(&cfg)
			c, err := Open(context.Background(), cfg)
			if err == nil {
				_ = c.Close()
				t.Fatalf("Open accepted invalid RTCP config")
			}
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("err = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

// --- end-to-end: both transports --------------------------------------------

// rtcpE2ECfg is an opaque ModeRTP config (payload type 96) with an OnFrame sink.
func rtcpE2ECfg(onFrame func(audiostream.Frame)) Config {
	return Config{Mode: ModeRTP, PayloadType: 96, Codec: audiostream.CodecUnknown{RTPMap: rtpMap90k}, ClockRate: 90000, OnFrame: onFrame}
}

func TestRTCPMuxPopulatesSenderClock(t *testing.T) {
	const ssrc = uint32(0x0C0FFEE0)
	var col collector
	cfg := rtcpE2ECfg(col.onFrame)
	cfg.RTCPMux = true
	c := openOK(t, cfg)
	defer func() { _ = c.Close() }()
	conn := senderFor(t, c)

	// One RTP packet identifies the media source (sets baseSet + mediaSSRC).
	sendAndSettle(t, c, conn, rtpPacket(96, 1, 48000, ssrc, []byte("audio")))
	waitCount(t, &col, 1, 2*time.Second)

	// The SR muxes onto the same socket; poll until it maps.
	sc := waitSenderClock(t, c, func() {
		_, _ = conn.Write(buildSR(ssrc, ntpAt(1_600_000_000), 48000, 1, 5))
	}, 2*time.Second)
	if sc.RTPTime != 48000 || sc.ClockRate != 90000 {
		t.Fatalf("SenderClock = %+v", sc)
	}
	if want := time.Unix(1_600_000_000, 0); !sc.NTPTime.Equal(want) {
		t.Errorf("NTPTime = %v, want %v", sc.NTPTime.UTC(), want.UTC())
	}
}

func TestRTCPSeparateSocketPopulatesSenderClock(t *testing.T) {
	const ssrc = uint32(0x0ABCDEF0)
	var col collector
	cfg := rtcpE2ECfg(col.onFrame)
	cfg.RTCPListenAddr = loopbackAddr
	c := openOK(t, cfg)
	defer func() { _ = c.Close() }()

	media := senderFor(t, c)
	rtcp := senderForAddr(t, c.rtcpConn.LocalAddr().String())

	sendAndSettle(t, c, media, rtpPacket(96, 1, 90000, ssrc, []byte("audio")))
	waitCount(t, &col, 1, 2*time.Second)

	sc := waitSenderClock(t, c, func() {
		_, _ = rtcp.Write(buildSR(ssrc, ntpAt(1_600_000_000), 90000, 1, 5))
	}, 2*time.Second)
	// A frame half a second of ticks ahead maps half a second past the report.
	if got, want := sc.WallClock(90000+45000), time.Unix(1_600_000_000, int64(time.Second)/2); !got.Equal(want) {
		t.Errorf("WallClock(+0.5s) = %v, want %v", got.UTC(), want.UTC())
	}
}

func TestRTCPSSRCResetClearsSenderClock(t *testing.T) {
	const ssrc1, ssrc2 = uint32(0x11111111), uint32(0x22222222)
	var col collector
	cfg := rtcpE2ECfg(col.onFrame)
	cfg.RTCPListenAddr = loopbackAddr
	c := openOK(t, cfg)
	defer func() { _ = c.Close() }()

	media := senderFor(t, c)
	rtcp := senderForAddr(t, c.rtcpConn.LocalAddr().String())

	sendAndSettle(t, c, media, rtpPacket(96, 1, 90000, ssrc1, []byte("a")))
	waitCount(t, &col, 1, 2*time.Second)
	waitSenderClock(t, c, func() { _, _ = rtcp.Write(buildSR(ssrc1, ntpAt(1_600_000_000), 90000, 1, 1)) }, 2*time.Second)

	// A new source resets the timeline and must clear the old mapping.
	sendAndSettle(t, c, media, rtpPacket(96, 2, 180000, ssrc2, []byte("b")))
	deadline := time.Now().Add(2 * time.Second)
	for c.Stats().Tracks[0].SenderClock.Valid && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if c.Stats().Tracks[0].SenderClock.Valid {
		t.Fatalf("SenderClock still valid after SSRC reset")
	}

	// The new source's report re-establishes the mapping with its own values.
	sc := waitSenderClock(t, c, func() { _, _ = rtcp.Write(buildSR(ssrc2, ntpAt(1_700_000_000), 180000, 1, 1)) }, 2*time.Second)
	if sc.RTPTime != 180000 {
		t.Errorf("RTPTime = %d, want 180000", sc.RTPTime)
	}
}

func TestRTCPMalformedNeverEndsSession(t *testing.T) {
	const ssrc = uint32(0x33333333)
	var col collector
	cfg := rtcpE2ECfg(col.onFrame)
	cfg.RTCPListenAddr = loopbackAddr
	c := openOK(t, cfg)
	defer func() { _ = c.Close() }()

	media := senderFor(t, c)
	rtcp := senderForAddr(t, c.rtcpConn.LocalAddr().String())

	sendAndSettle(t, c, media, rtpPacket(96, 1, 90000, ssrc, []byte("a")))
	waitCount(t, &col, 1, 2*time.Second)

	// Garbage on the RTCP socket must not end the session.
	for range 5 {
		_, _ = rtcp.Write([]byte("garbage rtcp payload"))
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait(context.Background()) }()
	select {
	case err := <-done:
		t.Fatalf("session ended on malformed RTCP: %v", err)
	case <-time.After(150 * time.Millisecond):
	}

	// A subsequent valid SR still maps: the goroutine survived the garbage.
	sc := waitSenderClock(t, c, func() { _, _ = rtcp.Write(buildSR(ssrc, ntpAt(1_600_000_000), 90000, 1, 1)) }, 2*time.Second)
	if !sc.Valid {
		t.Fatalf("valid SR after garbage did not map")
	}
}

func TestRTCPSeparateSocketCloseNoLeak(t *testing.T) {
	var col collector
	cfg := rtcpE2ECfg(col.onFrame)
	cfg.RTCPListenAddr = loopbackAddr
	c := openOK(t, cfg)

	media := senderFor(t, c)
	sendAndSettle(t, c, media, rtpPacket(96, 1, 90000, 0x44444444, []byte("a")))

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Wait must join both the media reader and the RTCP goroutine and return the
	// Close cause; the -race build proves no goroutine outlives it.
	if err := waitResult(t, c, 2*time.Second); !errors.Is(err, audiostream.ErrClosed) {
		t.Fatalf("Wait = %v, want ErrClosed", err)
	}
}
