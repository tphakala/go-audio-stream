package rtsp

import (
	"bytes"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/depacket/flac"
	"github.com/tphakala/go-audio-stream/rtsp/rtp"
)

func newFLACTrack() *track {
	tr := &track{id: 3, kind: deliverFLAC, clockRate: 48000, flac: flac.New()}
	tr.baseSet.Store(true)
	return tr
}

// An unfragmented FLAC frame (marker set) is delivered as one frame carrying the
// payload, with the gap drained onto it.
func TestDeliverFLACUnfragmented(t *testing.T) {
	t.Parallel()
	tr := newFLACTrack()
	payload := []byte{0xFF, 0xF8, 0x01, 0x02, 0x03}
	pkt := rtp.Packet{Header: rtp.Header{Timestamp: 480, Marker: true}, Payload: payload}

	var got audiostream.Frame
	n := 0
	tr.deliver(pkt, rtp.Update{Timestamp: 480, Gap: 2}, time.Unix(1, 0), func(f audiostream.Frame) { got = copyFrame(&f); n++ })
	if n != 1 {
		t.Fatalf("delivered %d frames, want 1", n)
	}
	if !bytes.Equal(got.Data, payload) {
		t.Errorf("Data = % x, want % x", got.Data, payload)
	}
	if got.RTPTime != 480 {
		t.Errorf("RTPTime = %d, want 480", got.RTPTime)
	}
	if got.PTS != 10*time.Millisecond { // 480 / 48000
		t.Errorf("PTS = %v, want 10ms", got.PTS)
	}
	if got.SeqGap != 2 {
		t.Errorf("SeqGap = %d, want 2", got.SeqGap)
	}
}

// A frame fragmented across three packets is delivered once, on the marker, as
// the concatenation. The buffering packets deliver nothing.
func TestDeliverFLACFragmented(t *testing.T) {
	t.Parallel()
	tr := newFLACTrack()
	p0 := []byte{0xFF, 0xF8, 0x11}
	p1 := []byte{0x22, 0x33}
	p2 := []byte{0x44}

	n := 0
	var got audiostream.Frame
	onFrame := func(f audiostream.Frame) { got = copyFrame(&f); n++ }

	tr.deliver(rtp.Packet{Header: rtp.Header{Timestamp: 96, Marker: false}, Payload: p0}, rtp.Update{Timestamp: 96}, time.Unix(1, 0), onFrame)
	tr.deliver(rtp.Packet{Header: rtp.Header{Timestamp: 96, Marker: false}, Payload: p1}, rtp.Update{Timestamp: 96}, time.Unix(1, 0), onFrame)
	if n != 0 {
		t.Fatalf("delivered %d frames before the marker, want 0", n)
	}
	tr.deliver(rtp.Packet{Header: rtp.Header{Timestamp: 96, Marker: true}, Payload: p2}, rtp.Update{Timestamp: 96}, time.Unix(1, 0), onFrame)
	if n != 1 {
		t.Fatalf("delivered %d frames, want 1", n)
	}
	want := bytes.Join([][]byte{p0, p1, p2}, nil)
	if !bytes.Equal(got.Data, want) {
		t.Errorf("Data = % x, want % x", got.Data, want)
	}
}

// A sequence gap between fragments drops the partial reassembly (via
// resetDepacketizer, as the reader drives on a gap), so a lost final fragment
// cannot be spliced onto the next frame.
func TestDeliverFLACGapDropsPartialReassembly(t *testing.T) {
	t.Parallel()
	tr := newFLACTrack()
	// Buffer a fragment, then simulate the reader's gap handling.
	tr.deliver(rtp.Packet{Header: rtp.Header{Marker: false}, Payload: []byte{0x01, 0x02}}, rtp.Update{}, time.Unix(1, 0), func(audiostream.Frame) {})
	tr.resetDepacketizer(false) // a plain gap resets FLAC, exactly like AAC

	tail := []byte{0xAB, 0xCD}
	var got audiostream.Frame
	n := 0
	tr.deliver(rtp.Packet{Header: rtp.Header{Marker: true}, Payload: tail}, rtp.Update{Gap: 1}, time.Unix(1, 0), func(f audiostream.Frame) { got = copyFrame(&f); n++ })
	if n != 1 {
		t.Fatalf("delivered %d frames, want 1", n)
	}
	if !bytes.Equal(got.Data, tail) {
		t.Errorf("Data = % x, want % x (partial reassembly not dropped on the gap)", got.Data, tail)
	}
}

