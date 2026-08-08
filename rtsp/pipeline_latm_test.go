package rtsp

import (
	"bytes"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/rtsp/rtp"
)

// LATM test vectors shared with depacket/latm (docs/plans/2026-07-23-phase2-latm-plan.md).

// latmV1 is the out-of-band StreamMuxConfig vector V1: AAC-LC, 44.1 kHz,
// stereo, one subframe, frameLengthType 0. Decodes to ASC 12 10.
var latmV1 = []byte{0x40, 0x00, 0x24, 0x20, 0x3F, 0xC0}

// latmV2 is the out-of-band AudioMuxElement vector V2 paired with V1: one
// subframe, PayloadLengthInfo 03, payload AA BB CC.
var latmV2 = []byte{0x03, 0xAA, 0xBB, 0xCC}

// latmV3 is the out-of-band StreamMuxConfig vector V3: same ASC as V1 but
// numSubFrames 1 (two access units per AudioMuxElement).
var latmV3 = []byte{0x41, 0x00, 0x24, 0x20, 0x3F, 0xC0}

// latmV3Payload is the AudioMuxElement paired with V3: two subframes,
// PayloadLengthInfo 02 / payload 11 22, PayloadLengthInfo 03 / payload 33 44 55.
var latmV3Payload = []byte{0x02, 0x11, 0x22, 0x03, 0x33, 0x44, 0x55}

// latmV4 is the in-band AudioMuxElement vector V4: useSameStreamMux 0, an
// inline StreamMuxConfig equal to V1, PayloadLengthInfo 03, payload AA BB CC.
// Decodes to ASC 12 10 and one AU AA BB CC.
var latmV4 = []byte{0x20, 0x00, 0x12, 0x10, 0x1F, 0xE0, 0x1D, 0x55, 0xDE, 0x60}

// latmV5 is the in-band AudioMuxElement vector V5 paired with V4:
// useSameStreamMux 1, reusing V4's retained StreamMuxConfig.
var latmV5 = []byte{0x81, 0xD5, 0x5D, 0xE6, 0x00}

// latmASC is the AudioSpecificConfig both V1 and V4 resolve to.
var latmASC = []byte{0x12, 0x10}

func TestNewTrackLATMOutOfBandDelivery(t *testing.T) {
	t.Parallel()
	desc := describedTrack{
		codec:     audiostream.CodecMP4ALATM{StreamMuxConfig: latmV1, MuxConfigPresent: false},
		clockRate: 44100,
		media:     audiostream.MediaAudio,
	}
	tr := newTrack(0, desc, SetupOptions{}, 1, nil)
	if tr.kind != deliverLATM {
		t.Fatalf("kind = %d, want deliverLATM", tr.kind)
	}
	if tr.latm == nil {
		t.Fatal("latm depacketizer is nil")
	}
	tr.baseSet.Store(true)

	pkt := rtp.Packet{Header: rtp.Header{Timestamp: 1000, Marker: true}, Payload: latmV2}
	var frames []audiostream.Frame
	tr.deliver(pkt, rtp.Update{Timestamp: 1000, Gap: 2}, time.Unix(1, 0), func(f audiostream.Frame) {
		frames = append(frames, copyFrame(&f))
	})
	if len(frames) != 1 {
		t.Fatalf("delivered %d frames, want 1", len(frames))
	}
	want := []byte{0xAA, 0xBB, 0xCC}
	if !bytes.Equal(frames[0].Data, want) {
		t.Errorf("Data = % x, want % x", frames[0].Data, want)
	}
	if frames[0].SeqGap != 2 {
		t.Errorf("SeqGap = %d, want 2", frames[0].SeqGap)
	}
	if frames[0].RTPTime != 1000 {
		t.Errorf("RTPTime = %d, want 1000", frames[0].RTPTime)
	}
}

