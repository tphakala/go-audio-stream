package httpsource

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/internal/mp3"
)

// --- MP3 test helpers ------------------------------------------------------

// mp3HeaderBytes is a valid MPEG-1 Layer III, 128 kbps, 44100 Hz, stereo frame
// header. The corresponding whole frame is mp3FrameLen bytes.
var mp3HeaderBytes = []byte{0xFF, 0xFB, 0x90, 0x00}

const (
	mp3FrameLen   = 417   // header + payload for the header above
	mp3SampleRate = 44100 // Hz, from the header above
	mp3SPF        = 1152  // samples per frame, MPEG-1 Layer III
)

// mp3Frame builds one valid MP3 frame: the header followed by filler bytes set
// to marker. marker is kept below 0x80 so the payload never contains a 0xFF that
// could masquerade as an internal frame sync.
func mp3Frame(marker byte) []byte {
	f := make([]byte, mp3FrameLen)
	copy(f, mp3HeaderBytes)
	for i := len(mp3HeaderBytes); i < mp3FrameLen; i++ {
		f[i] = marker
	}
	return f
}

// mp3Frames concatenates n frames, each with a distinct payload marker so a
// test can assert both the count and the exact bytes and order of delivery.
func mp3Frames(n int) (stream []byte, frames [][]byte) {
	frames = make([][]byte, n)
	for i := range n {
		fr := mp3Frame(byte(i + 1))
		frames[i] = fr
		stream = append(stream, fr...)
	}
	return stream, frames
}

// id3v2Tag builds a minimal ID3v2.3 tag with a zero-filled body of bodyLen
// bytes and no footer. bodyLen must be below 128 so its syncsafe size fits one
// active byte, which is all these tests need.
func id3v2Tag(bodyLen int) []byte {
	tag := make([]byte, 0, id3v2HeaderLen+bodyLen)
	tag = append(tag, 'I', 'D', '3', 0x03, 0x00, 0x00, 0x00, 0x00, 0x00, byte(bodyLen))
	return append(tag, make([]byte, bodyLen)...)
}

// serveChunked writes body in fixed-size flushed chunks so a frame straddles
// the reader's socket reads, exercising the framer's cross-read buffering.
func serveChunked(contentType string, body []byte, chunk int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		for off := 0; off < len(body); off += chunk {
			end := min(off+chunk, len(body))
			if _, err := w.Write(body[off:end]); err != nil {
				return
			}
			flush(w)
		}
	}
}

// --- tests -----------------------------------------------------------------

func TestMP3DeliversFrames(t *testing.T) {
	const n = 3
	stream, frames := mp3Frames(n)
	srv := httptest.NewServer(serveStatic("audio/mpeg", stream))
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
	frameDur := time.Duration(mp3SPF) * time.Second / time.Duration(mp3SampleRate)
	// Pin the per-frame duration to its independently computed value (1152 samples
	// / 44100 Hz = 26122448 ns), so a wrong-but-matching formula on both sides
	// cannot pass. 1152 * 1e9 / 44100 truncates to 26122448.
	if frameDur != 26122448*time.Nanosecond {
		t.Fatalf("frameDur = %d ns, want 26122448", frameDur)
	}
	for i, f := range got {
		if !bytes.Equal(f.Data, frames[i]) {
			t.Errorf("frame %d bytes mismatch", i)
		}
		if want := time.Duration(i) * frameDur; f.PTS != want {
			t.Errorf("frame %d PTS = %v, want %v", i, f.PTS, want)
		}
	}

	if f := c.Format(); f.Kind != audiostream.KindCompressed {
		t.Errorf("Format().Kind = %v, want KindCompressed", f.Kind)
	}
	if _, ok := c.Format().Codec.(audiostream.CodecMP3); !ok {
		t.Errorf("Format().Codec = %T, want CodecMP3", c.Format().Codec)
	}
	if f := c.Format(); f.SampleRate != 0 || f.Channels != 0 {
		t.Errorf("compressed Format() SampleRate/Channels = %d/%d, want 0/0", f.SampleRate, f.Channels)
	}

	stats := c.Stats().Tracks[0]
	if stats.Packets != n {
		t.Errorf("Packets = %d, want %d", stats.Packets, n)
	}
	if want := uint64(n * mp3FrameLen); stats.PayloadBytes != want {
		t.Errorf("PayloadBytes = %d, want %d", stats.PayloadBytes, want)
	}
	if stats.Malformed != 0 {
		t.Errorf("Malformed = %d, want 0 for a clean stream", stats.Malformed)
	}
}

