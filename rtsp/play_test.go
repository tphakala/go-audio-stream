package rtsp_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/depacket/g711"
	"github.com/tphakala/go-audio-stream/internal/testserver"
	"github.com/tphakala/go-audio-stream/rtsp"
)

// SDP fixtures for the non-AAC play paths. The AAC fixtures live in
// describe_test.go and are shared across the external test package.
const (
	opusSDP = "v=0\r\n" +
		"o=- 0 0 IN IP4 127.0.0.1\r\n" +
		"s=Stream\r\n" +
		"m=audio 0 RTP/AVP 96\r\n" +
		"a=rtpmap:96 opus/48000/2\r\n" +
		"a=control:audio\r\n"

	g711MuLawSDP = "v=0\r\n" +
		"o=- 0 0 IN IP4 127.0.0.1\r\n" +
		"s=Stream\r\n" +
		"m=audio 0 RTP/AVP 0\r\n" +
		"a=rtpmap:0 PCMU/8000\r\n" +
		"a=control:audio\r\n"

	g711ALawSDP = "v=0\r\n" +
		"o=- 0 0 IN IP4 127.0.0.1\r\n" +
		"s=Stream\r\n" +
		"m=audio 0 RTP/AVP 8\r\n" +
		"a=rtpmap:8 PCMA/8000\r\n" +
		"a=control:audio\r\n"
)

// Payload types the test SDPs above declare. The reader rejects a packet whose
// payload type is not the one its track was resolved from, so every injected
// packet must carry the PT its own m= line names.
const (
	ptAAC  uint8 = 97 // aacSDP and audioVideoSDP's audio section
	ptOpus uint8 = 96 // opusSDP
	ptPCMU uint8 = 0  // g711MuLawSDP
	ptPCMA uint8 = 8  // g711ALawSDP
	ptH264 uint8 = 96 // audioVideoSDP's video section
)

// buildRTPPacket assembles a minimal RTP packet (version 2, no CSRC, no
// extension, no padding) with the given fields and payload.
func buildRTPPacket(pt uint8, seq uint16, ts, ssrc uint32, marker bool, payload []byte) []byte {
	h := make([]byte, 12, 12+len(payload))
	h[0] = 0x80 // version 2
	h[1] = pt
	if marker {
		h[1] |= 0x80
	}
	binary.BigEndian.PutUint16(h[2:], seq)
	binary.BigEndian.PutUint32(h[4:], ts)
	binary.BigEndian.PutUint32(h[8:], ssrc)
	return append(h, payload...)
}

// aacHbrPayload builds a complete AAC-hbr RTP payload carrying the given
// access units (sizelength=13, indexlength=3, indexdeltalength=3).
func aacHbrPayload(aus ...[]byte) []byte {
	buf := binary.BigEndian.AppendUint16(nil, uint16(len(aus)*16))
	for _, au := range aus {
		buf = binary.BigEndian.AppendUint16(buf, uint16(len(au))<<3)
	}
	for _, au := range aus {
		buf = append(buf, au...)
	}
	return buf
}

// aacFragment builds an AAC-hbr payload with a single AU-header declaring
// declaredSize and carrying data (which may be shorter than declaredSize for
// a fragment start, or the trailing bytes of a fragment continuation).
func aacFragment(declaredSize int, data []byte) []byte {
	buf := binary.BigEndian.AppendUint16(nil, 16) // one 16-bit AU-header
	buf = binary.BigEndian.AppendUint16(buf, uint16(declaredSize)<<3)
	return append(buf, data...)
}

