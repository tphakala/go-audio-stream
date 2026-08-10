package httpsource

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
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

var aacWantASC = []byte{0x12, 0x10}

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
// access unit (the payload, i.e. the frame with its header stripped) the framer
// should deliver for a single-block frame.
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
func drainAll(s *adtsStream) [][]byte {
	var out [][]byte
	for {
		data, _, ok := s.nextFrame()
		if !ok {
			break
		}
		out = append(out, append([]byte(nil), data...))
	}
	return out
}

// adtsFrames concatenates n single-block frames, each with a distinct payload
// marker, and returns the concatenated stream plus the access units (stripped
// payloads) the framer should deliver in order.
func adtsFrames(n, payloadLen int) (stream []byte, aus [][]byte) {
	aus = make([][]byte, n)
	for i := range n {
		frame, au := adtsFrame(byte(i+1), payloadLen)
		aus[i] = au
		stream = append(stream, frame...)
	}
	return stream, aus
}

func aacFrameDurNs() time.Duration {
	// 1024 samples / 44100 Hz = 1024 * 1e9 / 44100 = 23219954 ns (truncated).
	return time.Duration(adts.SamplesPerFrame) * time.Second / time.Duration(aacSampleRate)
}

func TestAACDeliversAccessUnitsAndASC(t *testing.T) {
	const n = 3
	stream, aus := adtsFrames(n, 40)
	srv := httptest.NewServer(serveStatic("audio/aac", stream))
	defer srv.Close()

	var col collector
	c := openOK(t, srv, Config{OnFrame: col.onFrame})
	if err := waitResult(t, c, 5*time.Second); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}

	got := col.snapshot()
	if len(got) != n {
		t.Fatalf("delivered %d frames, want %d", len(got), n)
	}
	frameDur := aacFrameDurNs()
	if frameDur != 23219954*time.Nanosecond {
		t.Fatalf("frameDur = %d ns, want 23219954", frameDur)
	}
	for i, f := range got {
		// The delivered Data is the raw access unit, the ADTS header stripped.
		if !bytes.Equal(f.Data, aus[i]) {
			t.Errorf("frame %d Data = % x, want the stripped access unit % x", i, f.Data, aus[i])
		}
		if want := time.Duration(i) * frameDur; f.PTS != want {
			t.Errorf("frame %d PTS = %v, want %v", i, f.PTS, want)
		}
	}

	// Format reports CodecAAC carrying the ASC synthesized from the ADTS header,
	// so a consumer decodes this exactly like an RTSP AAC track.
	fmtd := c.Format()
	if fmtd.Kind != audiostream.KindCompressed {
		t.Errorf("Format().Kind = %v, want KindCompressed", fmtd.Kind)
	}
	codec, ok := fmtd.Codec.(audiostream.CodecAAC)
	if !ok {
		t.Fatalf("Format().Codec = %T, want CodecAAC", fmtd.Codec)
	}
	if !bytes.Equal(codec.AudioSpecificConfig, aacWantASC) {
		t.Errorf("synthesized ASC = % x, want % x", codec.AudioSpecificConfig, aacWantASC)
	}
	if fmtd.SampleRate != 0 || fmtd.Channels != 0 {
		t.Errorf("compressed Format() SampleRate/Channels = %d/%d, want 0/0", fmtd.SampleRate, fmtd.Channels)
	}

	stats := c.Stats().Tracks[0]
	if stats.Packets != n {
		t.Errorf("Packets = %d, want %d", stats.Packets, n)
	}
	if stats.Malformed != 0 {
		t.Errorf("Malformed = %d, want 0 for a clean stream", stats.Malformed)
	}
}

func TestAACAacpAlias(t *testing.T) {
	stream, aus := adtsFrames(2, 30)
	srv := httptest.NewServer(serveStatic("audio/aacp", stream))
	defer srv.Close()

	var col collector
	c := openOK(t, srv, Config{OnFrame: col.onFrame})
	if err := waitResult(t, c, 5*time.Second); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	if col.count() != len(aus) {
		t.Errorf("audio/aacp delivered %d frames, want %d", col.count(), len(aus))
	}
	if _, ok := c.Format().Codec.(audiostream.CodecAAC); !ok {
		t.Errorf("audio/aacp Format().Codec = %T, want CodecAAC", c.Format().Codec)
	}
}

