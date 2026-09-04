package rtp_test

import (
	"testing"

	"github.com/tphakala/go-audio-stream/rtsp/rtp"
)

// benchAudioPayload returns a realistic audio RTP payload of n bytes
// (audio packets typically run 100 to 1500 bytes), filled with
// non-repeating content so the parser never gets a degenerate all-zero
// buffer to work on.
func benchAudioPayload(n int) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = byte(i)
	}
	return p
}

// BenchmarkParsePacket covers the minimal 12-byte-header case and a packet
// carrying CSRCs, a header extension, and padding, the two shapes that
// exercise every branch of ParsePacket.
func BenchmarkParsePacket(b *testing.B) {
	payload := benchAudioPayload(172)

	b.Run("minimal", func(b *testing.B) {
		pkt := buildRTP(true, 97, 1000, 320000, 0x11223344, nil, nil, payload, 0)
		b.ReportAllocs()
		b.SetBytes(int64(len(pkt)))
		b.ResetTimer()
		for b.Loop() {
			if _, err := rtp.ParsePacket(pkt); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("csrc_ext_padding", func(b *testing.B) {
		ext := []byte{0xDE, 0xAD, 0xBE, 0xEF} // one 32-bit extension word
		csrc := []uint32{0xAAAAAAAA, 0xBBBBBBBB, 0xCCCCCCCC}
		pkt := buildRTP(true, 97, 1000, 320000, 0x11223344, csrc, ext, payload, 5)
		b.ReportAllocs()
		b.SetBytes(int64(len(pkt)))
		b.ResetTimer()
		for b.Loop() {
			if _, err := rtp.ParsePacket(pkt); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkStreamObserve measures the in-order sequence advance, the
// common case a live stream spends nearly all of its time in: every
// packet one greater than the last, no loss, no reorder, no SSRC change.
func BenchmarkStreamObserve(b *testing.B) {
	var s rtp.Stream
	h := rtp.Header{SSRC: 0x11223344, SequenceNumber: 0, Timestamp: 0}
	s.Observe(h) // establish init state before timing

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		h.SequenceNumber++
		h.Timestamp += 160
		s.Observe(h)
	}
}

// BenchmarkParseCompound measures parsing an RTCP compound packet holding a
// single Sender Report, the shape the keepalive path receives on every
// server SR.
func BenchmarkParseCompound(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(srBytes)))
	b.ResetTimer()
	for b.Loop() {
		if _, err := rtp.ParseCompound(srBytes); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkReceiverReportMarshal measures marshaling a Receiver Report
// carrying one report block, the shape sent for a single-track session.
func BenchmarkReceiverReportMarshal(b *testing.B) {
	rr := rtp.ReceiverReport{
		ReporterSSRC: 0xA5A5A5A5,
		Blocks: []rtp.ReportBlock{
			{
				SSRC:             0x11223344,
				FractionLost:     3,
				CumulativeLost:   42,
				HighestSequence:  1<<16 | 1000,
				Jitter:           120,
				LastSR:           0xCAFEBABE,
				DelaySinceLastSR: 4500,
			},
		},
	}
	out := rr.Marshal()

	b.ReportAllocs()
	b.SetBytes(int64(len(out)))
	b.ResetTimer()
	for b.Loop() {
		_ = rr.Marshal()
	}
}
