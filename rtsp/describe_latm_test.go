package rtsp_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/internal/testserver"
	"github.com/tphakala/go-audio-stream/rtsp"
)

// ptLATM is the payload type the LATM SDP fixtures below declare.
const ptLATM uint8 = 96

// latmOutOfBandSDP declares an MP4A-LATM track whose StreamMuxConfig is
// carried out-of-band in the fmtp config= (the depacket/latm V1 vector), the
// shape a camera that never signals useSameStreamMux=0 in the stream uses.
const latmOutOfBandSDP = "v=0\r\n" +
	"o=- 0 0 IN IP4 127.0.0.1\r\n" +
	"s=Stream\r\n" +
	"m=audio 0 RTP/AVP 96\r\n" +
	"a=rtpmap:96 MP4A-LATM/44100/2\r\n" +
	"a=fmtp:96 cpresent=0;object=2;config=400024203fc0\r\n" +
	"a=control:audio\r\n"

// latmInBandSDP declares an MP4A-LATM track whose fmtp carries no config=
// and cpresent=1 (in-band): the ASC is not known until the stream carries
// it, so it must be nil on the Track Describe returns.
const latmInBandSDP = "v=0\r\n" +
	"o=- 0 0 IN IP4 127.0.0.1\r\n" +
	"s=Stream\r\n" +
	"m=audio 0 RTP/AVP 96\r\n" +
	"a=rtpmap:96 MP4A-LATM/44100/2\r\n" +
	"a=fmtp:96 cpresent=1\r\n" +
	"a=control:audio\r\n"

// latmV4Payload is the in-band AudioMuxElement test vector (depacket/latm V4):
// useSameStreamMux 0, an inline StreamMuxConfig equal to V1, PayloadLengthInfo
// 03, payload AA BB CC, ByteAlign padding. It decodes to ASC 12 10 and one AU
// AA BB CC.
var latmV4Payload = []byte{0x20, 0x00, 0x12, 0x10, 0x1F, 0xE0, 0x1D, 0x55, 0xDE, 0x60}

// TestDescribeLATMOutOfBandExtractsASC covers the out-of-band ASC-delivery
// route: Describe must extract the AudioSpecificConfig from the fmtp
// StreamMuxConfig itself, so the returned Track already carries what a
// consumer needs to initialize its decoder, with no need to wait for
// OnCodecUpdate.
func TestDescribeLATMOutOfBandExtractsASC(t *testing.T) {
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		serve(t, sc, methodOptions, 200, "OK", publicHeader(), nil)
		serve(t, sc, methodDescribe, 200, "OK", sdpHeaders(""), []byte(latmOutOfBandSDP))
		drainRequests(sc)
	}})

	c := dialIdle(t, s.URL("/stream"))
	defer closeAndWait(t, c)

	tracks, err := c.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if len(tracks) != 1 {
		t.Fatalf("track count = %d, want 1", len(tracks))
	}

	latm, ok := tracks[0].Codec.(audiostream.CodecMP4ALATM)
	if !ok {
		t.Fatalf("Codec = %T, want CodecMP4ALATM", tracks[0].Codec)
	}
	if latm.MuxConfigPresent {
		t.Error("MuxConfigPresent = true, want false")
	}
	if want := []byte{0x12, 0x10}; !bytes.Equal(latm.AudioSpecificConfig, want) {
		t.Errorf("AudioSpecificConfig = % x, want % x extracted at Describe time", latm.AudioSpecificConfig, want)
	}
}

// TestDescribeLATMInBandASCUnknownAtDescribe covers the in-band case: the ASC
// is not known until the stream carries it, so it must stay nil on the Track
// Describe returns; it becomes available later through OnCodecUpdate.
func TestDescribeLATMInBandASCUnknownAtDescribe(t *testing.T) {
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		serve(t, sc, methodOptions, 200, "OK", publicHeader(), nil)
		serve(t, sc, methodDescribe, 200, "OK", sdpHeaders(""), []byte(latmInBandSDP))
		drainRequests(sc)
	}})

	c := dialIdle(t, s.URL("/stream"))
	defer closeAndWait(t, c)

	tracks, err := c.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if len(tracks) != 1 {
		t.Fatalf("track count = %d, want 1", len(tracks))
	}

	latm, ok := tracks[0].Codec.(audiostream.CodecMP4ALATM)
	if !ok {
		t.Fatalf("Codec = %T, want CodecMP4ALATM", tracks[0].Codec)
	}
	if !latm.MuxConfigPresent {
		t.Error("MuxConfigPresent = false, want true")
	}
	if latm.AudioSpecificConfig != nil {
		t.Errorf("AudioSpecificConfig = % x, want nil before the stream carries it", latm.AudioSpecificConfig)
	}
}