func TestAACResyncsPastLeadingGarbage(t *testing.T) {
	stream, aus := adtsFrames(2, 40)
	// Prepend non-sync garbage: the framer must skip it (counted as gaps) and
	// still deliver every access unit intact.
	body := append(bytes.Repeat([]byte{0x00, 0x11, 0x22}, 4), stream...)
	srv := httptest.NewServer(serveStatic("audio/aac", body))
	defer srv.Close()

	var col collector
	c := openOK(t, srv, Config{OnFrame: col.onFrame})
	if err := waitResult(t, c, 5*time.Second); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	want := append([]byte(nil), aus[0]...)
	want = append(want, aus[1]...)
	if !bytes.Equal(col.bytes(), want) {
		t.Errorf("delivered access units = % x, want % x", col.bytes(), want)
	}
	if got := c.Stats().Tracks[0].Malformed; got != 1 {
		t.Errorf("Malformed = %d, want 1 (a single discard of the 12-byte garbage run)", got)
	}
}

func TestAACDropsMultiBlockFrame(t *testing.T) {
	// A single-block frame, then a multi-block frame (nrdb=1) that cannot be
	// split, then another single-block frame. The middle frame is dropped and
	// counted; the two single-block access units are delivered.
	f1, au1 := adtsFrame(1, 40)
	f2, _ := adtsFrameN(2, 40, 1)
	f3, au3 := adtsFrame(3, 40)
	body := append(append(append([]byte(nil), f1...), f2...), f3...)
	srv := httptest.NewServer(serveStatic("audio/aac", body))
	defer srv.Close()

	var col collector
	c := openOK(t, srv, Config{OnFrame: col.onFrame})
	if err := waitResult(t, c, 5*time.Second); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	want := append(append([]byte(nil), au1...), au3...)
	if !bytes.Equal(col.bytes(), want) {
		t.Errorf("delivered = % x, want the two single-block AUs % x (multi-block dropped)", col.bytes(), want)
	}
	if got := c.Stats().Tracks[0].Malformed; got != 1 {
		t.Errorf("Malformed = %d, want 1 for the dropped multi-block frame", got)
	}
}

func TestAACFramesSplitAcrossReads(t *testing.T) {
	stream, aus := adtsFrames(4, 50)
	// A 10-byte chunk splits every frame (header + payload = 57 bytes) across
	// several socket reads, exercising the framer's cross-read buffering.
	srv := httptest.NewServer(serveChunked("audio/aac", stream, 10))
	defer srv.Close()

	var col collector
	c := openOK(t, srv, Config{OnFrame: col.onFrame})
	if err := waitResult(t, c, 5*time.Second); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	if col.count() != len(aus) {
		t.Fatalf("delivered %d frames, want %d", col.count(), len(aus))
	}
	var want []byte
	for _, au := range aus {
		want = append(want, au...)
	}
	if !bytes.Equal(col.bytes(), want) {
		t.Errorf("split-read delivery = % x, want % x", col.bytes(), want)
	}
}

func TestAACNonADTSBodyFailsOpen(t *testing.T) {
	// A body labeled AAC but carrying no parseable ADTS header must fail Open,
	// not deliver unframed bytes: the ASC cannot be resolved.
	srv := httptest.NewServer(serveStatic("audio/aac", bytes.Repeat([]byte{0x00, 0x11, 0x22}, 20)))
	defer srv.Close()
	if _, err := Open(context.Background(), Config{URL: srv.URL}); !errors.Is(err, ErrFormatUnknown) {
		t.Errorf("Open(non-ADTS audio/aac) = %v, want ErrFormatUnknown", err)
	}
}