// dialWithFrames dials with an OnFrame callback that deep-copies each frame
// (honoring the Data-valid-only-during-callback contract) onto frames.
func dialWithFrames(t *testing.T, url string, frames chan<- audiostream.Frame) *rtsp.Client {
	t.Helper()
	c, err := rtsp.Dial(context.Background(), rtsp.Config{
		URL:     url,
		Timeout: testTimeout,
		OnFrame: func(f audiostream.Frame) {
			cp := f
			cp.Data = append([]byte(nil), f.Data...)
			frames <- cp
		},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	return c
}

// describeSetupPlay drives the client through Describe, one Setup per
// discovered track (Discard chosen by discard, which may be nil), and Play.
func describeSetupPlay(t *testing.T, c *rtsp.Client, discard func(i int) bool) []rtsp.Track {
	t.Helper()
	tracks, err := c.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	for i, tr := range tracks {
		opts := rtsp.SetupOptions{}
		if discard != nil {
			opts.Discard = discard(i)
		}
		if err := c.Setup(context.Background(), tr, opts); err != nil {
			t.Fatalf("Setup track %d: %v", i, err)
		}
	}
	if err := c.Play(context.Background()); err != nil {
		t.Fatalf("Play: %v", err)
	}
	return tracks
}

// playAndInject scripts a standard handshake for hcfg, drives the client into
// the playing state, and only then (after Play has returned) runs inject on
// the server goroutine to push frames. Deferring injection until Play returns
// makes RTP-Info seeding deterministic: the seed is applied before any frame
// is processed. Delivered frames are copied onto the returned channel.
func playAndInject(t *testing.T, hcfg *testserver.HandshakeConfig, discard func(i int) bool,
	inject func(sc *testserver.ServerConn, pairs []testserver.ChannelPair),
) (client *rtsp.Client, frameCh <-chan audiostream.Frame) {
	t.Helper()
	if hcfg.SessionID == "" {
		hcfg.SessionID = testSessionID
	}
	if hcfg.SessionTimeout == 0 {
		hcfg.SessionTimeout = testTimeoutS
	}
	frames := make(chan audiostream.Frame, 128)
	release := make(chan struct{})
	cfg := *hcfg
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		pairs, err := sc.Handshake(cfg)
		if err != nil {
			return
		}
		<-release
		inject(sc, pairs)
		drainRequests(sc)
	}})
	// Released on cleanup as well as on the happy path. describeSetupPlay calls
	// t.Fatalf on any handshake failure, which runtime.Goexit's past the release
	// below; the handler would then be parked on a channel rather than on the
	// socket, where the server's own stop() cannot wake it, and the whole
	// package would hang in t.Cleanup until the test binary timed out.
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(closeRelease)

	c := dialWithFrames(t, s.URL("/stream"), frames)
	describeSetupPlay(t, c, discard)
	closeRelease()
	return c, frames
}

// waitForStats polls Stats until want is satisfied, or fails on timeout. The
// reader increments its counters AFTER handing the frame to OnFrame, so a test
// that received a frame has not necessarily observed the counters for it yet:
// asserting them directly is a measured flake (5% under -race), not a
// theoretical one.
func waitForStats(t *testing.T, c *rtsp.Client, trackID int, want func(audiostream.TrackStats) bool) audiostream.TrackStats {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for {
		st := c.Stats().Tracks[trackID]
		if want(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for track %d stats; last seen %+v", trackID, st)
			return st
		}
		time.Sleep(time.Millisecond)
	}
}

// recvFrame reads one frame or fails on timeout.
func recvFrame(t *testing.T, ch <-chan audiostream.Frame) audiostream.Frame {
	t.Helper()
	select {
	case f := <-ch:
		return f
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for a frame")
		return audiostream.Frame{}
	}
}

func TestPlayHappyPathAAC(t *testing.T) {
	au := []byte{0x01, 0x02, 0x03, 0x04}
	c, frames := playAndInject(t, &testserver.HandshakeConfig{SDP: aacSDP}, nil,
		func(sc *testserver.ServerConn, pairs []testserver.ChannelPair) {
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptAAC, 1000, 5000, 0x11223344, true, aacHbrPayload(au)))
		})
	defer closeAndWait(t, c)

	f := recvFrame(t, frames)
	if f.TrackID != 0 {
		t.Errorf("TrackID = %d, want 0", f.TrackID)
	}
	if !bytes.Equal(f.Data, au) {
		t.Errorf("Data = % x, want AU % x", f.Data, au)
	}
	if f.PTS != 0 {
		t.Errorf("first-frame PTS = %v, want 0 (baseline)", f.PTS)
	}
	if f.RTPTime != 5000 {
		t.Errorf("RTPTime = %d, want 5000", f.RTPTime)
	}
}

func TestPlayHappyPathOpus(t *testing.T) {
	payload := []byte{0x78, 0xaa, 0xbb, 0xcc}
	c, frames := playAndInject(t, &testserver.HandshakeConfig{SDP: opusSDP}, nil,
		func(sc *testserver.ServerConn, pairs []testserver.ChannelPair) {
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptOpus, 1, 960, 0xabcdef01, false, payload))
		})
	defer closeAndWait(t, c)

	f := recvFrame(t, frames)
	if !bytes.Equal(f.Data, payload) {
		t.Errorf("Data = % x, want the Opus payload verbatim % x", f.Data, payload)
	}
	if f.PTS != 0 {
		t.Errorf("first-frame PTS = %v, want 0", f.PTS)
	}
}

func TestPlayHappyPathG711MuLaw(t *testing.T) {
	payload := []byte{0x00, 0x7f, 0x80, 0xff, 0x40}
	c, frames := playAndInject(t, &testserver.HandshakeConfig{SDP: g711MuLawSDP}, nil,
		func(sc *testserver.ServerConn, pairs []testserver.ChannelPair) {
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptPCMU, 1, 160, 0x01, false, payload))
		})
	defer closeAndWait(t, c)

	f := recvFrame(t, frames)
	want, derr := g711.DepacketizeAlloc(payload, audiostream.MuLaw)
	if derr != nil {
		t.Fatalf("DepacketizeAlloc: %v", derr)
	}
	if !bytes.Equal(f.Data, want) {
		t.Errorf("Data = % x, want mu-law s16le % x", f.Data, want)
	}
	if len(f.Data) != 2*len(payload) {
		t.Errorf("len(Data) = %d, want %d", len(f.Data), 2*len(payload))
	}
}

