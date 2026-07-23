package audiostream

import "time"

// Frame is one depacketized unit delivered to the consumer: an AAC
// access unit, one Opus packet, a block of G.711 samples converted to
// s16le PCM, or a raw RTP payload for unknown codecs.
//
// Data is valid only for the duration of the delivery callback; the
// library reuses buffers. Consumers that retain audio must copy.
type Frame struct {
	// TrackID is the ID of the track this frame belongs to.
	TrackID int
	// Data is the frame payload. Ownership stays with the library.
	Data []byte
	// RTPTime is the raw 32-bit RTP timestamp of the packet that
	// completed this frame.
	RTPTime uint32
	// PTS is the presentation time relative to the first received
	// frame of the track, computed from the unwrapped RTP timestamp
	// and the track clock rate.
	PTS time.Duration
	// ReceivedAt is the local receive time of the completing packet.
	ReceivedAt time.Time
	// SeqGap is the number of RTP packets lost immediately before
	// this frame, derived from sequence number tracking.
	SeqGap int
}
