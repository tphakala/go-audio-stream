package flac

import "errors"

// MaxFrameSize is the largest FLAC frame, in bytes, that the reassembler will
// buffer across RTP packets. FLAC over RTP fragments a single frame across
// packets with no per-fragment length header, so the accumulation is bounded
// here rather than by any field in the payload: a flood of non-final fragments
// (marker bit clear) could otherwise grow the buffer without limit. A frame
// whose accumulated size would exceed this yields ErrFrameOverflow.
//
// 1 MiB is generous for the streams this library ingests: a 48 kHz 16-bit stereo
// FLAC frame at the common 4096-sample block size is on the order of 16 KiB, so a
// realistic mono or stereo ingest frame stays orders of magnitude below the cap.
// It is a DoS bound on the reassembly buffer, not a promise that every conceivable
// FLAC frame fits: an extreme many-channel, high-bit-depth frame at the maximum
// 65535-sample block size can exceed 1 MiB (8 channels x 65535 samples x 3 bytes
// is about 1.5 MiB) and would be rejected with ErrFrameOverflow. Such a frame is
// far outside the FLAC streamable subset and is not expected on this transport.
const MaxFrameSize = 1 << 20

// Sentinel errors. Depacketize returns one of these (never any other error
// value) and never panics.
var (
	// ErrEmptyPayload is returned when the RTP payload has zero length. A FLAC
	// frame (or fragment) is never empty on the wire, so an empty payload is
	// malformed. If it arrives mid-reassembly the partial frame is discarded.
	ErrEmptyPayload = errors.New("flac: empty RTP payload")
	// ErrFrameOverflow is returned when a fragmented frame's accumulated size
	// would exceed MaxFrameSize. The partial reassembly is discarded.
	ErrFrameOverflow = errors.New("flac: fragment reassembly overflow")
	// ErrTimestampMismatch is returned when a continuation fragment carries a
	// different RTP timestamp than the fragment that started the frame. All
	// fragments of one FLAC frame share the frame's timestamp, so a change means
	// the frame boundary was lost (a dropped final fragment, or a misframing
	// sender); the partial reassembly is discarded rather than spliced across the
	// boundary. This is the marker-only path's own integrity check, analogous to
	// the AAC depacketizer rejecting a continuation whose declared AU size differs.
	ErrTimestampMismatch = errors.New("flac: RTP timestamp changed mid-reassembly")
)

// Depacketizer reassembles FLAC frames from RTP payloads. A FLAC-over-RTP
// stream carries raw FLAC frames directly after the RTP header with no
// payload-specific header; a frame too large for one packet is split across
// consecutive packets and the RTP marker bit is set on the last fragment (and
// on every unfragmented single-packet frame). This type carries the
// cross-packet reassembly buffer, so one instance serves one RTP stream and is
// not safe for concurrent use.
type Depacketizer struct {
	frag       []byte
	fragActive bool
	// fragTime is the RTP timestamp of the fragment that started the frame now
	// being reassembled. Every fragment of one FLAC frame shares it; a
	// continuation carrying a different timestamp is rejected. Meaningful only
	// while fragActive is true.
	fragTime uint32
}

// New returns a ready Depacketizer.
func New() *Depacketizer {
	return &Depacketizer{}
}

// Depacketize processes one RTP payload and returns the FLAC frame it completes,
// or (nil, nil) while a fragmented frame is still being reassembled.
//
// marker is the RTP marker bit: it is set on an unfragmented frame carried in a
// single packet and on the final fragment of a frame split across packets. A
// packet with the marker set completes a frame (the payload alone when no
// fragment is in progress, or the accumulated fragments plus this payload); a
// packet with the marker clear starts or continues reassembly and completes
// nothing yet.
//
// The returned frame aliases either the input payload (the unfragmented
// single-packet case, allocation-free) or the depacketizer's own reassembly
// buffer (the reassembled case); it is valid only until the next call to
// Depacketize or Reset. Copy it to retain it.
//
// rtpTime is the packet's RTP timestamp. Every fragment of one FLAC frame
// carries the frame's timestamp, so a continuation whose timestamp differs from
// the fragment that started the frame is rejected with ErrTimestampMismatch and
// the partial reassembly discarded: this catches a lost final fragment or a
// misframing sender at the depacketizer, rather than relying solely on the
// caller resetting on a sequence gap.
//
// An empty payload returns ErrEmptyPayload; a fragmented frame whose accumulated
// size would exceed MaxFrameSize returns ErrFrameOverflow. On any error the
// partial reassembly is discarded so the next packet starts clean. The library
// does not decode or validate the FLAC bitstream: framing is delimited by the
// marker bit and the shared timestamp, and a sequence gap remains the caller's to
// guard by calling Reset on a discontinuity.
func (d *Depacketizer) Depacketize(payload []byte, marker bool, rtpTime uint32) ([]byte, error) {
	if len(payload) == 0 {
		if d.fragActive {
			d.Reset() // an empty payload mid-reassembly abandons the fragment.
		}
		return nil, ErrEmptyPayload
	}

	if !d.fragActive {
		if marker {
			// A complete frame in a single packet. Return it aliased without
			// touching the buffer, so the common unfragmented path allocates
			// nothing, exactly like the Opus pass-through path.
			return payload, nil
		}
		// First fragment of a frame split across packets. A single RTP payload is
		// MTU-bounded so it cannot itself exceed the cap, but guard defensively
		// before starting to buffer.
		if len(payload) > MaxFrameSize {
			return nil, ErrFrameOverflow
		}
		d.frag = append(d.frag[:0], payload...)
		d.fragActive = true
		d.fragTime = rtpTime
		return nil, nil
	}

	// Continuation of a frame already being reassembled. All fragments of one
	// frame share its timestamp, so a change means the frame boundary was lost;
	// drop the partial rather than splice two frames' bytes together.
	if rtpTime != d.fragTime {
		d.Reset()
		return nil, ErrTimestampMismatch
	}
	// Check the incoming length before appending so a run of fragments cannot be
	// copied into the buffer just to be rejected on the next call.
	if len(d.frag)+len(payload) > MaxFrameSize {
		d.Reset()
		return nil, ErrFrameOverflow
	}
	d.frag = append(d.frag, payload...)
	if !marker {
		return nil, nil // still buffering.
	}
	// The marker completes the frame. Leave fragActive false and do not clear
	// d.frag: the returned frame aliases it until the next call.
	d.fragActive = false
	return d.frag, nil
}

// Reset discards any partial fragment reassembly state. The caller invokes it on
// an RTP sequence discontinuity (SeqGap > 0), and on an SSRC change, so a lost
// fragment cannot be spliced onto the next frame.
func (d *Depacketizer) Reset() {
	d.fragActive = false
	d.frag = d.frag[:0]
}
