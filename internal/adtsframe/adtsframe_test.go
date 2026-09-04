package adtsframe

import (
	"bytes"
	"testing"
	"time"

	"github.com/tphakala/go-audio-stream/internal/adts"
)

// The test frames use one fixed configuration, AAC-LC / 44100 Hz / stereo, whose
// synthesized AudioSpecificConfig is the well-known 0x1210.
const (
	aacProfile    = 1 // AAC-LC: audioObjectType 2
	aacSRIdx      = 4 // 44100 Hz
	aacChanCfg    = 2 // stereo
	aacSampleRate = 44100
)

// adtsHeaderFull hand-encodes an ADTS header (7 bytes, or 9 with a CRC) for the
// given fields, with buffer_fullness and the flag bits left 0. It is the test's
// own encoder so a header reads as fields, not a byte blob.
func adtsHeaderFull(profile, srIdx, chanCfg, frameLen, nrdb int, crc bool) []byte {
	hlen := adts.MinHeaderLen
	if crc {
		hlen = adts.CRCHeaderLen
	}
	b := make([]byte, hlen)
	b[0] = 0xFF
	b[1] = 0xF0 // syncword low nibble (1111) + layer 00
	if !crc {
		b[1] |= 0x01 // protection_absent
	}
	b[2] = byte(profile&0x03)<<6 | byte(srIdx&0x0F)<<2 | byte((chanCfg>>2)&0x01)
	b[3] = byte(chanCfg&0x03)<<6 | byte((frameLen>>11)&0x03)
	b[4] = byte((frameLen >> 3) & 0xFF)
	b[5] = byte((frameLen & 0x07) << 5)
	b[6] = byte(nrdb & 0x03)
	return b
}

// adtsFrameN builds one ADTS frame (7-byte header, no CRC) of total length
// 7+payloadLen with number_of_raw_data_blocks = nrdb, its payload filled with
// marker so a test can assert delivery order. It returns the whole frame and the
// access unit (the payload) the framer should deliver for a single-block frame.
func adtsFrameN(marker byte, payloadLen, nrdb int) (frame, au []byte) {
	frameLen := adts.MinHeaderLen + payloadLen
	frame = adtsHeaderFull(aacProfile, aacSRIdx, aacChanCfg, frameLen, nrdb, false)
	for range payloadLen {
		frame = append(frame, marker)
	}
	return frame, frame[adts.MinHeaderLen:]
}

// adtsFrame builds a single-block ADTS frame.
func adtsFrame(marker byte, payloadLen int) (frame, au []byte) {
	return adtsFrameN(marker, payloadLen, 0)
}

// adtsFrameCRC builds a single-block ADTS frame with a 9-byte CRC-protected
// header. The access unit begins after the 9-byte header.
func adtsFrameCRC(marker byte, payloadLen int) (frame, au []byte) {
	frameLen := adts.CRCHeaderLen + payloadLen
	frame = adtsHeaderFull(aacProfile, aacSRIdx, aacChanCfg, frameLen, 0, true)
	for range payloadLen {
		frame = append(frame, marker)
	}
	return frame, frame[adts.CRCHeaderLen:]
}

// drainAll pulls every access unit the framer can currently yield, copying each
// out of the aliased buffer.
func drainAll(s *Stream) [][]byte {
	var out [][]byte
	for {
		data, _, ok := s.NextFrame()
		if !ok {
			break
		}
		out = append(out, bytes.Clone(data))
	}
	return out
}

// adtsFrames concatenates n single-block frames, each with a distinct payload
// marker, and returns the concatenated stream plus the access units the framer
// should deliver in order.
func adtsFrames(n, payloadLen int) (stream []byte, aus [][]byte) {
	aus = make([][]byte, n)
	for i := range n {
		frame, au := adtsFrame(byte(i+1), payloadLen)
		aus[i] = au
		stream = append(stream, frame...)
	}
	return stream, aus
}

func TestStreamDeliversAllFramesInOrder(t *testing.T) {
	stream, aus := adtsFrames(4, 33)
	s := NewStream(1024)
	s.Feed(stream)
	s.SetEOF()
	got := drainAll(s)
	if len(got) != len(aus) {
		t.Fatalf("delivered %d AUs, want %d", len(got), len(aus))
	}
	for i := range aus {
		if !bytes.Equal(got[i], aus[i]) {
			t.Errorf("AU %d mismatch", i)
		}
	}
	if s.GapCount() != 0 {
		t.Errorf("GapCount = %d, want 0 for a clean stream", s.GapCount())
	}
}

func TestStreamFrameDuration(t *testing.T) {
	f, _ := adtsFrame(1, 40)
	s := NewStream(64)
	s.Feed(f)
	s.SetEOF()
	_, dur, ok := s.NextFrame()
	if !ok {
		t.Fatal("no frame delivered")
	}
	want := time.Duration(adts.SamplesPerFrame) * time.Second / time.Duration(aacSampleRate)
	if dur != want {
		t.Errorf("duration = %v, want %v", dur, want)
	}
}