// TestNewTrackLATMMultiSubframeOffsets covers the multi-subframe RTPOffset
// math threading through deliverLATM's PTS computation, mirroring
// TestDeliverAACMultipleAUs.
func TestNewTrackLATMMultiSubframeOffsets(t *testing.T) {
	t.Parallel()
	desc := describedTrack{
		codec:     audiostream.CodecMP4ALATM{StreamMuxConfig: latmV3, MuxConfigPresent: false},
		clockRate: 44100,
		media:     audiostream.MediaAudio,
	}
	tr := newTrack(0, desc, SetupOptions{}, 1, nil)
	if tr.kind != deliverLATM {
		t.Fatalf("kind = %d, want deliverLATM", tr.kind)
	}
	tr.baseTS = 5000
	tr.baseSet.Store(true)

	pkt := rtp.Packet{Header: rtp.Header{Timestamp: 5000, Marker: true}, Payload: latmV3Payload}
	var frames []audiostream.Frame
	tr.deliver(pkt, rtp.Update{Timestamp: 5000}, time.Unix(1, 0), func(f audiostream.Frame) {
		frames = append(frames, copyFrame(&f))
	})
	if len(frames) != 2 {
		t.Fatalf("delivered %d frames, want 2", len(frames))
	}
	if want := []byte{0x11, 0x22}; !bytes.Equal(frames[0].Data, want) {
		t.Errorf("frame 0 Data = % x, want % x", frames[0].Data, want)
	}
	if want := []byte{0x33, 0x44, 0x55}; !bytes.Equal(frames[1].Data, want) {
		t.Errorf("frame 1 Data = % x, want % x", frames[1].Data, want)
	}
	if frames[0].PTS != 0 {
		t.Errorf("frame 0 PTS = %v, want 0", frames[0].PTS)
	}
	// The second AU's RTPOffset is 1024 ticks at 44100 Hz.
	wantPTS := time.Duration(1024) * time.Second / 44100
	if frames[1].PTS != wantPTS {
		t.Errorf("frame 1 PTS = %v, want %v", frames[1].PTS, wantPTS)
	}
}

func TestDeliverLATMMalformed(t *testing.T) {
	t.Parallel()
	desc := describedTrack{
		codec:     audiostream.CodecMP4ALATM{StreamMuxConfig: latmV1, MuxConfigPresent: false},
		clockRate: 44100,
		media:     audiostream.MediaAudio,
	}
	tr := newTrack(0, desc, SetupOptions{}, 1, nil)
	if tr.kind != deliverLATM {
		t.Fatalf("kind = %d, want deliverLATM", tr.kind)
	}
	tr.baseSet.Store(true)

	// An empty payload is truncated before the PayloadLengthInfo byte.
	pkt := rtp.Packet{Header: rtp.Header{Marker: true}, Payload: []byte{}}
	n := 0
	tr.deliver(pkt, rtp.Update{}, time.Unix(1, 0), func(audiostream.Frame) { n++ })
	if n != 0 {
		t.Errorf("delivered %d frames for a malformed LATM packet, want 0", n)
	}
	if tr.malformed.Load() != 1 {
		t.Errorf("malformed = %d, want 1", tr.malformed.Load())
	}
}

// TestNewTrackInvalidLATMConfigFallsBackToRaw covers an out-of-band track
// whose StreamMuxConfig is empty, which latm.New rejects; newTrack must
// degrade to raw delivery rather than failing Setup.
func TestNewTrackInvalidLATMConfigFallsBackToRaw(t *testing.T) {
	t.Parallel()
	desc := describedTrack{
		codec: audiostream.CodecMP4ALATM{MuxConfigPresent: false, StreamMuxConfig: nil},
		media: audiostream.MediaAudio,
	}
	tr := newTrack(0, desc, SetupOptions{}, 1, nil)
	if tr.kind != deliverRaw {
		t.Errorf("kind = %d, want deliverRaw for an invalid LATM config", tr.kind)
	}
	if tr.latm != nil {
		t.Error("latm depacketizer must be nil when the config is invalid")
	}
}

// TestNewTrackLATMNonAudioFallsBackToRaw mirrors
// TestNewTrackNonAudioFallsBackToRaw for LATM: the media check runs before
// codec dispatch, so a video section that happens to advertise MP4A-LATM
// never reaches the LATM depacketizer.
func TestNewTrackLATMNonAudioFallsBackToRaw(t *testing.T) {
	t.Parallel()
	desc := describedTrack{
		codec: audiostream.CodecMP4ALATM{StreamMuxConfig: latmV1, MuxConfigPresent: false},
		media: audiostream.MediaVideo,
	}
	tr := newTrack(0, desc, SetupOptions{}, 1, nil)
	if tr.kind != deliverRaw {
		t.Errorf("kind = %d, want deliverRaw for a non-audio track", tr.kind)
	}
	if tr.latm != nil {
		t.Error("a non-audio track must not get a LATM depacketizer")
	}
}

// newInBandLATMTrack builds a track configured for in-band MP4A-LATM
// (MuxConfigPresent true, no StreamMuxConfig), matching a camera that never
// puts config= in its fmtp.
func newInBandLATMTrack(t *testing.T) *track {
	t.Helper()
	desc := describedTrack{
		codec:     audiostream.CodecMP4ALATM{MuxConfigPresent: true},
		clockRate: 44100,
		media:     audiostream.MediaAudio,
	}
	tr := newTrack(0, desc, SetupOptions{}, 1, nil)
	if tr.kind != deliverLATM {
		t.Fatalf("kind = %d, want deliverLATM", tr.kind)
	}
	if tr.latm == nil {
		t.Fatal("latm depacketizer is nil")
	}
	tr.baseSet.Store(true)
	return tr
}

