package rtp_test

import (
	"testing"

	"github.com/tphakala/go-audio-stream/rtsp/rtp"
)

func FuzzParsePacket(f *testing.F) {
	f.Add([]byte{0x80, 0xE1, 0x00, 0x11, 0x00, 0x00, 0x27, 0x10,
		0x11, 0x22, 0x33, 0x44, 0x00, 0x10, 0xDE, 0xAD})
	f.Add(buildRTP(true, 97, 42, 9000, 0xCAFEBABE, []uint32{1, 2}, []byte{1, 2, 3, 4}, []byte{9, 9}, 2))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, buf []byte) {
		p, err := rtp.ParsePacket(buf)
		if err != nil {
			return
		}
		var s rtp.Stream
		_ = s.Observe(p.Header)
		_ = s.Stats()
	})
}

func FuzzParseCompound(f *testing.F) {
	f.Add(srBytes)
	f.Add([]byte{0x81, 0xCB, 0x00, 0x01, 0x00, 0x00, 0x00, 0x2A})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, buf []byte) {
		_, _ = rtp.ParseCompound(buf)
	})
}
