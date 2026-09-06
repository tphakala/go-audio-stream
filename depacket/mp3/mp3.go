package mp3

import (
	"encoding/binary"
	"errors"

	mpahdr "github.com/tphakala/go-audio-stream/internal/mp3"
)

// headerLen is the size in bytes of the RFC 2250 section 3.5 MPEG audio-specific
// header (16-bit MBZ + 16-bit fragmentation offset) that precedes the audio
// bytes in every MPA RTP payload.
const headerLen = 4

// defaultClockRate is the RTP clock rate for MPA (RFC 2250, RFC 3551 section
// 4.5.13): always 90 kHz, independent of the audio sampling rate. New falls back
// to it when the caller passes 0.
const defaultClockRate = 90000

// maxRTPOffset caps an aggregated frame's RTPOffset at the 32-bit RTP clock
// range. A conformant MPA clock (90 kHz) keeps a packet's accumulated offset far
// below this (tens of millions of ticks at most), but the SDP clock is remote
// input bounded only by MaxUint32, so a pathological rtpmap clock plus a
// maximally aggregated packet could push the uint64 accumulator past 2^32.
// Saturating rather than wrapping the cast keeps the per-frame offsets
// non-decreasing (a wrap would hand a later frame a SMALLER offset and a
// backwards PTS); a real stream never reaches the cap.
const maxRTPOffset uint64 = 1<<32 - 1

// Sentinel errors. Depacketize returns one of these (never any other error
// value) and never panics.
var (
	// ErrShortHeader is returned when the RTP payload is shorter than the 4-byte
	// MPEG audio-specific header, so the fragmentation offset cannot be read. An
	// empty payload falls here too. If it arrives mid-reassembly the partial frame
	// is discarded.
	ErrShortHeader = errors.New("mp3: RTP payload shorter than the 4-byte MPEG audio header")
	// ErrInvalidFrame is returned when a packet on a frame boundary
	// (fragmentation offset 0) does not begin with a valid MPEG audio frame
	// header, so no frame can be framed from it.
	ErrInvalidFrame = errors.New("mp3: payload does not begin with a valid MPEG audio frame header")
	// ErrOrphanFragment is returned for a continuation fragment (fragmentation
	// offset > 0) that cannot be attached to a frame in progress: no frame is
	// being reassembled (the head fragment was lost), the timestamp does not match
	// the frame being reassembled, or the offset does not continue it. A
	// continuation carries no frame header, so it cannot start a new frame and is
	// dropped.
	ErrOrphanFragment = errors.New("mp3: continuation fragment with no matching frame in progress")
	// ErrFrameOverflow is returned when a continuation fragment would push the
	// reassembled frame past the length its header declared. The partial
	// reassembly is discarded.
	ErrFrameOverflow = errors.New("mp3: fragment reassembly exceeded the frame length")
)

// Frame is one depacketized MPEG audio frame and its presentation offset within
// the packet.
type Frame struct {
	// Data is the whole MPEG audio frame, header included. It aliases either the
	// input payload (a frame carried whole in one packet) or the depacketizer's
	// reassembly buffer (a frame reassembled from fragments), and is valid only
	// until the next call to Depacketize or Reset. Copy it to retain it.
	Data []byte
	// RTPOffset is this frame's RTP timestamp offset from the packet timestamp, in
	// the RTP clock (90 kHz for MPA). It is 0 for the first or only frame of a
	// packet and for a reassembled fragmented frame; a packet aggregating several
	// whole frames spaces the later frames by their durations scaled to the clock.
	RTPOffset uint32
}

// Depacketizer reassembles MPEG audio frames from RTP payloads. It carries the
// cross-packet reassembly buffer for a fragmented frame, so one instance serves
// one RTP stream and is not safe for concurrent use.
type Depacketizer struct {
	// clockRate is the RTP clock rate in Hz, used to scale an aggregated frame's
	// sample offset to RTP ticks. 90 kHz for a conformant MPA stream.
	clockRate uint32
	// frames is the reused return slice, so a steady one-frame-per-packet stream
	// allocates nothing on the delivery path.
	frames []Frame
	// frag holds the bytes of a frame being reassembled across packets; fragActive
	// says whether one is in progress. fragTime is the RTP timestamp every
	// fragment of that frame shares, and expectedLen is the whole frame length its
	// header declared. All are meaningful only while fragActive is true.
	frag        []byte
	fragActive  bool
	fragTime    uint32
	expectedLen int
}

// New returns a ready Depacketizer for a stream whose RTP clock runs at
// clockRate Hz. A clockRate of 0 defaults to the RFC 2250 MPA clock of 90 kHz.
func New(clockRate uint32) *Depacketizer {
	if clockRate == 0 {
		clockRate = defaultClockRate
	}
	return &Depacketizer{clockRate: clockRate}
}

