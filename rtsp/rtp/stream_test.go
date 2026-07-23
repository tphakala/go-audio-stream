package rtp_test

import (
	"testing"

	"github.com/tphakala/go-audio-stream/rtsp/rtp"
)

func hdr(seq uint16, ts, ssrc uint32) rtp.Header {
	return rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts, SSRC: ssrc}
}

func TestStreamInOrder(t *testing.T) {
	t.Parallel()
	var s rtp.Stream
	if u := s.Observe(hdr(100, 1000, 1)); u.Gap != 0 || u.SSRCReset || u.Timestamp != 1000 {
		t.Errorf("first = %+v", u)
	}
	if u := s.Observe(hdr(101, 2000, 1)); u.Gap != 0 || u.Timestamp != 2000 {
		t.Errorf("second = %+v", u)
	}
	// Two packets skipped (102, 103): gap of 2.
	if u := s.Observe(hdr(104, 5000, 1)); u.Gap != 2 {
		t.Errorf("gap = %d, want 2", u.Gap)
	}
	if st := s.Stats(); st.SeqGaps != 2 || st.Received != 3 {
		t.Errorf("stats = %+v", st)
	}
}

func TestStreamSeqWrap(t *testing.T) {
	t.Parallel()
	var s rtp.Stream
	s.Observe(hdr(65535, 0, 7))
	u := s.Observe(hdr(0, 160, 7)) // wraps 65535 -> 0, in order
	if u.Gap != 0 || u.Duplicate {
		t.Errorf("wrap = %+v, want gap 0, not duplicate", u)
	}
	if st := s.Stats(); (st.ExtendedHighestSeq>>16) != 1 || uint16(st.ExtendedHighestSeq) != 0 {
		t.Errorf("extended highest seq = %#x, want cycle 1 seq 0", st.ExtendedHighestSeq)
	}
}

func TestStreamTimestampUnwrap(t *testing.T) {
	t.Parallel()
	var s rtp.Stream
	s.Observe(hdr(1, 0xFFFFFF00, 7))
	u := s.Observe(hdr(2, 0x00000100, 7)) // +0x200 across the 32-bit wrap
	if u.Timestamp != 0x100000100 {
		t.Errorf("unwrapped ts = %#x, want 0x100000100", u.Timestamp)
	}
}

func TestStreamDuplicate(t *testing.T) {
	t.Parallel()
	var s rtp.Stream
	s.Observe(hdr(10, 100, 7))
	s.Observe(hdr(11, 200, 7))
	if u := s.Observe(hdr(11, 200, 7)); !u.Duplicate || u.Gap != 0 {
		t.Errorf("duplicate = %+v", u)
	}
	if st := s.Stats(); st.Duplicates != 1 {
		t.Errorf("duplicates = %d, want 1", st.Duplicates)
	}
}

func TestStreamSSRCReset(t *testing.T) {
	t.Parallel()
	var s rtp.Stream
	s.Observe(hdr(100, 1000, 1))
	u := s.Observe(hdr(5, 50, 2)) // new SSRC
	if !u.SSRCReset || u.Gap != 0 || u.Timestamp != 50 {
		t.Errorf("ssrc reset = %+v", u)
	}
	if st := s.Stats(); st.SSRCResets != 1 {
		t.Errorf("ssrc resets = %d, want 1", st.SSRCResets)
	}
}

func TestObserveBackwardTimestampBeforeStreamStartClampsToZero(t *testing.T) {
	t.Parallel()
	// The running 64-bit timestamp starts at the first packet's raw
	// 32-bit value, so a stream that begins just after a timestamp wrap
	// can receive a duplicate from just before it. That step reaches
	// back further than everything accumulated so far, and an unsigned
	// subtraction would report it as a timestamp near 2^64 instead of
	// one near the start of the stream.
	var s rtp.Stream

	first := s.Observe(rtp.Header{SSRC: 1, SequenceNumber: 100, Timestamp: 100})
	if first.Timestamp != 100 {
		t.Fatalf("first Timestamp = %d, want 100", first.Timestamp)
	}

	// 396 ticks before the first packet, having wrapped past zero.
	const beforeWrap = uint32(4294966900)
	got := s.Observe(rtp.Header{SSRC: 1, SequenceNumber: 99, Timestamp: beforeWrap})

	if !got.Duplicate {
		t.Errorf("Duplicate = false, want true for a backward sequence step")
	}
	if got.Timestamp != 0 {
		t.Errorf("Timestamp = %d, want 0 (clamped, not an unsigned underflow)", got.Timestamp)
	}
	if got.Timestamp > 1<<32 {
		t.Errorf("Timestamp = %d underflowed past the 32-bit range", got.Timestamp)
	}
}