func TestPlayHappyPathG711ALaw(t *testing.T) {
	payload := []byte{0x00, 0x7f, 0x80, 0xff, 0x2a}
	c, frames := playAndInject(t, &testserver.HandshakeConfig{SDP: g711ALawSDP}, nil,
		func(sc *testserver.ServerConn, pairs []testserver.ChannelPair) {
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptPCMA, 1, 160, 0x01, false, payload))
		})
	defer closeAndWait(t, c)

	f := recvFrame(t, frames)
	want, derr := g711.DepacketizeAlloc(payload, audiostream.ALaw)
	if derr != nil {
		t.Fatalf("DepacketizeAlloc: %v", derr)
	}
	if !bytes.Equal(f.Data, want) {
		t.Errorf("Data = % x, want A-law s16le % x", f.Data, want)
	}
}

func TestPlayMultipleAUsPerPacket(t *testing.T) {
	au0, au1 := []byte{0x10, 0x11}, []byte{0x20, 0x21, 0x22}
	c, frames := playAndInject(t, &testserver.HandshakeConfig{SDP: aacSDP}, nil,
		func(sc *testserver.ServerConn, pairs []testserver.ChannelPair) {
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptAAC, 1, 8000, 0x55, true, aacHbrPayload(au0, au1)))
		})
	defer closeAndWait(t, c)

	f0 := recvFrame(t, frames)
	f1 := recvFrame(t, frames)
	if !bytes.Equal(f0.Data, au0) || !bytes.Equal(f1.Data, au1) {
		t.Errorf("AU data = % x / % x, want % x / % x", f0.Data, f1.Data, au0, au1)
	}
	if f0.PTS != 0 {
		t.Errorf("AU0 PTS = %v, want 0", f0.PTS)
	}
	// AU1 is one SamplesPerFrame (1024) later at 16000 Hz = 64ms.
	if f1.PTS != 64*time.Millisecond {
		t.Errorf("AU1 PTS = %v, want 64ms", f1.PTS)
	}
	if f1.SeqGap != 0 {
		t.Errorf("AU1 SeqGap = %d, want 0 (only the first AU carries the gap)", f1.SeqGap)
	}
}

func TestPlayFragmentedAAC(t *testing.T) {
	full := make([]byte, 300)
	for i := range full {
		full[i] = byte(i)
	}
	c, frames := playAndInject(t, &testserver.HandshakeConfig{SDP: aacSDP}, nil,
		func(sc *testserver.ServerConn, pairs []testserver.ChannelPair) {
			// Both fragments declare the size of the COMPLETE access unit, as
			// RFC 3640 requires; the depacketizer rejects a continuation that
			// declares only the size of the piece it carries.
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptAAC, 1, 9000, 0x66, false, aacFragment(len(full), full[:150])))
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptAAC, 2, 9000, 0x66, true, aacFragment(len(full), full[150:])))
		})
	defer closeAndWait(t, c)

	f := recvFrame(t, frames)
	if !bytes.Equal(f.Data, full) {
		t.Errorf("reassembled AU len = %d, want %d (fragment start must not deliver on its own)", len(f.Data), len(full))
	}
}

func TestRTPInfoAbsent(t *testing.T) {
	au := []byte{0xaa}
	c, frames := playAndInject(t, &testserver.HandshakeConfig{SDP: aacSDP}, nil,
		func(sc *testserver.ServerConn, pairs []testserver.ChannelPair) {
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptAAC, 1, 12345, 0x77, true, aacHbrPayload(au)))
		})
	defer closeAndWait(t, c)

	f := recvFrame(t, frames)
	if f.PTS != 0 {
		t.Errorf("first-frame PTS = %v, want 0 with no RTP-Info (first-packet baseline)", f.PTS)
	}
}

func TestRTPInfoPresentSeedsOrigin(t *testing.T) {
	au := []byte{0xbb}
	// Seed the origin at 1000; the first packet we actually receive is at
	// 2000 (a packet or two lost at the start). PTS must reflect the 1000-tick
	// offset from the seeded origin, not restart from 0.
	hcfg := &testserver.HandshakeConfig{SDP: aacSDP, RTPInfo: "url=audio;seq=100;rtptime=1000"}
	c, frames := playAndInject(t, hcfg, nil,
		func(sc *testserver.ServerConn, pairs []testserver.ChannelPair) {
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptAAC, 200, 2000, 0x88, true, aacHbrPayload(au)))
		})
	defer closeAndWait(t, c)

	f := recvFrame(t, frames)
	want := (1000 * time.Second) / 16000 // 1000 ticks at 16000 Hz = 62.5ms
	if f.PTS != want {
		t.Errorf("seeded first-frame PTS = %v, want %v (offset from the advertised origin)", f.PTS, want)
	}
}

