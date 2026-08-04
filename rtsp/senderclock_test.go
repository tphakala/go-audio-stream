package rtsp_test

import (
	"sync"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/internal/testserver"
)

// ntpUnixEpochOffset is the seconds from the NTP epoch (1900) to the Unix
// epoch (1970), used to build Sender Reports whose decoded NTP time lands on a
// known Unix instant.
const ntpUnixEpochOffset = 2208988800

// opusClockRate is the rate opusSDP declares (opus/48000/2); every populated
// SenderClock for that track carries it.
const opusClockRate = 48000

// ntpFor builds the 64-bit NTP timestamp (zero fraction) whose decode is the
// given Unix second, so a test can assert an exact NTPTime.
func ntpFor(unixSec uint64) uint64 {
	return (unixSec + ntpUnixEpochOffset) << 32
}

// A Sender Report on the RTCP channel publishes the RTP-to-NTP correspondence
// on TrackStats.SenderClock: the RTP timestamp, the decoded wall clock, the
// track clock rate, and a receive time no later than the snapshot. WallClock
// then extrapolates a later RTP timestamp to the matching wall clock.
func TestSenderClockSurfacesFromSenderReport(t *testing.T) {
	const mediaSSRC = 0x0BADF00D
	const srRTP = 3_000_000
	const unixSec = 1_000_000_000
	wantNTP := time.Unix(unixSec, 0)

	c, _ := playAndInject(t, &testserver.HandshakeConfig{SDP: opusSDP}, nil,
		func(sc *testserver.ServerConn, pairs []testserver.ChannelPair) {
			// The RTP packet settles the track's media SSRC first, so the Sender
			// Report that follows is recognized as describing this track.
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptOpus, 7, 960, mediaSSRC, false, []byte{0x78, 0x01}))
			_ = sc.InjectFrame(pairs[0].RTCP, buildSenderReport(mediaSSRC, ntpFor(unixSec), srRTP))
		})
	defer closeAndWait(t, c)

	waitForStats(t, c, 0, func(s audiostream.TrackStats) bool { return s.SenderClock.Valid })

	// Re-read a whole snapshot so CapturedAt and the mapping come from the same
	// read, then assert the report age is non-negative.
	full := c.Stats()
	got := full.Tracks[0].SenderClock
	if got.RTPTime != srRTP {
		t.Errorf("SenderClock.RTPTime = %d, want %d", got.RTPTime, srRTP)
	}
	if !got.NTPTime.Equal(wantNTP) {
		t.Errorf("SenderClock.NTPTime = %v, want %v", got.NTPTime.UTC(), wantNTP.UTC())
	}
	if got.ClockRate != opusClockRate {
		t.Errorf("SenderClock.ClockRate = %d, want the track rate %d", got.ClockRate, opusClockRate)
	}
	if got.ReceivedAt.After(full.CapturedAt) {
		t.Errorf("SenderClock.ReceivedAt %v is after CapturedAt %v", got.ReceivedAt, full.CapturedAt)
	}
	// One clock rate of ticks past the report is exactly one second later.
	if want := wantNTP.Add(time.Second); !got.WallClock(srRTP + opusClockRate).Equal(want) {
		t.Errorf("WallClock(srRTP+rate) = %v, want %v", got.WallClock(srRTP+opusClockRate).UTC(), want.UTC())
	}
}

// Before any Sender Report the mapping is the zero SenderClock: absent, not a
// stale or bogus pair.
func TestSenderClockAbsentBeforeSenderReport(t *testing.T) {
	const mediaSSRC = 0x0BADF00D

	c, _ := playAndInject(t, &testserver.HandshakeConfig{SDP: opusSDP}, nil,
		func(sc *testserver.ServerConn, pairs []testserver.ChannelPair) {
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptOpus, 7, 960, mediaSSRC, false, []byte{0x78, 0x01}))
		})
	defer closeAndWait(t, c)

	ts := waitForStats(t, c, 0, func(s audiostream.TrackStats) bool { return s.Packets >= 1 })
	if ts.SenderClock != (audiostream.SenderClock{}) {
		t.Errorf("SenderClock = %+v, want the zero value before any Sender Report", ts.SenderClock)
	}
}

// A sender with no wall clock sends an all-zero NTP timestamp (RFC 3550
// section 6.4.1); it must never publish a mapping, so the era-1 pivot cannot
// decode it into a bogus 2036 "valid" time.
func TestSenderClockIgnoresAllZeroNTP(t *testing.T) {
	const mediaSSRC = 0x0BADF00D

	c, _ := playAndInject(t, &testserver.HandshakeConfig{SDP: opusSDP}, nil,
		func(sc *testserver.ServerConn, pairs []testserver.ChannelPair) {
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptOpus, 7, 960, mediaSSRC, false, []byte{0x78, 0x01}))
			_ = sc.InjectFrame(pairs[0].RTCP, buildSenderReport(mediaSSRC, 0, 3_000_000))
			// A second RTP packet is a sync point: once Stats counts it, the
			// zero-NTP report ahead of it has been processed.
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptOpus, 8, 1920, mediaSSRC, false, []byte{0x78, 0x02}))
		})
	defer closeAndWait(t, c)

	ts := waitForStats(t, c, 0, func(s audiostream.TrackStats) bool { return s.Packets >= 2 })
	if ts.SenderClock.Valid {
		t.Errorf("SenderClock.Valid = true, want false for an all-zero NTP Sender Report")
	}
}

