package rtp_test

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"

	"github.com/tphakala/go-audio-stream/packet/l16"
	"github.com/tphakala/go-audio-stream/rtsp/rtp"
)

// TestSendPathAgainstIngest runs one second of the remote-mic hot path end to
// end inside the library: generate 48 kHz S16LE mono PCM, packetize it into
// 20 ms L16 payloads, marshal each into an RTP packet with running seq/ts,
// parse each back with the ingest parser, swap the big-endian payload back to
// little-endian the way the rtsp L16 delivery does, and assert the reassembled
// PCM is bit-identical to the source with monotonic seq/ts.
func TestSendPathAgainstIngest(t *testing.T) {
	const (
		rate         = 48000
		channels     = 1
		frameBytes   = 2 * channels
		periodFrames = rate / 50 // 20 ms => 960 frames
		payloadBytes = periodFrames * frameBytes
		totalFrames  = rate // one second
	)

	src := make([]byte, totalFrames*frameBytes)
	for i := range totalFrames {
		v := int16(0.5 * math.MaxInt16 * math.Sin(2*math.Pi*1000*float64(i)/rate))
		binary.LittleEndian.PutUint16(src[i*frameBytes:], uint16(v))
	}

	pk := &l16.Packetizer{Channels: channels, MaxBytes: payloadBytes}
	seq := uint16(1000)
	ts := uint32(5000)
	const ssrc = uint32(0xabcdef01)

	var reassembled []byte
	dst := make([]byte, 0, rtp.HeaderSize+payloadBytes)
	packets := 0
	prevSeq := seq - 1
	prevTS := ts

	frames, err := pk.Split(src, func(payload []byte) error {
		h := rtp.Header{Version: 2, PayloadType: 96, SequenceNumber: seq, Timestamp: ts, SSRC: ssrc}
		var perr error
		dst, perr = rtp.AppendPacket(dst[:0], h, payload)
		if perr != nil {
			return perr
		}
		pkt, perr := rtp.ParsePacket(dst)
		if perr != nil {
			return perr
		}
		if packets > 0 {
			if pkt.Header.SequenceNumber != prevSeq+1 {
				t.Errorf("packet %d: seq %d, want %d", packets, pkt.Header.SequenceNumber, prevSeq+1)
			}
			if pkt.Header.Timestamp != prevTS+periodFrames {
				t.Errorf("packet %d: ts %d, want %d", packets, pkt.Header.Timestamp, prevTS+periodFrames)
			}
		}
		if pkt.Header.SSRC != ssrc {
			t.Errorf("packet %d: ssrc %#x, want %#x", packets, pkt.Header.SSRC, ssrc)
		}
		prevSeq = pkt.Header.SequenceNumber
		prevTS = pkt.Header.Timestamp

		le := make([]byte, len(pkt.Payload))
		for i := 0; i+1 < len(pkt.Payload); i += 2 {
			binary.LittleEndian.PutUint16(le[i:i+2], binary.BigEndian.Uint16(pkt.Payload[i:i+2]))
		}
		reassembled = append(reassembled, le...)

		seq++
		ts += periodFrames
		packets++
		return nil
	})
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if frames != totalFrames {
		t.Errorf("frames = %d, want %d", frames, totalFrames)
	}
	if packets != totalFrames/periodFrames {
		t.Errorf("packets = %d, want %d", packets, totalFrames/periodFrames)
	}
	if !bytes.Equal(reassembled, src) {
		t.Errorf("reassembled PCM differs from source (len %d vs %d)", len(reassembled), len(src))
	}
}

// FuzzHeaderMarshalParse asserts AppendPacket then ParsePacket round-trips any
// valid header without loss.
func FuzzHeaderMarshalParse(f *testing.F) {
	f.Add(uint8(96), false, uint16(0), uint32(0), uint32(0), uint8(0))
	f.Add(uint8(127), true, uint16(65535), uint32(0xffffffff), uint32(1), uint8(2))
	f.Fuzz(func(t *testing.T, pt uint8, marker bool, seq uint16, ts, ssrc uint32, ncsrc uint8) {
		cc := int(ncsrc) % (rtp.MaxCSRC + 1)
		csrc := make([]uint32, 0, cc)
		for i := range cc {
			csrc = append(csrc, uint32(i)*7+1)
		}
		h := rtp.Header{
			Version:        2,
			Marker:         marker,
			PayloadType:    pt & 0x7f,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           ssrc,
			CSRC:           csrc,
		}
		raw, err := rtp.AppendPacket(nil, h, []byte{1, 2, 3})
		if err != nil {
			t.Fatalf("AppendPacket: %v", err)
		}
		pkt, err := rtp.ParsePacket(raw)
		if err != nil {
			t.Fatalf("ParsePacket: %v", err)
		}
		if pkt.Header.PayloadType != pt&0x7f || pkt.Header.Marker != marker ||
			pkt.Header.SequenceNumber != seq || pkt.Header.Timestamp != ts || pkt.Header.SSRC != ssrc {
			t.Errorf("header mismatch: got %+v", pkt.Header)
		}
		if !bytes.Equal(pkt.Payload, []byte{1, 2, 3}) {
			t.Errorf("payload = %v, want [1 2 3]", pkt.Payload)
		}
	})
}
