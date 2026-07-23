package opus

import "errors"

// ErrEmptyPayload is returned by Depacketize when the RTP payload has
// zero length. RFC 7587 puts no zero-length or DTX packets on the wire,
// so an empty payload is malformed rather than a comfort-noise frame.
var ErrEmptyPayload = errors.New("opus: empty RTP payload")

// Depacketize returns the Opus packet carried by one RTP payload. Under
// RFC 7587 the RTP payload IS the Opus packet, so this returns it
// unchanged (the returned slice aliases payload and is valid only for as
// long as the caller keeps payload alive). Extracting the packet needs
// no RTP metadata, so the marker bit is not a parameter; interpreting it,
// for talkspurt boundaries or anything else, stays with the caller that
// owns the RTP header. An empty payload returns ErrEmptyPayload.
//
// The returned bytes are handed to an Opus decoder such as
// github.com/tphakala/go-opus; this package does not validate the Opus
// TOC byte or frame structure, and it never constructs audiostream.Frame.
// The caller owns frame assembly, timestamps, and clock rate.
func Depacketize(payload []byte) ([]byte, error) {
	if len(payload) == 0 {
		return nil, ErrEmptyPayload
	}
	return payload, nil
}