// An SSRC change clears the mapping: the previous source's pair must not
// convert the new source's timestamps. The single predicate proves the reset
// was counted and the mapping cleared in the same snapshot.
func TestSenderClockClearedOnSSRCReset(t *testing.T) {
	const firstSSRC = 0x0BADF00D
	const secondSSRC = 0x11112222
	const unixSec = 1_000_000_000

	c, _ := playAndInject(t, &testserver.HandshakeConfig{SDP: opusSDP}, nil,
		func(sc *testserver.ServerConn, pairs []testserver.ChannelPair) {
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptOpus, 7, 960, firstSSRC, false, []byte{0x78, 0x01}))
			_ = sc.InjectFrame(pairs[0].RTCP, buildSenderReport(firstSSRC, ntpFor(unixSec), 3_000_000))
			// A new source resets the stream; the mapping must be dropped.
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptOpus, 20, 5000, secondSSRC, false, []byte{0x78, 0x02}))
		})
	defer closeAndWait(t, c)

	waitForStats(t, c, 0, func(s audiostream.TrackStats) bool {
		return s.SSRCResets == 1 && !s.SenderClock.Valid
	})
}

// A mixer or translator emits a Sender Report per contributing source. When the
// report describing this track's media is not first in the compound, the
// mapping must still come from the matching one, never from a foreign source.
func TestSenderClockFromMixerPicksMatchingSource(t *testing.T) {
	const mediaSSRC = 0x0BADF00D
	const otherSSRC = 0xDEADBEEF
	const mediaRTP = 3_000_000
	const mediaUnixSec = 1_000_000_000
	wantNTP := time.Unix(mediaUnixSec, 0)

	c, _ := playAndInject(t, &testserver.HandshakeConfig{SDP: opusSDP}, nil,
		func(sc *testserver.ServerConn, pairs []testserver.ChannelPair) {
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptOpus, 7, 960, mediaSSRC, false, []byte{0x78, 0x01}))
			// The foreign source's report comes first, with a different pair, so
			// taking the first report rather than the matching one is detectable.
			compound := append(buildSenderReport(otherSSRC, ntpFor(2_000_000_000), 111),
				buildSenderReport(mediaSSRC, ntpFor(mediaUnixSec), mediaRTP)...)
			_ = sc.InjectFrame(pairs[0].RTCP, compound)
		})
	defer closeAndWait(t, c)

	ts := waitForStats(t, c, 0, func(s audiostream.TrackStats) bool { return s.SenderClock.Valid })
	if ts.SenderClock.RTPTime != mediaRTP {
		t.Errorf("SenderClock.RTPTime = %d, want the matching source's %d", ts.SenderClock.RTPTime, mediaRTP)
	}
	if !ts.SenderClock.NTPTime.Equal(wantNTP) {
		t.Errorf("SenderClock.NTPTime = %v, want the matching source's %v", ts.SenderClock.NTPTime.UTC(), wantNTP.UTC())
	}
}

// A compound whose only Sender Report names a foreign SSRC leaves the mapping
// invalid: storing it would map this track's timestamps against a source the
// server is not sending.
func TestSenderClockForeignSourceStaysInvalid(t *testing.T) {
	const mediaSSRC = 0x0BADF00D
	const otherSSRC = 0xDEADBEEF

	c, _ := playAndInject(t, &testserver.HandshakeConfig{SDP: opusSDP}, nil,
		func(sc *testserver.ServerConn, pairs []testserver.ChannelPair) {
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptOpus, 7, 960, mediaSSRC, false, []byte{0x78, 0x01}))
			_ = sc.InjectFrame(pairs[0].RTCP, buildSenderReport(otherSSRC, ntpFor(1_000_000_000), 111))
			// Sync point: once Stats counts the second RTP packet, the foreign
			// report ahead of it has been processed.
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptOpus, 8, 1920, mediaSSRC, false, []byte{0x78, 0x02}))
		})
	defer closeAndWait(t, c)

	ts := waitForStats(t, c, 0, func(s audiostream.TrackStats) bool { return s.Packets >= 2 })
	if ts.SenderClock.Valid {
		t.Errorf("SenderClock.Valid = true, want false when no Sender Report names the media source")
	}
}