// Event kinds codecUpdateRecording records, named constants rather than
// inline literals since goconst flags the repeated string otherwise.
const (
	latmEventUpdate = "update"
	latmEventFrame  = "frame"
)

// codecUpdateRecording captures both the OnCodecUpdate and OnFrame callbacks
// in firing order, so a test can assert the documented ordering guarantee.
type codecUpdateRecording struct {
	events []string
	ascs   [][]byte
}

func (r *codecUpdateRecording) onCodecUpdate(trackID int, codec audiostream.Codec) {
	r.events = append(r.events, latmEventUpdate)
	c, _ := codec.(audiostream.CodecMP4ALATM)
	r.ascs = append(r.ascs, append([]byte(nil), c.AudioSpecificConfig...))
}

func (r *codecUpdateRecording) onFrame(audiostream.Frame) {
	r.events = append(r.events, latmEventFrame)
}

// TestDeliverLATMOnCodecUpdateFiresBeforeFirstFrame covers the primary
// ordering guarantee: an in-band track fed the V4 packet (which carries the
// config inline) fires OnCodecUpdate exactly once, with the resolved ASC,
// strictly before the OnFrame call for the first AU decoded under that
// config.
func TestDeliverLATMOnCodecUpdateFiresBeforeFirstFrame(t *testing.T) {
	t.Parallel()
	tr := newInBandLATMTrack(t)
	var rec codecUpdateRecording

	pkt := rtp.Packet{Header: rtp.Header{Timestamp: 0, Marker: true}, Payload: latmV4}
	tr.deliverLATM(pkt, rtp.Update{Timestamp: 0}, time.Unix(1, 0), rec.onFrame, rec.onCodecUpdate)

	if len(rec.events) != 2 || rec.events[0] != latmEventUpdate || rec.events[1] != latmEventFrame {
		t.Fatalf("events = %v, want [update frame]", rec.events)
	}
	if len(rec.ascs) != 1 || !bytes.Equal(rec.ascs[0], latmASC) {
		t.Errorf("OnCodecUpdate ASC = % x, want % x", rec.ascs, latmASC)
	}
}

// TestDeliverLATMOnCodecUpdateReuseDoesNotRefire covers useSameStreamMux
// reuse: feeding V5 after V4 must not fire OnCodecUpdate a second time,
// because the resolved config did not change.
func TestDeliverLATMOnCodecUpdateReuseDoesNotRefire(t *testing.T) {
	t.Parallel()
	tr := newInBandLATMTrack(t)
	var rec codecUpdateRecording

	pkt4 := rtp.Packet{Header: rtp.Header{Timestamp: 0, Marker: true}, Payload: latmV4}
	tr.deliverLATM(pkt4, rtp.Update{Timestamp: 0}, time.Unix(1, 0), rec.onFrame, rec.onCodecUpdate)

	pkt5 := rtp.Packet{Header: rtp.Header{Timestamp: 1024, Marker: true}, Payload: latmV5}
	tr.deliverLATM(pkt5, rtp.Update{Timestamp: 1024}, time.Unix(1, 0), rec.onFrame, rec.onCodecUpdate)

	updates := 0
	for _, e := range rec.events {
		if e == latmEventUpdate {
			updates++
		}
	}
	if updates != 1 {
		t.Errorf("OnCodecUpdate fired %d times across V4+V5, want 1", updates)
	}
}

// TestDeliverLATMOnCodecUpdateOutOfBandNeverFires covers the out-of-band
// suppression: newTrack seeds tr.latmASC from the config already known at
// Setup, so a track built out-of-band never fires OnCodecUpdate, matching the
// documented contract (the ASC was already on the Track from Describe).
func TestDeliverLATMOnCodecUpdateOutOfBandNeverFires(t *testing.T) {
	t.Parallel()
	desc := describedTrack{
		codec:     audiostream.CodecMP4ALATM{StreamMuxConfig: latmV1, MuxConfigPresent: false},
		clockRate: 44100,
		media:     audiostream.MediaAudio,
	}
	tr := newTrack(0, desc, SetupOptions{}, 1, nil)
	if tr.kind != deliverLATM {
		t.Fatalf("kind = %d, want deliverLATM", tr.kind)
	}
	tr.baseSet.Store(true)
	var rec codecUpdateRecording

	pkt := rtp.Packet{Header: rtp.Header{Timestamp: 0, Marker: true}, Payload: latmV2}
	tr.deliverLATM(pkt, rtp.Update{Timestamp: 0}, time.Unix(1, 0), rec.onFrame, rec.onCodecUpdate)
	// Deliver a second packet too, so the assertion covers steady-state
	// delivery, not just the first packet.
	tr.deliverLATM(pkt, rtp.Update{Timestamp: 0}, time.Unix(1, 0), rec.onFrame, rec.onCodecUpdate)

	for _, e := range rec.events {
		if e == latmEventUpdate {
			t.Fatalf("OnCodecUpdate fired for an out-of-band track: events = %v", rec.events)
		}
	}
}