// latmOutOfBandMalformedSDP declares an out-of-band MP4A-LATM track (cpresent=0)
// whose config= is valid hex but an unsupported StreamMuxConfig: 400050 has
// audioObjectType 5, which latm.New rejects. It exercises resolveLATMASC's error
// branch.
const latmOutOfBandMalformedSDP = "v=0\r\n" +
	"o=- 0 0 IN IP4 127.0.0.1\r\n" +
	"s=Stream\r\n" +
	"m=audio 0 RTP/AVP 96\r\n" +
	"a=rtpmap:96 MP4A-LATM/44100/2\r\n" +
	"a=fmtp:96 cpresent=0;object=2;config=400050\r\n" +
	"a=control:audio\r\n"

// TestDescribeLATMOutOfBandMalformedConfigASCNil covers resolveLATMASC's error
// branch: an out-of-band track whose StreamMuxConfig latm.New cannot parse still
// describes successfully, with AudioSpecificConfig left nil rather than failing
// the whole Describe. The track sets up and falls back to raw delivery later.
func TestDescribeLATMOutOfBandMalformedConfigASCNil(t *testing.T) {
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		serve(t, sc, methodOptions, 200, "OK", publicHeader(), nil)
		serve(t, sc, methodDescribe, 200, "OK", sdpHeaders(""), []byte(latmOutOfBandMalformedSDP))
		drainRequests(sc)
	}})

	c := dialIdle(t, s.URL("/stream"))
	defer closeAndWait(t, c)

	tracks, err := c.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if len(tracks) != 1 {
		t.Fatalf("track count = %d, want 1", len(tracks))
	}

	latm, ok := tracks[0].Codec.(audiostream.CodecMP4ALATM)
	if !ok {
		t.Fatalf("Codec = %T, want CodecMP4ALATM", tracks[0].Codec)
	}
	if latm.MuxConfigPresent {
		t.Error("MuxConfigPresent = true, want false")
	}
	if latm.AudioSpecificConfig != nil {
		t.Errorf("AudioSpecificConfig = % x, want nil (malformed StreamMuxConfig, resolveLATMASC error branch)", latm.AudioSpecificConfig)
	}
}

// latmEvent records one callback firing during TestOnCodecUpdateEndToEnd, so
// the test can assert both that OnCodecUpdate fired and that it preceded the
// OnFrame call for the first AU decoded under the newly resolved config.
type latmEvent struct {
	kind string // "update" or "frame"
	asc  []byte
}

// TestOnCodecUpdateEndToEnd drives a full Describe/Setup/Play/deliver cycle
// for an in-band LATM track over an interleaved connection and asserts
// OnCodecUpdate fires, carrying the resolved AudioSpecificConfig, before the
// OnFrame call for the first access unit decoded under that config: the
// ordering guarantee Config.OnCodecUpdate documents.
func TestOnCodecUpdateEndToEnd(t *testing.T) {
	events := make(chan latmEvent, 8)

	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		pairs, err := sc.Handshake(testserver.HandshakeConfig{
			SDP:            latmInBandSDP,
			SessionID:      testSessionID,
			SessionTimeout: testTimeoutS,
		})
		if err != nil {
			return
		}
		_ = sc.InjectFrame(pairs[0].RTP, buildRTPPacket(ptLATM, 1, 1024, 0xA1B2C3D4, true, latmV4Payload))
		drainRequests(sc)
	}})

	c, err := rtsp.Dial(context.Background(), rtsp.Config{
		URL:     s.URL("/stream"),
		Timeout: testTimeout,
		OnFrame: func(f audiostream.Frame) {
			events <- latmEvent{kind: "frame"}
		},
		OnCodecUpdate: func(trackID int, codec audiostream.Codec) {
			latm, _ := codec.(audiostream.CodecMP4ALATM)
			events <- latmEvent{kind: "update", asc: append([]byte(nil), latm.AudioSpecificConfig...)}
		},
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer closeAndWait(t, c)

	describeSetupPlay(t, c, nil)

	first := recvLATMEvent(t, events)
	if first.kind != "update" {
		t.Fatalf("first event = %q, want update to precede the frame", first.kind)
	}
	if want := []byte{0x12, 0x10}; !bytes.Equal(first.asc, want) {
		t.Errorf("OnCodecUpdate ASC = % x, want % x", first.asc, want)
	}

	second := recvLATMEvent(t, events)
	if second.kind != "frame" {
		t.Errorf("second event = %q, want frame", second.kind)
	}
}

// recvLATMEvent reads one event or fails the test on timeout.
func recvLATMEvent(t *testing.T, ch <-chan latmEvent) latmEvent {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for an OnCodecUpdate/OnFrame event")
		return latmEvent{}
	}
}
