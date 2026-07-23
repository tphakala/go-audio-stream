package rtp

// Stream maintains per-track RTP reception state: sequence continuity
// with wraparound, 64-bit timestamp unwrap, and SSRC-change detection.
// It is not safe for concurrent use; the RTSP client drives it from its
// single reader goroutine. The zero value is ready to use.
type Stream struct {
	init    bool
	ssrc    uint32
	lastSeq uint16
	cycles  uint16
	lastTS  uint32
	ts64    uint64

	received   uint64
	seqGaps    uint64
	duplicates uint64
	ssrcResets uint64
}

// Update reports what one observed packet implies for a Stream.
type Update struct {
	// Gap is the number of packets lost immediately before this packet:
	// 0 for the first packet, an in-order successor, or a duplicate or
	// reordered packet; N when N sequence numbers were skipped.
	Gap int
	// Timestamp is the 64-bit unwrapped RTP timestamp.
	Timestamp uint64
	// SSRCReset is true when this packet began a new SSRC and the Stream
	// reset its sequence and timestamp state.
	SSRCReset bool
	// Duplicate is true when the sequence number did not advance (a
	// duplicate or an old reordered packet). The caller may still use the
	// packet; over in-order TCP transport this is rare.
	Duplicate bool
}

// Observe folds one parsed header into the Stream and reports the loss,
// timing, and SSRC implications. It updates the Stream's cumulative
// counters.
func (s *Stream) Observe(h Header) Update {
	s.received++

	if !s.init {
		s.init = true
		s.ssrc = h.SSRC
		s.lastSeq = h.SequenceNumber
		s.lastTS = h.Timestamp
		s.ts64 = uint64(h.Timestamp)
		return Update{Timestamp: s.ts64}
	}

	if h.SSRC != s.ssrc {
		s.ssrc = h.SSRC
		s.lastSeq = h.SequenceNumber
		s.cycles = 0
		s.lastTS = h.Timestamp
		s.ts64 = uint64(h.Timestamp)
		s.ssrcResets++
		return Update{Timestamp: s.ts64, SSRCReset: true}
	}

	d := h.SequenceNumber - s.lastSeq // uint16, wraps
	gap := 0
	dup := false
	switch {
	case d == 0:
		// Same sequence number: a duplicate.
		dup = true
	case d < 0x8000:
		// Forward within half the sequence space: the in-order case.
		gap = int(d) - 1
		if h.SequenceNumber < s.lastSeq {
			s.cycles++
		}
		s.lastSeq = h.SequenceNumber
	default:
		// d >= 0x8000: a backward step, an old or reordered packet. Over
		// in-order TCP transport this is a genuine duplicate or a rare
		// reorder, so it does not advance the sequence view and is never
		// counted as tens of thousands of lost packets.
		dup = true
	}

	s.seqGaps += uint64(gap)
	ts := s.extendTS(h.Timestamp)
	if dup {
		s.duplicates++
	} else {
		s.ts64 = ts
		s.lastTS = h.Timestamp
	}
	return Update{Gap: gap, Timestamp: ts, Duplicate: dup}
}

// extendTS unwraps the raw 32-bit RTP timestamp to 64 bits relative to the
// Stream's last accepted timestamp, handling the 32-bit wrap and small
// backward steps from reordered or duplicate packets.
func (s *Stream) extendTS(ts uint32) uint64 {
	d := ts - s.lastTS // uint32, wraps
	if d < 0x80000000 {
		return s.ts64 + uint64(d) // forward, including 32-bit wrap
	}
	return s.ts64 - uint64(-d) // small backward step
}

// StreamStats is a snapshot of a Stream's cumulative counters.
type StreamStats struct {
	// Received is the number of headers observed.
	Received uint64
	// SeqGaps is the cumulative number of packets lost.
	SeqGaps uint64
	// Duplicates is the number of duplicate or reordered packets seen.
	Duplicates uint64
	// SSRCResets is the number of SSRC changes tolerated.
	SSRCResets uint64
	// ExtendedHighestSeq is (cycles << 16) | highest sequence number, the
	// extended highest sequence number an RTCP Receiver Report reports.
	ExtendedHighestSeq uint32
}

// Stats returns a snapshot of the Stream's counters.
func (s *Stream) Stats() StreamStats {
	return StreamStats{
		Received:           s.received,
		SeqGaps:            s.seqGaps,
		Duplicates:         s.duplicates,
		SSRCResets:         s.ssrcResets,
		ExtendedHighestSeq: uint32(s.cycles)<<16 | uint32(s.lastSeq),
	}
}
