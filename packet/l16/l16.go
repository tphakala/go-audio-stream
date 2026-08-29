// Package l16 packetizes little-endian S16 PCM into L16 RTP payloads.
//
// RFC 3551 defines L16 as big-endian (network order) 16-bit signed PCM, while
// ALSA and most capture paths deliver little-endian, so the packetizer swaps
// byte order per sample as it chunks. It is the inverse of the rtsp pipeline's
// L16 delivery, which swaps big-endian payloads back to little-endian.
package l16

import (
	"encoding/binary"
	"errors"
)

var (
	// ErrPartialFrame is returned when the input length is not a whole number
	// of frames (2*Channels bytes).
	ErrPartialFrame = errors.New("l16: input is not a whole number of frames")
	// ErrBadChannels is returned when Channels is not positive.
	ErrBadChannels = errors.New("l16: channels must be positive")
)

// Packetizer splits interleaved little-endian S16 PCM into big-endian L16 RTP
// payloads of at most MaxBytes, each a whole number of frames. Its internal
// swap buffer is reused across calls, so the payload passed to the callback is
// valid only for the duration of that call; copy it to retain it.
type Packetizer struct {
	Channels int // 1 or 2; frame size is 2*Channels bytes
	MaxBytes int // payload cap; rounded down to frame alignment (min one frame)

	buf []byte // reused big-endian output buffer
}

// Split calls fn once per payload, in order. pcm must be a whole number of
// frames or Split returns ErrPartialFrame before calling fn. It returns the
// number of frames consumed. An error from fn stops iteration and is returned
// with the frame count emitted so far.
func (p *Packetizer) Split(pcm []byte, fn func(payload []byte) error) (int, error) {
	if p.Channels < 1 {
		return 0, ErrBadChannels
	}
	frameBytes := 2 * p.Channels
	if len(pcm)%frameBytes != 0 {
		return 0, ErrPartialFrame
	}

	maxPayload := p.MaxBytes - (p.MaxBytes % frameBytes)
	if maxPayload < frameBytes {
		maxPayload = frameBytes // always make progress, at least one frame per payload
	}

	frames := 0
	for off := 0; off < len(pcm); {
		end := off + maxPayload
		if end > len(pcm) {
			end = len(pcm)
		}
		chunk := pcm[off:end]
		if cap(p.buf) < len(chunk) {
			p.buf = make([]byte, len(chunk))
		}
		out := p.buf[:len(chunk)]
		for i := 0; i+1 < len(chunk); i += 2 {
			binary.BigEndian.PutUint16(out[i:i+2], binary.LittleEndian.Uint16(chunk[i:i+2]))
		}
		if err := fn(out); err != nil {
			return frames, err
		}
		frames += len(chunk) / frameBytes
		off = end
	}
	return frames, nil
}