func TestAACFramerDropsMultiBlockDirect(t *testing.T) {
	// Drive the framer directly to pin the multi-block drop without the reader.
	f1, au1 := adtsFrame(1, 20)
	f2, _ := adtsFrameN(2, 20, 2) // 3 raw data blocks
	f3, au3 := adtsFrame(3, 20)
	s := &adtsStream{}
	s.feed(append(append(append([]byte(nil), f1...), f2...), f3...))
	s.setEOF()

	var got [][]byte
	for {
		data, _, ok := s.nextFrame()
		if !ok {
			break
		}
		got = append(got, append([]byte(nil), data...))
	}
	if len(got) != 2 || !bytes.Equal(got[0], au1) || !bytes.Equal(got[1], au3) {
		t.Fatalf("delivered %d AUs, want the two single-block AUs (multi-block dropped)", len(got))
	}
	if s.gapCount() != 1 {
		t.Errorf("gapCount = %d, want 1 for the dropped multi-block frame", s.gapCount())
	}
}

func TestAACFramerStripsCRCHeader(t *testing.T) {
	// A CRC-protected (9-byte header) frame delivers the access unit after the
	// full 9 bytes; stripping only 7 would leak the 2 CRC bytes into the AU.
	f1, au1 := adtsFrameCRC(1, 30)
	f2, au2 := adtsFrameCRC(2, 30)
	s := &adtsStream{}
	s.feed(append(append([]byte(nil), f1...), f2...))
	s.setEOF()
	got := drainAll(s)
	if len(got) != 2 || !bytes.Equal(got[0], au1) || !bytes.Equal(got[1], au2) {
		t.Fatalf("CRC frames: got %d AUs, want 2 with the 9-byte header stripped", len(got))
	}
	if len(got[0]) != 30 {
		t.Errorf("CRC AU length = %d, want 30 (header and CRC stripped)", len(got[0]))
	}
}

func TestAACFramerRejectsFalseSync(t *testing.T) {
	// A valid-looking ADTS header claiming frameLen 14, but the bytes at +14 are
	// not a consistent header, so the framer must reject it as a false sync
	// rather than deliver its bytes as an access unit. A real frame follows and
	// must still be delivered.
	decoy := adtsHeaderFull(aacProfile, aacSRIdx, aacChanCfg, 14, 0, false)
	decoy = append(decoy, make([]byte, 14-adts.MinHeaderLen)...) // pad to the claimed 14 bytes
	f, au := adtsFrame(3, 40)
	body := append(append(append([]byte(nil), decoy...), make([]byte, 4)...), f...)
	s := &adtsStream{}
	s.feed(body)
	s.setEOF()
	got := drainAll(s)
	if len(got) != 1 || !bytes.Equal(got[0], au) {
		t.Fatalf("got %d AUs, want 1 real AU (the false sync rejected)", len(got))
	}
	if s.gapCount() == 0 {
		t.Error("gapCount = 0, want > 0 for the rejected false sync")
	}
}

func TestAACProbeRejectsUnconfirmedLeadingSync(t *testing.T) {
	// A false ADTS sync at a non-zero offset, with a frame too large to confirm
	// within the probe prefix, must NOT seed the ASC. With no confirmable frame
	// anywhere, Open fails rather than adopting a garbage config.
	falseHdr := adtsHeaderFull(1, 3, 1, 8000, 0, false) // LC/48000/mono, huge frameLen
	body := append(append([]byte{0x00, 0x00, 0x00}, falseHdr...), 0x00, 0x00)
	srv := httptest.NewServer(serveStatic("audio/aac", body))
	defer srv.Close()
	if _, err := Open(context.Background(), Config{URL: srv.URL}); !errors.Is(err, ErrFormatUnknown) {
		t.Errorf("Open with only an unconfirmable non-zero-offset sync = %v, want ErrFormatUnknown", err)
	}
}

func TestAACFramerBuffersAcrossReads(t *testing.T) {
	// Feed one byte at a time so every frame straddles a "read" boundary,
	// deterministically exercising the partial-frame wait the localhost socket
	// coalesces away in the end-to-end tests.
	stream, aus := adtsFrames(3, 40)
	s := &adtsStream{}
	got := make([][]byte, 0, len(aus))
	for _, b := range stream {
		s.feed([]byte{b})
		got = append(got, drainAll(s)...)
	}
	s.setEOF()
	got = append(got, drainAll(s)...)
	if len(got) != len(aus) {
		t.Fatalf("byte-by-byte delivery: got %d AUs, want %d", len(got), len(aus))
	}
	for i := range aus {
		if !bytes.Equal(got[i], aus[i]) {
			t.Errorf("AU %d mismatch under cross-read buffering", i)
		}
	}
}