func TestRTPInfoImplausibleIgnored(t *testing.T) {
	au := []byte{0xcc}
	// The advertised origin (5000) is later than the first packet (2000):
	// implausible, so the seed is discarded and the first-packet baseline
	// stands. PTS must be 0, never negative.
	hcfg := &testserver.HandshakeConfig{SDP: aacSDP, RTPInfo: "url=audio;seq=100;rtptime=5000"}
	c, frames := playAndInject(t, hcfg, nil,
		func(sc *testserver.ServerConn, pairs []testserver.ChannelPair) {
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptAAC, 200, 2000, 0x99, true, aacHbrPayload(au)))
		})
	defer closeAndWait(t, c)

	f := recvFrame(t, frames)
	if f.PTS != 0 {
		t.Errorf("first-frame PTS = %v, want 0 (implausible seed discarded)", f.PTS)
	}
}

func TestDataBeforePlayResponse(t *testing.T) {
	au := []byte{0xd0, 0xd1}
	frames := make(chan audiostream.Frame, 8)
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		serve(t, sc, methodOptions, 200, "OK", publicHeader(), nil)
		serve(t, sc, methodDescribe, 200, "OK", sdpHeaders(""), []byte(aacSDP))
		serve(t, sc, methodSetup, 200, "OK", setupHeaders(0, 1, testSessionID, testTimeoutS), nil)
		// Read PLAY but inject frames BEFORE answering it: early interleaved
		// data is a non-event and must be delivered without a fatal error.
		req, err := sc.ReadRequest()
		if err != nil {
			return
		}
		if err := sc.InjectFrame(0, buildRTPPacket(ptAAC, 1, 3000, 0x11, true, aacHbrPayload(au))); err != nil {
			return
		}
		h := rtsp.Header{}
		h.Set("Session", sessionValue(testSessionID, testTimeoutS))
		if err := sc.Respond(req, 200, "OK", h, nil); err != nil {
			return
		}
		drainRequests(sc)
	}})

	c := dialWithFrames(t, s.URL("/stream"), frames)
	defer closeAndWait(t, c)

	tracks, err := c.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if err := c.Setup(context.Background(), tracks[0], rtsp.SetupOptions{}); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := c.Play(context.Background()); err != nil {
		t.Fatalf("Play must return nil despite early data: %v", err)
	}
	f := recvFrame(t, frames)
	if !bytes.Equal(f.Data, au) {
		t.Errorf("early-data frame Data = % x, want % x", f.Data, au)
	}
}

func TestUnknownChannelSkipped(t *testing.T) {
	p1, p2 := []byte{0x78, 0x01}, []byte{0x78, 0x02}
	c, frames := playAndInject(t, &testserver.HandshakeConfig{SDP: opusSDP}, nil,
		func(sc *testserver.ServerConn, pairs []testserver.ChannelPair) {
			// A frame on a never-set-up channel must be dropped without
			// desynchronizing the framing loop or calling OnFrame.
			_ = sc.InjectFrame(99, []byte{0x00, 0x01, 0x02, 0x03})
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptOpus, 1, 960, 0x01, false, p1))
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptOpus, 2, 1920, 0x01, false, p2))
		})
	defer closeAndWait(t, c)

	f1 := recvFrame(t, frames)
	f2 := recvFrame(t, frames)
	if !bytes.Equal(f1.Data, p1) || !bytes.Equal(f2.Data, p2) {
		t.Errorf("delivered % x / % x, want % x / % x (unknown channel must be skipped cleanly)", f1.Data, f2.Data, p1, p2)
	}
}