func TestMP3SkipsLeadingID3v2(t *testing.T) {
	stream, frames := mp3Frames(3)
	body := append(id3v2Tag(40), stream...)
	srv := httptest.NewServer(serveStatic("audio/mpeg", body))
	defer srv.Close()

	var col collector
	c := openOK(t, srv, Config{OnFrame: col.onFrame})
	if err := waitResult(t, c, 5*time.Second); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	if got := col.snapshot(); len(got) != len(frames) || !bytes.Equal(col.bytes(), stream) {
		t.Fatalf("delivered %d frames; bytes-equal=%v; want %d frames matching the audio after the tag",
			len(got), bytes.Equal(col.bytes(), stream), len(frames))
	}
	if m := c.Stats().Tracks[0].Malformed; m != 0 {
		t.Errorf("Malformed = %d, want 0 (skipping an ID3v2 tag is not a gap)", m)
	}
}

func TestMP3ResyncsPastLeadingGarbage(t *testing.T) {
	stream, _ := mp3Frames(3)
	garbage := bytes.Repeat([]byte("not audio"), 8) // 72 bytes, no 0xFF
	body := append(append([]byte{}, garbage...), stream...)
	srv := httptest.NewServer(serveStatic("audio/mpeg", body))
	defer srv.Close()

	var col collector
	c := openOK(t, srv, Config{OnFrame: col.onFrame})
	if err := waitResult(t, c, 5*time.Second); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	if !bytes.Equal(col.bytes(), stream) {
		t.Fatalf("delivered bytes do not match the audio after the garbage prefix")
	}
	if m := c.Stats().Tracks[0].Malformed; m == 0 {
		t.Errorf("Malformed = 0, want >0 after resynchronizing past garbage")
	}
}

// TestMP3RejectsFalseSync places a valid-looking header whose next-frame offset
// lands on non-header bytes. The consistency check must reject it and resync to
// the real frames, delivering only those.
func TestMP3RejectsFalseSync(t *testing.T) {
	_, genuine := mp3Frames(2)
	genuineStream := append(append([]byte{}, genuine[0]...), genuine[1]...)

	const decoyGap = 100
	body := make([]byte, 0, len(mp3HeaderBytes)+decoyGap+len(genuineStream))
	body = append(body, mp3HeaderBytes...)         // decoy header (claims a 417-byte frame)
	body = append(body, make([]byte, decoyGap)...) // too short: byte 417 past the decoy is not a header
	body = append(body, genuineStream...)          // the genuine frames

	srv := httptest.NewServer(serveStatic("audio/mpeg", body))
	defer srv.Close()

	var col collector
	c := openOK(t, srv, Config{OnFrame: col.onFrame})
	if err := waitResult(t, c, 5*time.Second); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	got := col.snapshot()
	if len(got) != 2 {
		t.Fatalf("delivered %d frames, want 2 (the decoy must not be delivered)", len(got))
	}
	if !bytes.Equal(got[0].Data, genuine[0]) || !bytes.Equal(got[1].Data, genuine[1]) {
		t.Fatalf("delivered frames are not the genuine ones")
	}
	if m := c.Stats().Tracks[0].Malformed; m == 0 {
		t.Errorf("Malformed = 0, want >0 after rejecting a false sync")
	}
}

func TestMP3FramesSplitAcrossReads(t *testing.T) {
	stream, frames := mp3Frames(4)
	srv := httptest.NewServer(serveChunked("audio/mpeg", stream, 64)) // frames span 64-byte chunks
	defer srv.Close()

	var col collector
	c := openOK(t, srv, Config{OnFrame: col.onFrame})
	if err := waitResult(t, c, 5*time.Second); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	got := col.snapshot()
	if len(got) != len(frames) {
		t.Fatalf("delivered %d frames, want %d", len(got), len(frames))
	}
	for i, f := range got {
		if !bytes.Equal(f.Data, frames[i]) {
			t.Errorf("frame %d reassembled wrong across read boundaries", i)
		}
	}
}

// TestLooksLikeID3 pins the syncsafe-size guard: only a well-formed ID3v2 header
// (syncsafe size bytes) is treated as a tag, so an "ID3" byte run in the audio
// payload does not trigger a bogus, possibly huge, skip.
func TestLooksLikeID3(t *testing.T) {
	valid := []byte{'I', 'D', '3', 0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x2A} // size 42, syncsafe
	if is, more := looksLikeID3(valid); !is || more {
		t.Fatalf("valid tag: is=%v more=%v, want true,false", is, more)
	}
	// A size byte with its top bit set is not syncsafe: not a real tag.
	nonSyncsafe := []byte{'I', 'D', '3', 0x03, 0x00, 0x00, 0x80, 0x00, 0x00, 0x00}
	if is, _ := looksLikeID3(nonSyncsafe); is {
		t.Fatal("non-syncsafe size must not be treated as an ID3 tag")
	}
	if is, more := looksLikeID3([]byte("ID3xy")); !is || !more {
		t.Fatalf("short ID3: is=%v more=%v, want true,true", is, more)
	}
	if is, _ := looksLikeID3([]byte("XYZ0123456")); is {
		t.Fatal("non-ID3 bytes must not match")
	}
}