func TestAACFramerCountsTruncatedTail(t *testing.T) {
	// A header whose frame never completes before EOF yields no access unit and
	// is counted once as a truncated tail.
	f, _ := adtsFrame(1, 40) // frameLen 47
	s := &adtsStream{}
	s.feed(f[:20]) // header plus a partial payload, short of frameLen
	s.setEOF()
	if got := drainAll(s); len(got) != 0 {
		t.Fatalf("delivered %d AUs from a truncated frame, want 0", len(got))
	}
	s.finish()
	if s.gapCount() != 1 {
		t.Errorf("gapCount = %d, want 1 for the truncated tail", s.gapCount())
	}
}

func TestAACFramerRejectsEmptyFrame(t *testing.T) {
	// A frame whose length equals its header carries a zero-length access unit,
	// which the parser rejects, so the framer never delivers an empty AU: such a
	// "frame" is treated as a false sync during resync.
	f1, _ := adtsFrame(1, 0) // frameLen == MinHeaderLen
	f2, _ := adtsFrame(2, 0)
	s := &adtsStream{}
	s.feed(append(append([]byte(nil), f1...), f2...))
	s.setEOF()
	if got := drainAll(s); len(got) != 0 {
		t.Fatalf("delivered %d AUs from zero-length frames, want 0 (empty frames rejected)", len(got))
	}
}

// aacID3v2Tag builds an ID3v2.3 tag: a 10-byte header, a body of bodyLen bytes,
// and an optional 10-byte footer. The body size is written as a proper 4-byte
// syncsafe integer so bodyLen may exceed the 4 KiB reader buffer, as an album-art
// tag does. Per ID3v2 the syncsafe size counts neither the header nor the footer,
// matching the skip length id3v2TagLen computes. The body is filled with bytes
// that mimic ADTS syncs (0xFF 0xFx), so a correct length-driven skip must ignore
// sync-looking bytes inside binary album-art data.
func aacID3v2Tag(bodyLen int, footer bool) []byte {
	flags := byte(0x00)
	if footer {
		flags = id3v2FooterFlag
	}
	tag := []byte{
		'I', 'D', '3', 0x03, 0x00, flags,
		byte((bodyLen >> 21) & 0x7F), byte((bodyLen >> 14) & 0x7F),
		byte((bodyLen >> 7) & 0x7F), byte(bodyLen & 0x7F),
	}
	body := make([]byte, bodyLen)
	for i := range body {
		body[i] = 0xFF
		if i%2 == 1 {
			body[i] = 0xF1
		}
	}
	tag = append(tag, body...)
	if footer {
		tag = append(tag, make([]byte, id3v2HeaderLen)...)
	}
	return tag
}

// runAACID3Skip serves body as audio/aac and asserts that Open skipped a leading
// ID3v2 tag: the ASC is synthesized from the first real frame, every access unit
// after the tag is delivered in order, and skipping the tag is not counted as a
// malformed gap.
func runAACID3Skip(t *testing.T, body []byte, aus [][]byte) {
	t.Helper()
	srv := httptest.NewServer(serveStatic("audio/aac", body))
	defer srv.Close()

	var col collector
	c := openOK(t, srv, Config{OnFrame: col.onFrame})
	if err := waitResult(t, c, 5*time.Second); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	codec, ok := c.Format().Codec.(audiostream.CodecAAC)
	if !ok {
		t.Fatalf("Format().Codec = %T, want CodecAAC", c.Format().Codec)
	}
	if !bytes.Equal(codec.AudioSpecificConfig, aacWantASC) {
		t.Errorf("synthesized ASC = % x, want % x", codec.AudioSpecificConfig, aacWantASC)
	}
	var want []byte
	for _, au := range aus {
		want = append(want, au...)
	}
	if col.count() != len(aus) || !bytes.Equal(col.bytes(), want) {
		t.Fatalf("delivered %d AUs (bytes-equal=%v), want %d matching the audio after the tag",
			col.count(), bytes.Equal(col.bytes(), want), len(aus))
	}
	if m := c.Stats().Tracks[0].Malformed; m != 0 {
		t.Errorf("Malformed = %d, want 0 (skipping an ID3v2 tag is not a gap)", m)
	}
}