// A continuation fragment carrying a different RTP timestamp than the fragment
// that started the frame is counted malformed and delivers no frame, and the
// partial reassembly is dropped so two frames' bytes are never spliced.
func TestDeliverFLACTimestampMismatchDropsPartial(t *testing.T) {
	t.Parallel()
	tr := newFLACTrack()
	tr.deliver(rtp.Packet{Header: rtp.Header{Timestamp: 100, Marker: false}, Payload: []byte{0x01, 0x02}}, rtp.Update{Timestamp: 100}, time.Unix(1, 0), func(audiostream.Frame) {
		t.Fatal("a buffering fragment must deliver no frame")
	})
	n := 0
	tr.deliver(rtp.Packet{Header: rtp.Header{Timestamp: 200, Marker: false}, Payload: []byte{0x03}}, rtp.Update{Timestamp: 200}, time.Unix(1, 0), func(audiostream.Frame) { n++ })
	if n != 0 {
		t.Fatalf("delivered %d frames on a timestamp mismatch, want 0", n)
	}
	if tr.malformed.Load() != 1 {
		t.Errorf("malformed = %d, want 1", tr.malformed.Load())
	}
	// After the mismatch reset, a fresh frame reassembles cleanly.
	var got audiostream.Frame
	tr.deliver(rtp.Packet{Header: rtp.Header{Timestamp: 300, Marker: true}, Payload: []byte{0xAB}}, rtp.Update{Timestamp: 300}, time.Unix(1, 0), func(f audiostream.Frame) { got = copyFrame(&f); n++ })
	if n != 1 || !bytes.Equal(got.Data, []byte{0xAB}) {
		t.Errorf("after mismatch: n=%d data=% x, want one frame AB", n, got.Data)
	}
}

// An empty FLAC payload is counted malformed and delivers no frame.
func TestDeliverFLACEmptyPayloadMalformed(t *testing.T) {
	t.Parallel()
	tr := newFLACTrack()
	n := 0
	tr.deliver(rtp.Packet{Header: rtp.Header{Marker: true}, Payload: nil}, rtp.Update{}, time.Unix(1, 0), func(audiostream.Frame) { n++ })
	if n != 0 {
		t.Errorf("delivered %d frames for an empty payload, want 0", n)
	}
	if tr.malformed.Load() != 1 {
		t.Errorf("malformed = %d, want 1", tr.malformed.Load())
	}
}

// A gap that lands on a buffering fragment (which delivers no frame) is retained
// and surfaces on the next completed frame, not lost.
func TestDeliverFLACGapStrandedOnBufferingFragment(t *testing.T) {
	t.Parallel()
	tr := newFLACTrack()
	// A buffering fragment carrying a gap: no frame, gap retained.
	tr.deliver(rtp.Packet{Header: rtp.Header{Marker: false}, Payload: []byte{0x01}}, rtp.Update{Gap: 3}, time.Unix(1, 0), func(audiostream.Frame) {
		t.Fatal("a buffering fragment must deliver no frame")
	})
	var got audiostream.Frame
	n := 0
	tr.deliver(rtp.Packet{Header: rtp.Header{Marker: true}, Payload: []byte{0x02}}, rtp.Update{Gap: 0}, time.Unix(1, 0), func(f audiostream.Frame) { got = copyFrame(&f); n++ })
	if n != 1 {
		t.Fatalf("delivered %d frames, want 1", n)
	}
	if got.SeqGap != 3 {
		t.Errorf("SeqGap = %d, want 3 (gap stranded on the buffering fragment must surface here)", got.SeqGap)
	}
}