// A second format multiplexed onto a track's RTP channel (a telephone-event
// beside speech, or the second entry of a multi-format m= line) is counted and
// dropped rather than fed to the track's depacketizer.
func TestForeignPayloadTypeRejected(t *testing.T) {
	first, second := []byte{0x78, 0x42}, []byte{0x78, 0x43}
	c, frames := playAndInject(t, &testserver.HandshakeConfig{SDP: opusSDP}, nil,
		func(sc *testserver.ServerConn, pairs []testserver.ChannelPair) {
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptOpus, 1, 960, 0x01, false, first))
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptAAC, 2, 1920, 0x01, false, []byte{0xff, 0xfe}))
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptOpus, 3, 2880, 0x01, false, second))
		})
	defer closeAndWait(t, c)

	// Delivery is ordered, so the interloper reaching OnFrame would arrive
	// between these two.
	if f := recvFrame(t, frames); !bytes.Equal(f.Data, first) {
		t.Errorf("first delivered frame = % x, want % x", f.Data, first)
	}
	if f := recvFrame(t, frames); !bytes.Equal(f.Data, second) {
		t.Errorf("second delivered frame = % x, want % x (the foreign PT must not reach OnFrame)", f.Data, second)
	}
	st := waitForStats(t, c, 0, func(ts audiostream.TrackStats) bool { return ts.Packets == 2 })
	if st.Malformed != 1 {
		t.Errorf("Malformed = %d, want 1", st.Malformed)
	}
	if st.Packets != 2 {
		t.Errorf("Packets = %d, want 2 (the rejected packet must not be counted as accepted)", st.Packets)
	}
	// The rejected packet still consumed a sequence number, so the tracker sees
	// a hole. That is the documented cost of rejecting before observing: loss
	// accounting cannot tell a dropped interloper from a lost packet, and the
	// alternative (observing first) would let a foreign SSRC or timestamp
	// corrupt this track's baseline, which is far more expensive.
	if st.SeqGaps != 1 {
		t.Errorf("SeqGaps = %d, want 1 (the rejected packet leaves a hole in the sequence space)", st.SeqGaps)
	}
}

// A camera whose stream consistently carries a payload type its own SDP did not
// declare keeps working. Enforcing the SDP's value would reject every packet,
// deliver nothing, and never trip the read-idle watchdog, because frames would
// still be arriving: a silent stall rather than a diagnosable failure.
func TestPayloadTypeDisagreeingWithSDPStillDelivers(t *testing.T) {
	payload := []byte{0x78, 0x11}
	const sent = 8 // enough to cross the adopt threshold and then some
	c, frames := playAndInject(t, &testserver.HandshakeConfig{SDP: opusSDP}, nil,
		func(sc *testserver.ServerConn, pairs []testserver.ChannelPair) {
			// opusSDP declares PT 96; this camera sends PT 98 throughout.
			for i := range sent {
				_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(98, uint16(1+i), uint32(960*(1+i)), 0x01, false, payload))
			}
		})
	defer closeAndWait(t, c)

	// The prologue before the stream's type is adopted is dropped; everything
	// after it is delivered.
	if f := recvFrame(t, frames); !bytes.Equal(f.Data, payload) {
		t.Errorf("first delivered frame = % x, want % x", f.Data, payload)
	}
	st := waitForStats(t, c, 0, func(ts audiostream.TrackStats) bool {
		return ts.Packets+ts.Malformed == sent
	})
	if st.Packets == 0 {
		t.Error("no packet was ever delivered; the stream's payload type was never adopted")
	}
	if st.Malformed >= sent {
		t.Errorf("Malformed = %d of %d, want only the prologue before adoption", st.Malformed, sent)
	}
}

// While resynchronizing, an interleaved frame on a channel this session bound
// re-locks the framing loop. Without that, a desync during playback could never
// recover, because a playing stream is almost entirely interleaved frames.
func TestResyncRelocksOnBoundChannel(t *testing.T) {
	payload := []byte{0x78, 0x99}
	c, frames := playAndInject(t, &testserver.HandshakeConfig{SDP: opusSDP}, nil,
		func(sc *testserver.ServerConn, pairs []testserver.ChannelPair) {
			// Bytes that classify as neither a message nor a frame start, so
			// the reader is resynchronizing when the real frame arrives.
			_ = sc.WriteRaw(unclassifiableBytes(64))
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptOpus, 1, 960, 0x01, false, payload))
		})
	defer closeAndWait(t, c)

	f := recvFrame(t, frames)
	if !bytes.Equal(f.Data, payload) {
		t.Errorf("delivered % x, want % x", f.Data, payload)
	}
}

