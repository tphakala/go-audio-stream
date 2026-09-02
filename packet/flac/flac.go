package flac

import "errors"

// Sentinel errors. Packetize returns one of these (never any other error value)
// and never panics.
var (
	// ErrEmptyFrame is returned when the FLAC frame to packetize has zero length.
	// A FLAC frame is never empty, so there is nothing to send.
	ErrEmptyFrame = errors.New("flac: empty FLAC frame")
	// ErrInvalidMTU is returned when maxPayload is not positive: a fragment must
	// be able to carry at least one byte.
	ErrInvalidMTU = errors.New("flac: non-positive max payload size")
)

// Fragment is one RTP payload carrying part (or all) of a FLAC frame, together
// with the RTP marker bit the sender must set on the packet that carries it.
// Data aliases the frame passed to Packetize, so it is valid only as long as
// that frame is; copy it to retain it. Every fragment of one frame shares the
// frame's RTP timestamp; the sender advances the sequence number by one per
// fragment.
type Fragment struct {
	// Data is the payload bytes to place directly after the RTP header (there is
	// no FLAC-over-RTP payload header).
	Data []byte
	// Marker is the value of the RTP marker bit for this fragment's packet: true
	// on the last fragment of a frame (and on a whole frame that fits one packet),
	// false on every earlier fragment. The receiver reassembles by concatenating
	// payloads until it sees the marker set.
	Marker bool
}

// Packetize splits a FLAC frame into ordered RTP payload fragments of at most
// maxPayload bytes each and appends them to dst, returning the extended slice.
// Pass dst[:0] (reusing a slice from a previous call) to avoid allocating the
// fragment slice per frame; the fragment Data values alias frame and allocate
// nothing.
//
// A frame that fits in maxPayload bytes yields a single fragment with Marker
// true. A larger frame yields ceil(len(frame)/maxPayload) fragments in order,
// each of size maxPayload except possibly the last, with Marker true on the last
// fragment only. This is the send-side counterpart to the
// depacket/flac reassembler: feeding a frame's fragments to that Depacketizer in
// order, marker included, reconstructs the original frame.
//
// It returns ErrInvalidMTU when maxPayload is not positive and ErrEmptyFrame when
// frame is empty; in both cases dst is returned unchanged.
//
// NOTE: this send-side helper is the first packetizer in a library otherwise
// chartered for receive/ingest. It is a pure transform (no RTP session state: the
// caller owns the sequence number, SSRC, timestamp, and socket) and exists so the
// de facto FLAC-over-RTP framing lives in one place for both ends.
func Packetize(dst []Fragment, frame []byte, maxPayload int) ([]Fragment, error) {
	if maxPayload <= 0 {
		return dst, ErrInvalidMTU
	}
	if len(frame) == 0 {
		return dst, ErrEmptyFrame
	}
	for off := 0; off < len(frame); off += maxPayload {
		end := off + maxPayload
		if end >= len(frame) {
			// Last fragment (the whole frame when it fits one packet): carries the
			// marker and stops the loop.
			dst = append(dst, Fragment{Data: frame[off:], Marker: true})
			break
		}
		dst = append(dst, Fragment{Data: frame[off:end], Marker: false})
	}
	return dst, nil
}
