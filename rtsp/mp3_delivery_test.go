package rtsp

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/depacket/mp3"
	"github.com/tphakala/go-audio-stream/rtsp/rtp"
)

// mp3TrackClock is the MPA RTP clock (90 kHz, RFC 2250).
const mp3TrackClock = 90000

func newMP3Track() *track {
	tr := &track{id: 5, kind: deliverMP3, clockRate: mp3TrackClock, mp3: mp3.New(mp3TrackClock)}
	tr.baseSet.Store(true)
	return tr
}

// mp3Frame builds a whole MPEG-1 Layer III frame (128 kbps, 44100 Hz, stereo, no
// padding): a 417-byte frame the depacketizer frames by its header. The body is
// filler; only the 4-byte header matters to the framer.
func mp3Frame() []byte {
	const frameLen = 417
	b := make([]byte, frameLen)
	copy(b, []byte{0xFF, 0xFB, 0x90, 0x00})
	for i := 4; i < len(b); i++ {
		b[i] = byte(i)
	}
	return b
}

// mpaRTPPayload prepends the 4-byte RFC 2250 MPEG audio-specific header (MBZ +
// fragmentation offset) to data.
func mpaRTPPayload(fragOffset uint16, data []byte) []byte {
	p := make([]byte, 4+len(data))
	binary.BigEndian.PutUint16(p[2:4], fragOffset)
	copy(p[4:], data)
	return p
}

// A single whole MP3 frame in one packet is delivered as one frame, with the
// RFC 2250 header stripped, the packet timestamp as RTPTime, and the gap drained.
func TestDeliverMP3SingleFrame(t *testing.T) {
	t.Parallel()
	tr := newMP3Track()
	f := mp3Frame()
	pkt := rtp.Packet{Header: rtp.Header{Timestamp: 90000}, Payload: mpaRTPPayload(0, f)}

	var got audiostream.Frame
	n := 0
	tr.deliver(pkt, rtp.Update{Timestamp: 90000, Gap: 2}, time.Unix(1, 0), func(fr audiostream.Frame) { got = copyFrame(&fr); n++ })
	if n != 1 {
		t.Fatalf("delivered %d frames, want 1", n)
	}
	if !bytes.Equal(got.Data, f) {
		t.Errorf("Data mismatch: got %d bytes, want %d", len(got.Data), len(f))
	}
	if got.RTPTime != 90000 {
		t.Errorf("RTPTime = %d, want 90000", got.RTPTime)
	}
	if got.PTS != time.Second { // 90000 ticks / 90000 Hz
		t.Errorf("PTS = %v, want 1s", got.PTS)
	}
	if got.SeqGap != 2 {
		t.Errorf("SeqGap = %d, want 2", got.SeqGap)
	}
}

// A packet aggregating several whole frames delivers them all: the gap is drained
// onto the first only, and each later frame's PTS advances by its RTP offset.
func TestDeliverMP3Aggregated(t *testing.T) {
	t.Parallel()
	tr := newMP3Track()
	f := mp3Frame()
	pkt := rtp.Packet{Header: rtp.Header{Timestamp: 0}, Payload: mpaRTPPayload(0, bytes.Join([][]byte{f, f}, nil))}

	var frames []audiostream.Frame
	tr.deliver(pkt, rtp.Update{Timestamp: 0, Gap: 4}, time.Unix(1, 0), func(fr audiostream.Frame) { frames = append(frames, copyFrame(&fr)) })
	if len(frames) != 2 {
		t.Fatalf("delivered %d frames, want 2", len(frames))
	}
	if frames[0].SeqGap != 4 || frames[1].SeqGap != 0 {
		t.Errorf("SeqGap = %d,%d, want 4,0", frames[0].SeqGap, frames[1].SeqGap)
	}
	// Second frame offset = 1152 * 90000 / 44100 = 2351 ticks.
	if want := tr.ptsOf(2351); frames[1].PTS != want {
		t.Errorf("second frame PTS = %v, want %v", frames[1].PTS, want)
	}
	if frames[0].PTS != 0 {
		t.Errorf("first frame PTS = %v, want 0", frames[0].PTS)
	}
}