// Interleaved data that arrives before the SETUP response is dropped: the
// routing table is published only once the server has confirmed the channel
// pair. This pins the trade in publishTrack's doc comment, so a future change
// to pre-publish the proposal is a deliberate one.
func TestDataBeforeSetupResponseDropped(t *testing.T) {
	early, late := []byte{0xe0, 0xe1}, []byte{0x1a, 0x1e}
	frames := make(chan audiostream.Frame, 8)
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		serve(t, sc, methodOptions, 200, "OK", publicHeader(), nil)
		serve(t, sc, methodDescribe, 200, "OK", sdpHeaders(""), []byte(opusSDP))

		req, err := sc.ReadRequest()
		if err != nil {
			return
		}
		if err := sc.InjectFrame(0, buildRTPPacket(ptOpus, 1, 960, 0x01, false, early)); err != nil {
			return
		}
		if err := sc.Respond(req, 200, "OK", setupHeaders(0, 1, testSessionID, testTimeoutS), nil); err != nil {
			return
		}

		playHeaders := rtsp.Header{}
		playHeaders.Set("Session", sessionValue(testSessionID, testTimeoutS))
		serve(t, sc, methodPlay, 200, "OK", playHeaders, nil)
		_ = sc.InjectFrame(0, buildRTPPacket(ptOpus, 2, 1920, 0x01, false, late))
		drainRequests(sc)
	}})

	c := dialWithFrames(t, s.URL("/stream"), frames)
	defer closeAndWait(t, c)

	tracks, err := c.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if err := c.Setup(context.Background(), tracks[0], rtsp.SetupOptions{}); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := c.Play(context.Background()); err != nil {
		t.Fatalf("Play: %v", err)
	}

	// Delivery is ordered, so the first frame out is the late one only if the
	// early frame was dropped for want of a routing table.
	f := recvFrame(t, frames)
	if !bytes.Equal(f.Data, late) {
		t.Errorf("delivered % x, want % x (data before the SETUP response is not routable)", f.Data, late)
	}
}

func TestSSRCChangeResets(t *testing.T) {
	c, frames := playAndInject(t, &testserver.HandshakeConfig{SDP: opusSDP}, nil,
		func(sc *testserver.ServerConn, pairs []testserver.ChannelPair) {
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptOpus, 1, 960, 0xAAAA0001, false, []byte{0x78, 0x01}))
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptOpus, 2, 1920, 0xAAAA0001, false, []byte{0x78, 0x02}))
			// A new SSRC mid-stream: tolerated, counted, delivery continues.
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptOpus, 500, 100, 0xBBBB0002, false, []byte{0x78, 0x03}))
		})
	defer closeAndWait(t, c)

	for range 3 {
		recvFrame(t, frames)
	}
	st := waitForStats(t, c, 0, func(ts audiostream.TrackStats) bool { return ts.Packets == 3 })
	if st.SSRCResets != 1 {
		t.Errorf("SSRCResets = %d, want 1", st.SSRCResets)
	}
	if st.Packets != 3 {
		t.Errorf("Packets = %d, want 3", st.Packets)
	}
}

func TestSSRCChangeResetsWithSeededOrigin(t *testing.T) {
	// RTP-Info seeds the origin at 48000. The first stream (ssrc 0xA1) starts a
	// second later (ts 96000), so its first frame's PTS reflects the seeded
	// offset. After a mid-stream SSRC change to a much later timestamp, the new
	// stream must re-baseline from its own first packet (PTS restarts at 0);
	// the stale origin must NOT be re-applied.
	hcfg := &testserver.HandshakeConfig{SDP: opusSDP, RTPInfo: "url=audio;seq=100;rtptime=48000"}
	c, frames := playAndInject(t, hcfg, nil,
		func(sc *testserver.ServerConn, pairs []testserver.ChannelPair) {
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptOpus, 100, 96000, 0xA1, false, []byte{0x78, 0x01}))
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptOpus, 500, 480000, 0xB2, false, []byte{0x78, 0x02}))
		})
	defer closeAndWait(t, c)

	f0 := recvFrame(t, frames)
	if f0.PTS != time.Second { // (96000 - 48000) / 48000 Hz = 1s, from the seeded origin
		t.Errorf("pre-change PTS = %v, want 1s (seeded origin applied to the first stream)", f0.PTS)
	}
	f1 := recvFrame(t, frames)
	if f1.PTS != 0 {
		t.Errorf("post-SSRC-change PTS = %v, want 0 (new baseline from the first post-change packet, not the stale seed)", f1.PTS)
	}
	st := waitForStats(t, c, 0, func(ts audiostream.TrackStats) bool { return ts.SSRCResets == 1 })
	if st.SSRCResets != 1 {
		t.Errorf("SSRCResets = %d, want 1", st.SSRCResets)
	}
}

func TestSeqGapReported(t *testing.T) {
	full := make([]byte, 40)
	for i := range full {
		full[i] = byte(0xF0 + i%16)
	}
	c, frames := playAndInject(t, &testserver.HandshakeConfig{SDP: aacSDP}, nil,
		func(sc *testserver.ServerConn, pairs []testserver.ChannelPair) {
			// A fragment start that is never completed, then a one-sequence gap
			// followed by a complete packet. The depacketizer must reset on the
			// gap so the complete packet is not mistaken for a continuation.
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptAAC, 100, 500, 0x01, false, aacFragment(200, make([]byte, 50))))
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptAAC, 102, 1000, 0x01, true, aacHbrPayload(full)))
		})
	defer closeAndWait(t, c)

	f := recvFrame(t, frames)
	if f.SeqGap != 1 {
		t.Errorf("SeqGap = %d, want 1", f.SeqGap)
	}
	if !bytes.Equal(f.Data, full) {
		t.Errorf("Data len = %d, want %d (depacketizer must have reset on the gap)", len(f.Data), len(full))
	}
}