// TestAACSkipsLeadingID3v2 covers the common static .aac case: a leading ID3v2
// tag precedes the ADTS frames. Open must skip the tag, synthesize the ASC from
// the first real frame, and deliver every access unit after it.
func TestAACSkipsLeadingID3v2(t *testing.T) {
	stream, aus := adtsFrames(3, 40)
	runAACID3Skip(t, append(aacID3v2Tag(64, false), stream...), aus)
}

// TestAACSkipsLargeID3v2 covers an album-art-sized tag larger than the 4 KiB
// reader buffer, which Peek cannot see past: Open must consume the tag by its
// declared length to reach the first ADTS frame far beyond any peek window.
func TestAACSkipsLargeID3v2(t *testing.T) {
	stream, aus := adtsFrames(3, 40)
	runAACID3Skip(t, append(aacID3v2Tag(12*1024, false), stream...), aus)
}

// TestAACSkipsID3v2WithFooter exercises the footer-flag branch: the skip length
// must include the trailing 10-byte footer so framing resumes exactly at the
// first ADTS frame.
func TestAACSkipsID3v2WithFooter(t *testing.T) {
	stream, aus := adtsFrames(3, 40)
	runAACID3Skip(t, append(aacID3v2Tag(30, true), stream...), aus)
}

// TestAACSkipsConsecutiveID3v2 covers a body that concatenates two ID3v2 tags
// before the audio: skipLeadingID3 must consume both in turn before the probe
// reaches the first ADTS frame.
func TestAACSkipsConsecutiveID3v2(t *testing.T) {
	stream, aus := adtsFrames(3, 40)
	body := append(aacID3v2Tag(48, false), aacID3v2Tag(24, true)...)
	body = append(body, stream...)
	runAACID3Skip(t, body, aus)
}

// TestAACRejectsTruncatedID3v2 covers the read-error branch in skipLeadingID3: an
// ID3v2 header declaring a body the stream never delivers cannot be skipped, so
// Open fails cleanly with ErrFormatUnknown rather than hanging or panicking.
// (Refining this open-phase read-error classification is tracked in #92.)
func TestAACRejectsTruncatedID3v2(t *testing.T) {
	tag := aacID3v2Tag(12*1024, false) // header declares a 12 KiB body
	body := tag[:id3v2HeaderLen+100]   // header plus 100 body bytes, then EOF
	srv := httptest.NewServer(serveStatic("audio/aac", body))
	defer srv.Close()

	_, err := Open(context.Background(), Config{URL: srv.URL})
	if !errors.Is(err, ErrFormatUnknown) {
		t.Fatalf("Open on a truncated ID3v2 tag = %v, want ErrFormatUnknown", err)
	}
	// Pin the Discard-error branch specifically: the probe's own no-frame path
	// also wraps ErrFormatUnknown, so without this the test could pass on either
	// route and mask a regression that swallowed the skip error.
	if !strings.Contains(err.Error(), "skipping leading ID3v2 tag") {
		t.Errorf("Open error = %v, want it to identify the ID3v2 skip path", err)
	}
}

// TestAACRejectsOversizeID3v2 covers the skip cap: an ID3v2 header declaring a
// body larger than maxID3v2SkipBytes is rejected on its declared length before
// any body byte is read, so a mislabelled or hostile audio/aac response cannot
// make Open stream a huge prefix. Only the 10-byte header is served; a correct
// fail-fast never waits on the (never-sent) body.
func TestAACRejectsOversizeID3v2(t *testing.T) {
	declared := maxID3v2SkipBytes // body size; total tag length exceeds the cap
	body := []byte{
		'I', 'D', '3', 0x03, 0x00, 0x00,
		byte((declared >> 21) & 0x7F), byte((declared >> 14) & 0x7F),
		byte((declared >> 7) & 0x7F), byte(declared & 0x7F),
	}
	srv := httptest.NewServer(serveStatic("audio/aac", body))
	defer srv.Close()

	_, err := Open(context.Background(), Config{URL: srv.URL})
	if !errors.Is(err, ErrFormatUnknown) {
		t.Fatalf("Open on an oversize ID3v2 tag = %v, want ErrFormatUnknown", err)
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("Open error = %v, want it to report the size limit", err)
	}
}
