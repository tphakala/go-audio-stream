package udpsource

import (
	"bytes"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/depacket/flac"
	"github.com/tphakala/go-audio-stream/rtsp/rtp"
)

// An unfragmented FLAC frame is delivered whole, with the gap drained onto it.
func TestDeliverFLACUnfragmented(t *testing.T) {
	var got audiostream.Frame
	n := 0
	c := &Client{kind: kindFLAC, flac: flac.New(), cfg: Config{ClockRate: 48000, OnFrame: func(f audiostream.Frame) { got = f; n++ }}}
	payload := []byte{0xFF, 0xF8, 0x0A, 0x0B}
	c.deliverRTP(rtp.Packet{Header: rtp.Header{Timestamp: 480, Marker: true}, Payload: payload}, rtp.Update{Timestamp: 480, Gap: 2}, time.Unix(1, 0))
	if n != 1 {
		t.Fatalf("delivered %d frames, want 1", n)
	}
	if !bytes.Equal(got.Data, payload) {
		t.Errorf("Data = % x, want % x", got.Data, payload)
	}
	if got.SeqGap != 2 {
		t.Errorf("SeqGap = %d, want 2", got.SeqGap)
	}
}

// A frame fragmented across packets is delivered once, on the marker.
func TestDeliverFLACFragmented(t *testing.T) {
	n := 0
	var got audiostream.Frame
	c := &Client{kind: kindFLAC, flac: flac.New(), cfg: Config{ClockRate: 48000, OnFrame: func(f audiostream.Frame) { got = f; n++ }}}
	p0 := []byte{0xFF, 0xF8, 0x01}
	p1 := []byte{0x02, 0x03}
	p2 := []byte{0x04}
	c.deliverRTP(rtp.Packet{Header: rtp.Header{Timestamp: 96, Marker: false}, Payload: p0}, rtp.Update{Timestamp: 96}, time.Unix(1, 0))
	c.deliverRTP(rtp.Packet{Header: rtp.Header{Timestamp: 96, Marker: false}, Payload: p1}, rtp.Update{Timestamp: 96}, time.Unix(1, 0))
	if n != 0 {
		t.Fatalf("delivered %d frames before the marker, want 0", n)
	}
	c.deliverRTP(rtp.Packet{Header: rtp.Header{Timestamp: 96, Marker: true}, Payload: p2}, rtp.Update{Timestamp: 96}, time.Unix(1, 0))
	if n != 1 {
		t.Fatalf("delivered %d frames, want 1", n)
	}
	if want := bytes.Join([][]byte{p0, p1, p2}, nil); !bytes.Equal(got.Data, want) {
		t.Errorf("Data = % x, want % x", got.Data, want)
	}
}

// An empty payload is counted malformed, delivers no frame, and retains the gap
// for the next delivered frame.
func TestDeliverFLACMalformedRetainsGap(t *testing.T) {
	var got audiostream.Frame
	n := 0
	c := &Client{kind: kindFLAC, flac: flac.New(), cfg: Config{ClockRate: 48000, OnFrame: func(f audiostream.Frame) { got = f; n++ }}}
	c.deliverRTP(rtp.Packet{Header: rtp.Header{Marker: true}, Payload: nil}, rtp.Update{Gap: 2}, time.Unix(1, 0)) // malformed, retains gap 2
	if n != 0 || c.malformed.Load() != 1 {
		t.Fatalf("n=%d malformed=%d, want 0 and 1", n, c.malformed.Load())
	}
	c.deliverRTP(rtp.Packet{Header: rtp.Header{Timestamp: 480, Marker: true}, Payload: []byte{0xFF, 0xF8}}, rtp.Update{Timestamp: 480, Gap: 0}, time.Unix(1, 0))
	if n != 1 {
		t.Fatalf("delivered %d frames, want 1", n)
	}
	if got.SeqGap != 2 {
		t.Errorf("SeqGap = %d, want 2 (gap stranded on the malformed packet must surface here)", got.SeqGap)
	}
}

// A continuation fragment carrying a different RTP timestamp than the fragment
// that started the frame is counted malformed and delivers no frame; the partial
// reassembly is dropped so two frames' bytes are never spliced.
func TestDeliverFLACTimestampMismatchDropsPartial(t *testing.T) {
	n := 0
	c := &Client{kind: kindFLAC, flac: flac.New(), cfg: Config{ClockRate: 48000, OnFrame: func(audiostream.Frame) { n++ }}}
	c.deliverRTP(rtp.Packet{Header: rtp.Header{Timestamp: 100, Marker: false}, Payload: []byte{0x01, 0x02}}, rtp.Update{Timestamp: 100}, time.Unix(1, 0)) // buffering
	c.deliverRTP(rtp.Packet{Header: rtp.Header{Timestamp: 200, Marker: false}, Payload: []byte{0x03}}, rtp.Update{Timestamp: 200}, time.Unix(1, 0))       // mismatch
	if n != 0 {
		t.Fatalf("delivered %d frames on a timestamp mismatch, want 0", n)
	}
	if c.malformed.Load() != 1 {
		t.Errorf("malformed = %d, want 1", c.malformed.Load())
	}
}

// With a nil OnFrame, deliverFLAC still drains the pending gap onto a completed
// frame so the counter cannot grow unbounded across a nil-callback stream.
func TestDeliverFLACNilOnFrameDrains(t *testing.T) {
	c := &Client{kind: kindFLAC, flac: flac.New(), cfg: Config{ClockRate: 48000}} // nil OnFrame
	// A buffering fragment carrying a gap retains it (no frame completed).
	c.deliverRTP(rtp.Packet{Header: rtp.Header{Marker: false}, Payload: []byte{0x01}}, rtp.Update{Gap: 3}, time.Unix(1, 0))
	if c.pendingGap != 3 {
		t.Fatalf("pendingGap = %d after a buffering fragment, want 3 (retained)", c.pendingGap)
	}
	// Completing the frame drains the gap even though OnFrame is nil.
	c.deliverRTP(rtp.Packet{Header: rtp.Header{Marker: true}, Payload: []byte{0x02}}, rtp.Update{Gap: 0}, time.Unix(1, 0))
	if c.pendingGap != 0 {
		t.Errorf("pendingGap = %d after a completed frame under a nil callback, want 0 (drained)", c.pendingGap)
	}
}

// resetReassembly drops a partial FLAC frame, so a lost final fragment cannot be
// spliced onto the next frame.
func TestFLACResetReassemblyDropsPartial(t *testing.T) {
	n := 0
	var got audiostream.Frame
	c := &Client{kind: kindFLAC, flac: flac.New(), cfg: Config{ClockRate: 48000, OnFrame: func(f audiostream.Frame) { got = f; n++ }}}
	c.deliverRTP(rtp.Packet{Header: rtp.Header{Marker: false}, Payload: []byte{0x01, 0x02}}, rtp.Update{}, time.Unix(1, 0)) // buffering
	c.resetReassembly()                                                                                                     // reader drops partial on a discontinuity
	tail := []byte{0xEE, 0xFF}
	c.deliverRTP(rtp.Packet{Header: rtp.Header{Marker: true}, Payload: tail}, rtp.Update{Gap: 1}, time.Unix(1, 0))
	if n != 1 {
		t.Fatalf("delivered %d frames, want 1", n)
	}
	if !bytes.Equal(got.Data, tail) {
		t.Errorf("Data = % x, want % x (partial reassembly not dropped)", got.Data, tail)
	}
}