func TestStreamDropsMultiBlockFrame(t *testing.T) {
	f1, au1 := adtsFrame(1, 20)
	f2, _ := adtsFrameN(2, 20, 2) // 3 raw data blocks
	f3, au3 := adtsFrame(3, 20)
	s := NewStream(0)
	s.Feed(append(append(bytes.Clone(f1), f2...), f3...))
	s.SetEOF()
	got := drainAll(s)
	if len(got) != 2 || !bytes.Equal(got[0], au1) || !bytes.Equal(got[1], au3) {
		t.Fatalf("delivered %d AUs, want the two single-block AUs (multi-block dropped)", len(got))
	}
	if s.GapCount() != 1 {
		t.Errorf("GapCount = %d, want 1 for the dropped multi-block frame", s.GapCount())
	}
}

func TestStreamStripsCRCHeader(t *testing.T) {
	f1, au1 := adtsFrameCRC(1, 30)
	f2, au2 := adtsFrameCRC(2, 30)
	s := NewStream(0)
	s.Feed(append(bytes.Clone(f1), f2...))
	s.SetEOF()
	got := drainAll(s)
	if len(got) != 2 || !bytes.Equal(got[0], au1) || !bytes.Equal(got[1], au2) {
		t.Fatalf("CRC frames: got %d AUs, want 2 with the 9-byte header stripped", len(got))
	}
	if len(got[0]) != 30 {
		t.Errorf("CRC AU length = %d, want 30 (header and CRC stripped)", len(got[0]))
	}
}

func TestStreamRejectsFalseSync(t *testing.T) {
	decoy := adtsHeaderFull(aacProfile, aacSRIdx, aacChanCfg, 14, 0, false)
	decoy = append(decoy, make([]byte, 14-adts.MinHeaderLen)...) // pad to the claimed 14 bytes
	f, au := adtsFrame(3, 40)
	body := append(append(bytes.Clone(decoy), make([]byte, 4)...), f...)
	s := NewStream(0)
	s.Feed(body)
	s.SetEOF()
	got := drainAll(s)
	if len(got) != 1 || !bytes.Equal(got[0], au) {
		t.Fatalf("got %d AUs, want 1 real AU (the false sync rejected)", len(got))
	}
	if s.GapCount() == 0 {
		t.Error("GapCount = 0, want > 0 for the rejected false sync")
	}
}

func TestStreamBuffersAcrossFeeds(t *testing.T) {
	// Feed one byte at a time so every frame straddles a feed boundary.
	stream, aus := adtsFrames(3, 40)
	s := NewStream(0)
	got := make([][]byte, 0, len(aus))
	for _, b := range stream {
		s.Feed([]byte{b})
		got = append(got, drainAll(s)...)
	}
	s.SetEOF()
	got = append(got, drainAll(s)...)
	if len(got) != len(aus) {
		t.Fatalf("byte-by-byte delivery: got %d AUs, want %d", len(got), len(aus))
	}
	for i := range aus {
		if !bytes.Equal(got[i], aus[i]) {
			t.Errorf("AU %d mismatch under cross-feed buffering", i)
		}
	}
}

func TestStreamCountsTruncatedTail(t *testing.T) {
	f, _ := adtsFrame(1, 40) // frameLen 47
	s := NewStream(0)
	s.Feed(f[:20]) // header plus a partial payload, short of frameLen
	s.SetEOF()
	if got := drainAll(s); len(got) != 0 {
		t.Fatalf("delivered %d AUs from a truncated frame, want 0", len(got))
	}
	s.Finish()
	if s.GapCount() != 1 {
		t.Errorf("GapCount = %d, want 1 for the truncated tail", s.GapCount())
	}
}

func TestStreamRejectsEmptyFrame(t *testing.T) {
	f1, _ := adtsFrame(1, 0) // frameLen == MinHeaderLen
	f2, _ := adtsFrame(2, 0)
	s := NewStream(0)
	s.Feed(append(bytes.Clone(f1), f2...))
	s.SetEOF()
	if got := drainAll(s); len(got) != 0 {
		t.Fatalf("delivered %d AUs from zero-length frames, want 0 (empty frames rejected)", len(got))
	}
}

func TestStreamCompactPreservesPartialFrame(t *testing.T) {
	// A frame delivered, then a partial frame left in the buffer: Compact must
	// slide the partial to the front so the next feed completes it.
	f1, au1 := adtsFrame(1, 20)
	f2, au2 := adtsFrame(2, 20)
	s := NewStream(0)
	s.Feed(f1)
	s.Feed(f2[:10]) // partial second frame
	got := drainAll(s)
	if len(got) != 1 || !bytes.Equal(got[0], au1) {
		t.Fatalf("got %d AUs before completion, want the first only", len(got))
	}
	s.Compact()
	s.Feed(f2[10:]) // rest of the second frame
	s.SetEOF()
	got = drainAll(s)
	if len(got) != 1 || !bytes.Equal(got[0], au2) {
		t.Fatalf("got %d AUs after completion, want the second", len(got))
	}
}