func TestDiscardTrackNotDelivered(t *testing.T) {
	// audioVideoSDP: track 0 audio (AAC), track 1 video (discarded).
	au0, au1 := []byte{0x01}, []byte{0x02}
	c, frames := playAndInject(t, &testserver.HandshakeConfig{SDP: audioVideoSDP},
		func(i int) bool { return i == 1 }, // discard the video track
		func(sc *testserver.ServerConn, pairs []testserver.ChannelPair) {
			// pairs[0] = audio (RTP ch 0), pairs[1] = video (RTP ch 2).
			_ = sc.InjectFrame(pairs[1].RTP, buildRTPPacket(ptH264, 1, 90000, 0x0F, true, []byte{0xde, 0xad}))
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptAAC, 1, 8000, 0xA0, true, aacHbrPayload(au0)))
			_ = sc.InjectFrame(pairs[1].RTP, buildRTPPacket(ptH264, 2, 93600, 0x0F, true, []byte{0xbe, 0xef}))
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptAAC, 2, 9024, 0xA0, true, aacHbrPayload(au1)))
		})
	defer closeAndWait(t, c)

	f0 := recvFrame(t, frames)
	f1 := recvFrame(t, frames)
	if f0.TrackID != 0 || f1.TrackID != 0 {
		t.Errorf("delivered TrackIDs %d and %d, want only the audio track 0", f0.TrackID, f1.TrackID)
	}
	if !bytes.Equal(f0.Data, au0) || !bytes.Equal(f1.Data, au1) {
		t.Errorf("audio AUs = % x / % x, want % x / % x", f0.Data, f1.Data, au0, au1)
	}
	// The discarded video track counts packets but delivers nothing.
	if st := waitForStats(t, c, 1, func(ts audiostream.TrackStats) bool { return ts.Packets == 2 }); st.Packets != 2 {
		t.Errorf("discarded track Packets = %d, want 2", st.Packets)
	}
}

// The same bound must hold when the gate-admitted channel is an RTCP channel.
// A minimal Receiver Report (8 bytes, no Sender Report) parses as a valid RTCP
// compound, so treating it as a re-lock would let a run of garbage bracketing
// forged tiny RTCP frames hold the session open indefinitely. Only a real
// Sender Report is strong enough evidence to clear the budget.
func TestResyncBudgetStillBoundedByRTCPChannel(t *testing.T) {
	// V=2, RC=0, PT=201 (Receiver Report), length 1 word, plus a reporter SSRC:
	// a well-formed 8-byte RTCP compound that carries no Sender Report.
	rr := []byte{'$', 0x01, 0x00, 0x08, 0x80, 0xC9, 0x00, 0x01, 0xCA, 0xFE, 0xBA, 0xBE}
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(closeRelease)

	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		if _, err := sc.Handshake(testserver.HandshakeConfig{
			SDP:            opusSDP,
			SessionID:      testSessionID,
			SessionTimeout: testTimeoutS,
		}); err != nil {
			return
		}
		<-release
		junk := make([]byte, 0, 32*1024)
		for range 10 {
			junk = append(junk, unclassifiableBytes(2000)...)
			junk = append(junk, rr...)
		}
		_ = sc.WriteRaw(junk)
		drainRequests(sc)
	}})

	c := dialIdle(t, s.URL("/stream"))
	defer func() { _ = c.Close() }()
	describeSetupPlay(t, c, nil)
	closeRelease()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	err := c.Wait(ctx)
	if err == nil || !strings.Contains(err.Error(), "resync exceeded") {
		t.Errorf("Wait = %v, want a resync budget framing error; an RTCP frame carrying "+
			"no Sender Report must not clear the budget", err)
	}
}

// The same bound must hold when the gate-admitted channel belongs to a
// DISCARDED track. A discard track is never delivered from, so treating its
// frames as proof of a re-lock on the strength of the channel byte alone would
// reopen the hole for any session that discards a video track, which is the
// configuration SetupOptions.Discard exists for.
func TestResyncBudgetStillBoundedWithADiscardedTrack(t *testing.T) {
	// audioVideoSDP gives track 0 channels 0-1 and track 1 channels 2-3; only
	// track 1 is discarded. Channel 2 is therefore bound to a discard track.
	fake := []byte{'$', 0x02, 0x00, 0x02, 0xAB, 0xCD}
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(closeRelease)

	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		if _, err := sc.Handshake(testserver.HandshakeConfig{
			SDP:            audioVideoSDP,
			SessionID:      testSessionID,
			SessionTimeout: testTimeoutS,
		}); err != nil {
			return
		}
		<-release
		junk := make([]byte, 0, 16*1024)
		for range 8 {
			junk = append(junk, unclassifiableBytes(2000)...)
			junk = append(junk, fake...)
		}
		_ = sc.WriteRaw(junk)
		drainRequests(sc)
	}})

	c := dialIdle(t, s.URL("/stream"))
	defer func() { _ = c.Close() }()
	describeSetupPlay(t, c, func(i int) bool { return i == 1 })
	closeRelease()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	err := c.Wait(ctx)
	if err == nil || !strings.Contains(err.Error(), "resync exceeded") {
		t.Errorf("Wait = %v, want a resync budget framing error; a discarded track's "+
			"channel must not vouch for a frame the gate admitted", err)
	}
}