// A Sender Report that arrives before any RTP packet must not publish a
// mapping: with no media SSRC learned yet, handleRTCP adopts the first report
// for the Receiver Report path, but that source is unconfirmed and (for a mixer
// or translator) may be one the server is not sending us. The mapping must stay
// absent until an RTP packet identifies the media source and a matching report
// arrives.
func TestSenderClockIgnoresSenderReportBeforeRTP(t *testing.T) {
	const mediaSSRC = 0x0BADF00D
	const foreignSSRC = 0xDEADBEEF
	const mediaRTP = 3_000_000
	const mediaUnixSec = 1_000_000_000
	wantNTP := time.Unix(mediaUnixSec, 0)

	// injectSR holds back the media source's Sender Report until the test has
	// confirmed the pre-RTP foreign report published nothing. The deferred
	// release is a safety net: it runs ahead of the testserver stop cleanup that
	// waits on the handler goroutine, so a failed assertion cannot leave the
	// handler parked on the channel and hang the package.
	injectSR := make(chan struct{})
	var once sync.Once
	releaseSR := func() { once.Do(func() { close(injectSR) }) }
	defer releaseSR()

	c, _ := playAndInject(t, &testserver.HandshakeConfig{SDP: opusSDP}, nil,
		func(sc *testserver.ServerConn, pairs []testserver.ChannelPair) {
			// A Sender Report for a foreign source, before any RTP.
			_ = sc.InjectFrame(pairs[0].RTCP, buildSenderReport(foreignSSRC, ntpFor(2_000_000_000), 111))
			// The first RTP packet identifies the real media source.
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptOpus, 7, 960, mediaSSRC, false, []byte{0x78, 0x01}))
			<-injectSR
			// A Sender Report for the identified media source now publishes it.
			_ = sc.InjectFrame(pairs[0].RTCP, buildSenderReport(mediaSSRC, ntpFor(mediaUnixSec), mediaRTP))
		})
	defer closeAndWait(t, c)

	// Once the RTP packet is counted, the foreign Sender Report ahead of it in
	// the stream has been processed, so the mapping must still be absent.
	ts := waitForStats(t, c, 0, func(s audiostream.TrackStats) bool { return s.Packets >= 1 })
	if ts.SenderClock.Valid {
		t.Fatalf("SenderClock.Valid = true after a pre-RTP Sender Report, want false")
	}

	// A Sender Report for the confirmed media source publishes the mapping.
	releaseSR()
	got := waitForStats(t, c, 0, func(s audiostream.TrackStats) bool { return s.SenderClock.Valid })
	if got.SenderClock.RTPTime != mediaRTP {
		t.Errorf("SenderClock.RTPTime = %d, want the media source's %d", got.SenderClock.RTPTime, mediaRTP)
	}
	if !got.SenderClock.NTPTime.Equal(wantNTP) {
		t.Errorf("SenderClock.NTPTime = %v, want the media source's %v", got.SenderClock.NTPTime.UTC(), wantNTP.UTC())
	}
}

// A sender that first advertises a wall clock and later reports it has none
// (an all-zero NTP timestamp, RFC 3550 section 6.4.1) must have its prior
// mapping cleared, not left visible: WallClock would otherwise keep
// extrapolating a stale pair.
func TestSenderClockClearedByAllZeroNTP(t *testing.T) {
	const mediaSSRC = 0x0BADF00D
	const validRTP = 3_000_000
	const validUnixSec = 1_000_000_000

	// injectZero holds back the all-zero report until the valid mapping is
	// observed. The deferred release runs ahead of the testserver stop cleanup
	// that waits on the handler, so a failed assertion cannot park it.
	injectZero := make(chan struct{})
	var once sync.Once
	releaseZero := func() { once.Do(func() { close(injectZero) }) }
	defer releaseZero()

	c, _ := playAndInject(t, &testserver.HandshakeConfig{SDP: opusSDP}, nil,
		func(sc *testserver.ServerConn, pairs []testserver.ChannelPair) {
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptOpus, 7, 960, mediaSSRC, false, []byte{0x78, 0x01}))
			_ = sc.InjectFrame(pairs[0].RTCP, buildSenderReport(mediaSSRC, ntpFor(validUnixSec), validRTP))
			<-injectZero
			// The same source now reports it has no wall clock.
			_ = sc.InjectFrame(pairs[0].RTCP, buildSenderReport(mediaSSRC, 0, validRTP))
			// A second RTP packet is a sync point: once Stats counts it, the
			// all-zero report ahead of it has been processed.
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptOpus, 8, 1920, mediaSSRC, false, []byte{0x78, 0x02}))
		})
	defer closeAndWait(t, c)

	// A valid mapping is established first; nothing clears it until the gate.
	waitForStats(t, c, 0, func(s audiostream.TrackStats) bool { return s.SenderClock.Valid })

	// The all-zero NTP report must clear it rather than leave the stale pair.
	releaseZero()
	waitForStats(t, c, 0, func(s audiostream.TrackStats) bool {
		return s.Packets >= 2 && !s.SenderClock.Valid
	})
}