// A frame fragmented across packets delivers once, when the final fragment
// completes it; the buffering packets deliver nothing and a gap stranded on a
// buffering fragment surfaces on the completed frame.
func TestDeliverMP3Fragmented(t *testing.T) {
	t.Parallel()
	tr := newMP3Track()
	f := mp3Frame()
	const split = 200

	n := 0
	var got audiostream.Frame
	onFrame := func(fr audiostream.Frame) { got = copyFrame(&fr); n++ }

	tr.deliver(rtp.Packet{Header: rtp.Header{Timestamp: 0}, Payload: mpaRTPPayload(0, f[:split])}, rtp.Update{Timestamp: 0, Gap: 3}, time.Unix(1, 0), onFrame)
	if n != 0 {
		t.Fatalf("buffering fragment delivered %d frames, want 0", n)
	}
	tr.deliver(rtp.Packet{Header: rtp.Header{Timestamp: 0}, Payload: mpaRTPPayload(split, f[split:])}, rtp.Update{Timestamp: 0}, time.Unix(1, 0), onFrame)
	if n != 1 {
		t.Fatalf("delivered %d frames, want 1", n)
	}
	if !bytes.Equal(got.Data, f) {
		t.Errorf("reassembled frame mismatch: got %d bytes, want %d", len(got.Data), len(f))
	}
	if got.SeqGap != 3 {
		t.Errorf("SeqGap = %d, want 3 (gap stranded on the buffering fragment must surface here)", got.SeqGap)
	}
}

// A sequence gap between fragments drops the partial reassembly (via
// resetDepacketizer, as the reader drives on a gap), so a lost fragment cannot be
// spliced onto the next frame.
func TestDeliverMP3GapDropsPartialReassembly(t *testing.T) {
	t.Parallel()
	tr := newMP3Track()
	f := mp3Frame()
	// Buffer a first fragment, then simulate the reader's gap handling.
	tr.deliver(rtp.Packet{Header: rtp.Header{Timestamp: 0}, Payload: mpaRTPPayload(0, f[:200])}, rtp.Update{Timestamp: 0}, time.Unix(1, 0), func(audiostream.Frame) {
		t.Fatal("a buffering fragment must deliver no frame")
	})
	tr.resetDepacketizer(false) // a plain gap resets MP3, exactly like FLAC

	// The continuation that would have completed the dropped partial is now an
	// orphan: it is counted malformed and delivers no frame.
	n := 0
	tr.deliver(rtp.Packet{Header: rtp.Header{Timestamp: 0}, Payload: mpaRTPPayload(200, f[200:])}, rtp.Update{Gap: 1}, time.Unix(1, 0), func(audiostream.Frame) { n++ })
	if n != 0 {
		t.Fatalf("delivered %d frames after the partial was dropped, want 0", n)
	}
	if tr.malformed.Load() != 1 {
		t.Errorf("malformed = %d, want 1 (orphaned continuation)", tr.malformed.Load())
	}
}

// A short RTP payload (too short for the 4-byte RFC 2250 header) is counted
// malformed and delivers no frame.
func TestDeliverMP3ShortPayloadMalformed(t *testing.T) {
	t.Parallel()
	tr := newMP3Track()
	n := 0
	tr.deliver(rtp.Packet{Header: rtp.Header{Timestamp: 0}, Payload: []byte{0x00, 0x00}}, rtp.Update{}, time.Unix(1, 0), func(audiostream.Frame) { n++ })
	if n != 0 {
		t.Errorf("delivered %d frames for a short payload, want 0", n)
	}
	if tr.malformed.Load() != 1 {
		t.Errorf("malformed = %d, want 1", tr.malformed.Load())
	}
}