// TestMP3FramerReconfirmsAfterLostSync guards the state-machine fix: once a
// garbage run has broken sync, a following candidate header with no lookahead
// must be held for the next-frame confirmation, not delivered on a stale synced
// flag. Pre-fix, the last next() delivered the unconfirmed frame.
func TestMP3FramerReconfirmsAfterLostSync(t *testing.T) {
	_, f := mp3Frames(3)
	s := &mp3Stream{}
	s.Feed(f[0])
	s.Feed(f[1])
	s.Feed(f[2])
	delivered := 0
	for {
		if _, _, ok := s.next(); ok {
			delivered++
		} else {
			break
		}
	}
	s.Compact()
	if delivered != 3 || !s.synced {
		t.Fatalf("setup: delivered=%d synced=%v, want 3 and synced", delivered, s.synced)
	}

	// A run of non-header garbage breaks sync.
	s.Feed(bytes.Repeat([]byte{'x'}, 50))
	for {
		if _, _, ok := s.next(); ok {
			t.Fatal("garbage was delivered as a frame")
		} else {
			break
		}
	}
	s.Compact()
	if s.synced {
		t.Fatal("sync was not cleared after a garbage run")
	}

	// A lone valid frame with no following header must be held for confirmation.
	s.Feed(f[0])
	if _, _, ok := s.next(); ok {
		t.Fatal("delivered an unconfirmed frame on a stale synced flag after losing sync")
	}
}

// TestMP3AllGarbageNeverSyncs covers a stream that never contains a valid frame:
// zero frames delivered, an orderly end, and a nonzero malformed count.
func TestMP3AllGarbageNeverSyncs(t *testing.T) {
	body := bytes.Repeat([]byte("no audio "), 200) // no 0xFF anywhere
	srv := httptest.NewServer(serveStatic("audio/mpeg", body))
	defer srv.Close()

	var col collector
	c := openOK(t, srv, Config{OnFrame: col.onFrame})
	if err := waitResult(t, c, 5*time.Second); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	if col.count() != 0 {
		t.Fatalf("delivered %d frames, want 0 for a stream with no valid frame", col.count())
	}
	if c.Stats().Tracks[0].Malformed == 0 {
		t.Error("Malformed = 0, want >0 for a never-syncing stream")
	}
}

// TestMP3SkipsID3v2WithFooter exercises the footer-flag branch: the skip length
// must include the 10-byte footer so framing resumes exactly at the audio.
func TestMP3SkipsID3v2WithFooter(t *testing.T) {
	const bodyLen = 30
	tag := make([]byte, 0, id3v2HeaderLen+bodyLen+id3v2HeaderLen)
	tag = append(tag, 'I', 'D', '3', 0x03, 0x00, id3v2FooterFlag, 0x00, 0x00, 0x00, byte(bodyLen))
	tag = append(tag, make([]byte, bodyLen)...)        // tag body
	tag = append(tag, make([]byte, id3v2HeaderLen)...) // 10-byte footer, skipped by count
	stream, frames := mp3Frames(3)

	srv := httptest.NewServer(serveStatic("audio/mpeg", append(tag, stream...)))
	defer srv.Close()

	var col collector
	c := openOK(t, srv, Config{OnFrame: col.onFrame})
	if err := waitResult(t, c, 5*time.Second); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	if col.count() != len(frames) || !bytes.Equal(col.bytes(), stream) {
		t.Fatalf("delivered %d frames; bytes-equal=%v; want %d frames matching the audio after the footered tag",
			col.count(), bytes.Equal(col.bytes(), stream), len(frames))
	}
	if m := c.Stats().Tracks[0].Malformed; m != 0 {
		t.Errorf("Malformed = %d, want 0 (skipping a footered ID3v2 tag is not a gap)", m)
	}
}

// TestMP3NilOnFrameStillCounts mirrors the PCM nil-OnFrame contract: with no
// callback the coded frames are still counted in Stats.
func TestMP3NilOnFrameStillCounts(t *testing.T) {
	stream, frames := mp3Frames(3)
	srv := httptest.NewServer(serveStatic("audio/mpeg", stream))
	defer srv.Close()

	c := openOK(t, srv, Config{}) // OnFrame nil
	if err := waitResult(t, c, 5*time.Second); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	stats := c.Stats().Tracks[0]
	if stats.Packets != uint64(len(frames)) {
		t.Errorf("Packets = %d, want %d", stats.Packets, len(frames))
	}
	if want := uint64(len(frames) * mp3FrameLen); stats.PayloadBytes != want {
		t.Errorf("PayloadBytes = %d, want %d", stats.PayloadBytes, want)
	}
}