// Depacketize processes one RTP payload and returns the MPEG audio frames it
// completes: the whole frame(s) of a frame-boundary packet, the one frame a final
// fragment completes, or none while a fragmented frame is still being reassembled
// or when the packet is dropped.
//
// rtpTime is the packet's RTP timestamp. All fragments of one frame share it, so
// a continuation whose timestamp differs from the frame being reassembled means a
// fragment was lost; the stale partial is dropped and the continuation, which
// carries no header, is dropped with it (ErrOrphanFragment).
//
// The returned frames alias the input payload (whole-frame path) or the
// reassembly buffer (reassembled path) and are valid only until the next call to
// Depacketize or Reset. On any error the return slice is empty; a packet that
// only buffers a fragment returns an empty slice and a nil error. The MBZ field
// is not validated and the marker bit is not consulted, matching how senders and
// other depacketizers treat MPA.
func (d *Depacketizer) Depacketize(payload []byte, rtpTime uint32) ([]Frame, error) {
	if len(payload) < headerLen {
		// A malformed packet orphans any frame in progress: drop it so a later
		// fragment cannot splice onto a partial the loss broke.
		if d.fragActive {
			d.Reset()
		}
		return d.frames[:0], ErrShortHeader
	}
	// payload[0:2] is MBZ, ignored. payload[2:4] is the fragmentation offset.
	fragOffset := binary.BigEndian.Uint16(payload[2:4])
	data := payload[headerLen:]

	if fragOffset == 0 {
		return d.startBoundary(data, rtpTime)
	}
	return d.continueFragment(data, uint32(fragOffset), rtpTime)
}

// startBoundary handles a packet whose fragmentation offset is 0: a frame
// boundary carrying one or more whole frames, or the first fragment of a frame
// split across packets. Any partial in progress lost its final fragment (a new
// boundary arrived before the old frame completed) and is dropped first.
func (d *Depacketizer) startBoundary(data []byte, rtpTime uint32) ([]Frame, error) {
	if d.fragActive {
		d.Reset()
	}
	frames := d.frames[:0]
	off := 0
	var accTicks uint64 // RTP ticks of the frames already emitted from this packet
	for {
		rem := data[off:]
		if len(rem) < mpahdr.HeaderLen {
			if off == 0 {
				// Too short to even hold a frame header: nothing framable.
				return d.frames[:0], ErrInvalidFrame
			}
			// A stray tail after one or more whole frames. RFC 2250 aggregates only
			// integral frames, so these bytes are not a frame; discard them.
			break
		}
		h, err := mpahdr.Parse(binary.BigEndian.Uint32(rem[:mpahdr.HeaderLen]))
		if err != nil {
			if off == 0 {
				return d.frames[:0], ErrInvalidFrame
			}
			// Junk after one or more whole frames: discard the remainder.
			break
		}
		if len(rem) < h.FrameLen {
			if off == 0 {
				// The only frame in this packet is larger than the payload: it is
				// fragmented across packets, so begin reassembly. Its continuation
				// fragments arrive with a matching timestamp and ascending offset.
				d.beginFragment(rem, rtpTime, h.FrameLen)
				return d.frames[:0], nil
			}
			// A short final frame after one or more whole frames. A fragmented frame is
			// sent alone (its own boundary packet), so a partial tail after aggregation
			// is malformed; discard it.
			break
		}
		frames = append(frames, Frame{
			Data:      rem[:h.FrameLen],
			RTPOffset: uint32(min(accTicks, maxRTPOffset)),
		})
		// Accumulate this frame's duration in RTP ticks for the next frame's offset.
		// Parse guarantees SampleRate > 0 and FrameLen >= HeaderLen, so neither the
		// division nor the loop advance can misbehave; the accumulator is uint64 and
		// the cast above saturates at maxRTPOffset, so a pathological clock cannot wrap
		// it.
		accTicks += uint64(h.SamplesPerFrame) * uint64(d.clockRate) / uint64(h.SampleRate)
		off += h.FrameLen
	}
	d.frames = frames
	return frames, nil
}

// beginFragment starts reassembling a frame whose first fragment arrived on a
// boundary packet (fragmentation offset 0) with fewer bytes than the frame
// length its header declared.
func (d *Depacketizer) beginFragment(data []byte, rtpTime uint32, frameLen int) {
	d.frag = append(d.frag[:0], data...)
	d.fragActive = true
	d.fragTime = rtpTime
	d.expectedLen = frameLen
}

// continueFragment handles a continuation packet (fragmentation offset > 0),
// appending its bytes to the frame in progress and returning that frame once it
// is complete.
func (d *Depacketizer) continueFragment(data []byte, fragOffset, rtpTime uint32) ([]Frame, error) {
	if !d.fragActive {
		// The head fragment (carrying the frame header) was lost, so these bytes
		// cannot be framed. Drop them.
		return d.frames[:0], ErrOrphanFragment
	}
	if rtpTime != d.fragTime || int(fragOffset) != len(d.frag) {
		// The frame boundary was lost (a dropped fragment or a misframing sender): the
		// reassembly is broken and this continuation has no header to start a new
		// frame, so drop the stale partial and this packet with it.
		d.Reset()
		return d.frames[:0], ErrOrphanFragment
	}
	if len(d.frag)+len(data) > d.expectedLen {
		// More bytes than the frame's declared length: corruption. Drop the partial.
		d.Reset()
		return d.frames[:0], ErrFrameOverflow
	}
	d.frag = append(d.frag, data...)
	if len(d.frag) < d.expectedLen {
		return d.frames[:0], nil // still buffering
	}
	// The frame is complete. Leave fragActive false; the returned frame aliases
	// d.frag until the next call.
	d.fragActive = false
	d.frames = append(d.frames[:0], Frame{Data: d.frag, RTPOffset: 0})
	return d.frames, nil
}

// Reset discards any partial fragment reassembly state. The caller invokes it on
// an RTP sequence discontinuity (SeqGap > 0) and on an SSRC change, so a lost
// fragment cannot be spliced onto the next frame.
func (d *Depacketizer) Reset() {
	d.fragActive = false
	d.frag = d.frag[:0]
	d.expectedLen = 0
}
