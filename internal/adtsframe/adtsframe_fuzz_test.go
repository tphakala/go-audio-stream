package adtsframe

import (
	"bytes"
	"testing"
)

// FuzzStream drives the framer with arbitrary bytes fed as one chunk at EOF,
// then drains it. It must never panic and must always terminate: every iteration
// either delivers a frame that consumed at least a header's worth of bytes or
// stops, so the frame count can never exceed the input length. This is a
// totality check over untrusted input; the framing behavior has dedicated tests.
func FuzzStream(f *testing.F) {
	stream, _ := adtsFrames(2, 30)
	f.Add(stream)
	f.Add([]byte{0xFF, 0xF1, 0x50, 0x80, 0x00, 0x1F, 0xFC})
	f.Add(bytes.Repeat([]byte{0xFF}, 64))
	f.Add(bytes.Repeat([]byte{0xFF, 0xF1}, 32))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		s := NewStream(0)
		s.Feed(data)
		s.SetEOF()
		limit := len(data) + 16 // a frame consumes >= 7 bytes, so this cannot be reached legitimately
		for n := 0; ; n++ {
			if n > limit {
				t.Fatal("NextFrame did not terminate: delivered more frames than input can hold")
			}
			if _, _, ok := s.NextFrame(); !ok {
				break
			}
		}
	})
}