// TestDeliverLATMNilOnCodecUpdateSafe covers the same in-band flow with a nil
// OnCodecUpdate: frames deliver normally and nothing panics.
func TestDeliverLATMNilOnCodecUpdateSafe(t *testing.T) {
	t.Parallel()
	tr := newInBandLATMTrack(t)
	var frames []audiostream.Frame

	pkt := rtp.Packet{Header: rtp.Header{Timestamp: 0, Marker: true}, Payload: latmV4}
	tr.deliverLATM(pkt, rtp.Update{Timestamp: 0}, time.Unix(1, 0), func(f audiostream.Frame) {
		frames = append(frames, copyFrame(&f))
	}, nil)

	if len(frames) != 1 {
		t.Fatalf("delivered %d frames, want 1", len(frames))
	}
	if want := []byte{0xAA, 0xBB, 0xCC}; !bytes.Equal(frames[0].Data, want) {
		t.Errorf("Data = % x, want % x", frames[0].Data, want)
	}
}

// TestResetDepacketizerClearsLATMState covers the SSRC-reset path: resetting
// a LATM track's depacketizer state also clears the reader-owned ASC
// snapshot, so a subsequent resolution re-announces via OnCodecUpdate.
func TestResetDepacketizerClearsLATMState(t *testing.T) {
	t.Parallel()
	tr := newInBandLATMTrack(t)
	var rec codecUpdateRecording

	pkt := rtp.Packet{Header: rtp.Header{Timestamp: 0, Marker: true}, Payload: latmV4}
	tr.deliverLATM(pkt, rtp.Update{Timestamp: 0}, time.Unix(1, 0), rec.onFrame, rec.onCodecUpdate)
	if tr.latmASC == nil {
		t.Fatal("latmASC not seeded after the first in-band resolution")
	}

	tr.resetDepacketizer()
	if tr.latmASC != nil {
		t.Errorf("latmASC = % x after resetDepacketizer, want nil", tr.latmASC)
	}
}

// TestResetDepacketizerLATMOutOfBandNeverRefires is the regression test for the
// bug resetDepacketizer previously had: it cleared tr.latmASC unconditionally
// on every SSRC reset, but latm.Depacketizer.Reset is a no-op for an
// out-of-band config (the ASC is fixed for the session, not learned from the
// stream), so the next packet's unchanged ASC compared against a just-cleared
// nil snapshot looked like a change and fired a spurious OnCodecUpdate with
// the wrong MuxConfigPresent:true. resetDepacketizer must re-seed tr.latmASC
// from whatever tr.latm.AudioSpecificConfig() reports right after Reset, so
// an out-of-band track's snapshot survives the reset unchanged and never
// refires, across any number of resets and deliveries.
func TestResetDepacketizerLATMOutOfBandNeverRefires(t *testing.T) {
	t.Parallel()
	desc := describedTrack{
		codec:     audiostream.CodecMP4ALATM{StreamMuxConfig: latmV1, MuxConfigPresent: false},
		clockRate: 44100,
		media:     audiostream.MediaAudio,
	}
	tr := newTrack(0, desc, SetupOptions{}, 1, nil)
	if tr.kind != deliverLATM {
		t.Fatalf("kind = %d, want deliverLATM", tr.kind)
	}
	tr.baseSet.Store(true)
	var rec codecUpdateRecording

	pkt := rtp.Packet{Header: rtp.Header{Timestamp: 0, Marker: true}, Payload: latmV2}
	tr.deliverLATM(pkt, rtp.Update{Timestamp: 0}, time.Unix(1, 0), rec.onFrame, rec.onCodecUpdate)

	tr.resetDepacketizer()
	if !bytes.Equal(tr.latmASC, latmASC) {
		t.Fatalf("latmASC = % x after resetDepacketizer, want the out-of-band ASC % x to survive", tr.latmASC, latmASC)
	}

	tr.deliverLATM(pkt, rtp.Update{Timestamp: 0}, time.Unix(1, 0), rec.onFrame, rec.onCodecUpdate)

	for _, e := range rec.events {
		if e == latmEventUpdate {
			t.Fatalf("OnCodecUpdate fired for an out-of-band track after an SSRC reset: events = %v", rec.events)
		}
	}
}