// A camera whose SDP declares one payload type but whose stream opens with a
// different one (a session joined mid-DTMF) must still deliver the declared
// format once it arrives, rather than latching onto the interloper and dropping
// every real packet for the rest of the session.
func TestInterloperFirstDoesNotCaptureTheTrack(t *testing.T) {
	dtmf, speech := []byte{0x00, 0x0a, 0x00, 0x50}, []byte{0x78, 0x42}
	c, frames := playAndInject(t, &testserver.HandshakeConfig{SDP: opusSDP}, nil,
		func(sc *testserver.ServerConn, pairs []testserver.ChannelPair) {
			// opusSDP declares PT 96; 101 is a telephone-event arriving first.
			_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(101, 1, 960, 0x01, false, dtmf))
			for i := range 3 {
				_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptOpus, uint16(2+i), uint32(1920+960*i), 0x01, false, speech))
			}
		})
	defer closeAndWait(t, c)

	for i := range 3 {
		if f := recvFrame(t, frames); !bytes.Equal(f.Data, speech) {
			t.Fatalf("frame %d = % x, want the speech payload % x", i, f.Data, speech)
		}
	}
	st := waitForStats(t, c, 0, func(ts audiostream.TrackStats) bool { return ts.Packets == 3 })
	if st.Malformed != 1 {
		t.Errorf("Malformed = %d, want 1 (only the telephone-event)", st.Malformed)
	}
}

func TestPlayWithoutSetup(t *testing.T) {
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		serve(t, sc, methodOptions, 200, "OK", publicHeader(), nil)
		serve(t, sc, methodDescribe, 200, "OK", sdpHeaders(""), []byte(aacSDP))
		drainRequests(sc)
	}})

	c := dialIdle(t, s.URL("/stream"))
	defer closeAndWait(t, c)
	if _, err := c.Describe(context.Background()); err != nil {
		t.Fatalf("Describe: %v", err)
	}
	err := c.Play(context.Background())
	var se *rtsp.StateError
	if !errors.As(err, &se) {
		t.Fatalf("Play without Setup = %v, want *StateError", err)
	}
	if !errors.Is(err, rtsp.ErrInvalidState) {
		t.Errorf("Play without Setup does not match ErrInvalidState")
	}
}

// The resync budget must still terminate a session once a channel IS bound.
// The gate admits a frame whose channel byte the routing table knows, so a run
// of garbage that happens to present such a header would otherwise clear the
// budget every few thousand bytes and hold the session open forever, delivering
// nothing while the read-idle watchdog was re-armed by each fake frame.
func TestResyncBudgetStillBoundedWithABoundChannel(t *testing.T) {
	// A header that passes the gate (channel 0 is bound) but whose payload is
	// far too short to be an RTP packet, so it can never prove usable.
	fake := []byte{'$', 0x00, 0x00, 0x02, 0xAB, 0xCD}
	// Held until Play has returned. Writing the junk straight after the
	// handshake let it share a TCP segment with the PLAY 200, so the reader
	// could exhaust the budget and record the terminal error before Play
	// re-checked it, failing Play itself rather than the session.
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(closeRelease)

	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		if _, err := sc.Handshake(testserver.HandshakeConfig{
			SDP:            opusSDP,
			SessionID:      testSessionID,
			SessionTimeout: testTimeoutS,
		}); err != nil {
			return
		}
		<-release
		junk := make([]byte, 0, 8*1024)
		for range 4 {
			junk = append(junk, unclassifiableBytes(2000)...)
			junk = append(junk, fake...)
		}
		_ = sc.WriteRaw(junk)
		drainRequests(sc)
	}})

	c := dialIdle(t, s.URL("/stream"))
	defer func() { _ = c.Close() }()
	describeSetupPlay(t, c, nil)
	closeRelease()

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	err := c.Wait(ctx)
	if err == nil || !strings.Contains(err.Error(), "resync exceeded") {
		t.Errorf("Wait = %v, want a resync budget framing error; a frame the gate admits "+
			"must not clear the budget until it has proved usable", err)
	}
}
