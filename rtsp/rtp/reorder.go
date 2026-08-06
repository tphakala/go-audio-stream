package rtp

// MaxReorderWindow is the largest span, in sequence numbers, between the
// next packet to release and the newest buffered packet. A packet whose
// sequence sits more than MaxReorderWindow ahead of the release point forces
// the buffer to release its oldest slots (declaring the intervening gap) to
// make room. It bounds both memory and the resequencing latency.
const MaxReorderWindow = 128

// slot holds one buffered packet awaiting release, indexed by seq modulo
// MaxReorderWindow in the Reorderer's ring.
type slot struct {
	present bool
	seq     uint16
	payload []byte
}

// Reorderer resequences UDP RTP packets by 16-bit sequence number. It emits
// packets in strictly ascending sequence order, tolerating reordering within
// MaxReorderWindow and declaring loss when the window forces a release. It is
// pure and does no I/O. It is not safe for concurrent use; the UDP receive
// goroutine drives one Reorderer per track. The zero value is ready to use.
type Reorderer struct {
	initialized bool
	nextSeq     uint16 // next sequence number to release
	slots       [MaxReorderWindow]slot
	buffered    int
	late        uint64
	forced      uint64
}

// Released is one packet the Reorderer emits, in sequence order. Payload is a
// copy the Reorderer owns; the receive goroutine must have copied the
// datagram into the Reorderer, because the socket receive buffer is reused.
type Released struct {
	// Seq is the packet's 16-bit RTP sequence number.
	Seq uint16
	// Payload is the RTP packet bytes (header plus payload), owned by the
	// caller after release.
	Payload []byte
}

// ahead returns the signed forward distance from b to a on the 16-bit
// sequence number circle: positive when a is ahead of b, negative when a is
// behind b. It is wraparound-aware, so it stays correct across the 65535->0
// rollover as long as the true separation is within +-32767.
func ahead(a, b uint16) int {
	return int(int16(a - b))
}

// Push inserts one arrived packet, identified by its 16-bit sequence number,
// and returns every packet now releasable in ascending sequence order (into
// the caller-provided out slice, which is truncated and reused). payload must
// be a copy the Reorderer may retain. Push never blocks and never panics.
//
// The first Push initializes the release point at seq. A packet at or after
// the release point is buffered; buffering it may complete a run of
// consecutive sequence numbers, which is released. A packet whose sequence is
// more than MaxReorderWindow ahead of the release point first force-releases
// the oldest buffered slots (advancing the release point past the gap and
// counting the skipped sequence numbers as forced loss) until it fits, then
// buffers. A packet before the release point (already released or too late)
// is dropped and counted as late; it is not emitted.
func (r *Reorderer) Push(seq uint16, payload []byte, out []Released) []Released {
	out = out[:0]

	if !r.initialized {
		r.initialized = true
		r.nextSeq = seq
	}

	d := ahead(seq, r.nextSeq)
	if d < 0 {
		r.late++
		return out
	}

	if d >= MaxReorderWindow {
		for ahead(seq, r.nextSeq) >= MaxReorderWindow {
			idx := r.nextSeq % MaxReorderWindow
			if r.slots[idx].present {
				out = append(out, Released{Seq: r.slots[idx].seq, Payload: r.slots[idx].payload})
				r.slots[idx] = slot{}
				r.buffered--
			} else {
				r.forced++
			}
			r.nextSeq++
		}
	}

	idx := seq % MaxReorderWindow
	if r.slots[idx].present {
		// Same seq: a duplicate. A different seq at this index is
		// unreachable by construction (the force-release above always
		// advances the release point past any seq more than
		// MaxReorderWindow behind, so an in-window seq's slot is always
		// free of older occupants), but treating it identically here
		// means Push never corrupts state or panics even if that
		// invariant were ever violated.
		r.late++
		return out
	}
	r.slots[idx] = slot{present: true, seq: seq, payload: payload}
	r.buffered++

	for {
		idx := r.nextSeq % MaxReorderWindow
		if !r.slots[idx].present {
			break
		}
		out = append(out, Released{Seq: r.slots[idx].seq, Payload: r.slots[idx].payload})
		r.slots[idx] = slot{}
		r.buffered--
		r.nextSeq++
	}

	return out
}

// Flush releases every buffered packet in ascending sequence order (into out,
// truncated and reused) and resets the release point to just past the last
// released packet. The UDP receive goroutine calls it on an SSRC change,
// before resetting, so buffered packets from the old source are drained in
// order rather than discarded.
func (r *Reorderer) Flush(out []Released) []Released {
	out = out[:0]

	seq := r.nextSeq
	for i := 0; i < MaxReorderWindow && r.buffered > 0; i++ {
		idx := seq % MaxReorderWindow
		if r.slots[idx].present {
			out = append(out, Released{Seq: r.slots[idx].seq, Payload: r.slots[idx].payload})
			r.slots[idx] = slot{}
			r.buffered--
			r.nextSeq = seq + 1
		}
		seq++
	}

	return out
}

// Reset clears all buffered packets and returns the Reorderer to its
// uninitialized state, so the next Push re-establishes the release point. The
// receive goroutine calls it after Flush on an SSRC change.
func (r *Reorderer) Reset() {
	r.slots = [MaxReorderWindow]slot{}
	r.initialized = false
	r.nextSeq = 0
	r.buffered = 0
}

// ReorderStats is a snapshot of a Reorderer's cumulative counters.
type ReorderStats struct {
	// Late is the number of packets dropped for arriving before the release
	// point (duplicates or too-late reorders).
	Late uint64
	// Forced is the number of sequence numbers skipped by window-overflow
	// force-releases (a lower bound on genuinely lost packets).
	Forced uint64
	// Buffered is the number of packets currently held awaiting release.
	Buffered int
}

// Stats returns a snapshot of the Reorderer's counters.
func (r *Reorderer) Stats() ReorderStats {
	return ReorderStats{
		Late:     r.late,
		Forced:   r.forced,
		Buffered: r.buffered,
	}
}
