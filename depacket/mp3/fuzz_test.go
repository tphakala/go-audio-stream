package mp3

import (
	"encoding/binary"
	"errors"
	"testing"

	mpahdr "github.com/tphakala/go-audio-stream/internal/mp3"
)

// isSentinel reports whether err is one of the package's documented sentinels.
func isSentinel(err error) bool {
	switch {
	case err == nil,
		errors.Is(err, ErrShortHeader),
		errors.Is(err, ErrInvalidFrame),
		errors.Is(err, ErrOrphanFragment),
		errors.Is(err, ErrFrameOverflow):
		return true
	default:
		return false
	}
}

// FuzzDepacketize drives the public entry point across a sequence of two packets
// (to exercise fragment continuation) with arbitrary bytes and fragmentation
// offsets. It must never panic, must return only documented sentinels, and every
// frame it returns must be a whole MPEG audio frame: its header must re-parse and
// its length must equal that header's FrameLen. That postcondition catches a
// mis-sliced or mis-reassembled frame, which a mere non-empty check would pass.
func FuzzDepacketize(f *testing.F) {
	seeds := []struct {
		p0, p1     []byte
		off0, off1 uint16
		t0, t1     uint32
	}{
		{nil, nil, 0, 0, 0, 0},
		{[]byte{0xFF, 0xFB, 0x90, 0x00}, nil, 0, 0, 1, 1},
		{frame(hdrMP1L3)[:200], frame(hdrMP1L3)[200:], 0, 200, 9, 9},
		{[]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF}, []byte{0x00}, 0, 5, 3, 3},
	}
	for _, s := range seeds {
		f.Add(s.p0, s.p1, s.off0, s.off1, s.t0, s.t1)
	}
	f.Fuzz(func(t *testing.T, p0, p1 []byte, off0, off1 uint16, t0, t1 uint32) {
		d := New(90000)
		run := func(data []byte, off uint16, rtpTime uint32) {
			// Build a well-formed RFC 2250 payload around the fuzzed audio bytes so
			// the fragmentation-offset field is exercised directly rather than as a
			// slice of the fuzzed body.
			payload := mpaPayload(off, data)
			frames, err := d.Depacketize(payload, rtpTime)
			if !isSentinel(err) {
				t.Fatalf("non-sentinel error: %v", err)
			}
			for _, fr := range frames {
				if len(fr.Data) < mpahdr.HeaderLen {
					t.Fatalf("returned frame too short to hold a header: %d bytes", len(fr.Data))
				}
				h, perr := mpahdr.Parse(binary.BigEndian.Uint32(fr.Data[:mpahdr.HeaderLen]))
				if perr != nil {
					t.Fatalf("returned frame does not begin with a valid header: %v", perr)
				}
				if len(fr.Data) != h.FrameLen {
					t.Fatalf("returned frame length %d != header FrameLen %d", len(fr.Data), h.FrameLen)
				}
			}
		}
		run(p0, off0, t0)
		run(p1, off1, t1)
	})
}