// TestMP3AudioMp3Alias verifies the audio/mp3 content-type alias also frames MP3.
func TestMP3AudioMp3Alias(t *testing.T) {
	stream, frames := mp3Frames(2)
	srv := httptest.NewServer(serveStatic("audio/mp3", stream))
	defer srv.Close()

	var col collector
	c := openOK(t, srv, Config{OnFrame: col.onFrame})
	if err := waitResult(t, c, 5*time.Second); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	if col.count() != len(frames) || !bytes.Equal(col.bytes(), stream) {
		t.Fatalf("audio/mp3 alias: delivered %d frames, bytes-equal=%v, want %d matching",
			col.count(), bytes.Equal(col.bytes(), stream), len(frames))
	}
}

// TestMP3CloseMidStream closes an endless MP3 stream mid-delivery and expects
// ErrClosed, exercising the compressed reader's closing-select path.
func TestMP3CloseMidStream(t *testing.T) {
	srv := httptest.NewServer(serveStream("audio/mpeg", nil, mp3Frame(1), 2*time.Millisecond))
	defer srv.Close()

	var col collector
	c := openOK(t, srv, Config{OnFrame: col.onFrame})
	deadline := time.Now().Add(2 * time.Second)
	for col.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if col.count() == 0 {
		t.Fatal("no frame arrived before the deadline; cannot prove a mid-stream close")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if err := waitResult(t, c, 5*time.Second); !errors.Is(err, audiostream.ErrClosed) {
		t.Fatalf("Wait = %v, want ErrClosed", err)
	}
}

// TestMP3ConnLossMidStream drops the connection mid-body and expects
// ErrConnectionClosed, not an orderly end: a lost connection is not a truncated
// final frame, so the EOF-only finish path must not run.
func TestMP3ConnLossMidStream(t *testing.T) {
	stream, _ := mp3Frames(3)
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(stream)
		flush(w)
		panic(http.ErrAbortHandler) // drops the connection mid-stream
	}
	srv := httptest.NewServer(http.HandlerFunc(handler))
	defer srv.Close()

	c := openOK(t, srv, Config{})
	err := waitResult(t, c, 5*time.Second)
	if !errors.Is(err, ErrConnectionClosed) {
		t.Fatalf("Wait = %v, want ErrConnectionClosed", err)
	}
	if errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, an abort must not read as an orderly end", err)
	}
}

// FuzzMP3Stream drives the framer with arbitrary chunked input and asserts its
// invariants hold: it never panics, the cursor never passes the buffer end,
// every delivered slice is a whole valid coded frame, and it never emits more
// bytes than it consumed.
func FuzzMP3Stream(f *testing.F) {
	seed, _ := mp3Frames(2)
	f.Add(seed, 7)
	f.Add(append(id3v2Tag(20), seed...), 3)
	f.Add([]byte("no audio here at all"), 5)
	f.Fuzz(func(t *testing.T, data []byte, chunk int) {
		if chunk <= 0 {
			chunk = 1
		}
		s := &mp3Stream{}
		var emitted int
		drain := func() {
			for {
				frame, hdr, ok := s.next()
				if !ok {
					break
				}
				// The cursor must never pass the buffer end (checked here, before
				// compact resets off to 0), and every delivered slice must be a
				// whole, valid coded frame: its length matches the header FrameLen
				// and the header re-parses.
				if s.off > len(s.buf) {
					t.Fatalf("off %d past buffer len %d", s.off, len(s.buf))
				}
				if len(frame) != hdr.FrameLen {
					t.Fatalf("delivered %d bytes, header FrameLen %d", len(frame), hdr.FrameLen)
				}
				if _, err := mp3.Parse(binary.BigEndian.Uint32(frame[:mp3.HeaderLen])); err != nil {
					t.Fatalf("delivered frame does not re-parse: %v", err)
				}
				emitted += len(frame)
			}
			s.Compact()
		}
		for off := 0; off < len(data); off += chunk {
			end := min(off+chunk, len(data))
			s.Feed(data[off:end])
			drain()
		}
		s.ended = true
		drain()
		s.Finish()
		if emitted > len(data) {
			t.Fatalf("emitted %d bytes from %d bytes of input", emitted, len(data))
		}
	})
}
