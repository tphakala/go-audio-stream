package httpsource

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
)

// --- shared test helpers ---------------------------------------------------

const (
	testUser = "alice"
	testPass = "secret"
)

func le16(b []byte, v uint16) []byte { return binary.LittleEndian.AppendUint16(b, v) }
func le32(b []byte, v uint32) []byte { return binary.LittleEndian.AppendUint32(b, v) }

// stdWAVHeader builds a complete 44-byte canonical WAV header for integer PCM.
func stdWAVHeader(audioFormat, channels uint16, rate uint32, bits uint16, dataSize, riffSize uint32) []byte {
	b := []byte(riffMagic)
	b = le32(b, riffSize)
	b = append(b, waveMagic...)
	b = append(b, fmtChunkID...)
	b = le32(b, fmtChunkMinSize)
	b = le16(b, audioFormat)
	b = le16(b, channels)
	b = le32(b, rate)
	b = le32(b, rate*uint32(channels)*uint32(bits/8)) // byte rate
	b = le16(b, channels*bits/8)                      // block align
	b = le16(b, bits)
	b = append(b, dataChunkID...)
	b = le32(b, dataSize)
	return b
}

// pcmMono builds n little-endian s16 samples with a recognizable ramp.
func pcmMono(n int) []byte {
	b := make([]byte, 0, n*2)
	for i := range n {
		b = le16(b, uint16(i*7+1))
	}
	return b
}

// swapPairs returns the pairwise byte-swapped image of an even-length buffer.
func swapPairs(src []byte) []byte {
	out := make([]byte, len(src))
	for i := 0; i+1 < len(src); i += 2 {
		out[i] = src[i+1]
		out[i+1] = src[i]
	}
	return out
}

// collector records delivered frames, copying each Data since it aliases
// reader-owned memory. Every access is locked, so a test may read it while the
// reader is still delivering.
type collector struct {
	mu     sync.Mutex
	data   []byte
	frames []audiostream.Frame
}

//nolint:gocritic // The signature must match the OnFrame callback (func(audiostream.Frame)); it cannot take a pointer.
func (c *collector) onFrame(f audiostream.Frame) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make([]byte, len(f.Data))
	copy(cp, f.Data)
	f.Data = cp
	c.frames = append(c.frames, f)
	c.data = append(c.data, cp...)
}

func (c *collector) bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]byte, len(c.data))
	copy(out, c.data)
	return out
}

func (c *collector) snapshot() []audiostream.Frame {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]audiostream.Frame, len(c.frames))
	copy(out, c.frames)
	return out
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.frames)
}

// authCapture records the Basic credentials a handler received. The handler
// goroutine and the test goroutine share it with no happens-before edge over
// the socket, so every access is mutex-guarded to stay race-clean.
type authCapture struct {
	mu   sync.Mutex
	user string
	pass string
	ok   bool
}

func (a *authCapture) set(user, pass string, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.user, a.pass, a.ok = user, pass, ok
}

func (a *authCapture) get() (user, pass string, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.user, a.pass, a.ok
}

func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// serveStatic writes body with the given Content-Type then returns (clean EOF).
func serveStatic(contentType string, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}

// serveThenPark writes header, flushes, then blocks until the request context
// is cancelled or release fires, so a test can exercise the read-idle watchdog
// or a mid-stream Close.
func serveThenPark(contentType string, header []byte, release <-chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(header)
		flush(w)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}
}

// serveStream writes header then a frame every interval until the request
// context is cancelled, modeling an endless progressive stream.
func serveStream(contentType string, header, frame []byte, interval time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(header)
		flush(w)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-t.C:
				if _, err := w.Write(frame); err != nil {
					return
				}
				flush(w)
			}
		}
	}
}

// waitResult calls Wait in a goroutine and fails the test if it does not return
// within the bound, so a broken lifecycle surfaces as a clear failure rather
// than a hung test.
func waitResult(t *testing.T, c *Client, within time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.Wait(context.Background()) }()
	select {
	case err := <-done:
		return err
	case <-time.After(within):
		t.Fatal("Wait did not return within bound")
		return nil
	}
}

// wantPTS computes the expected presentation time of sample-frame index cum at
// the given rate, mirroring ptsOf's overflow-safe split.
func wantPTS(cum uint64, rate int) time.Duration {
	r := uint64(rate)
	sec := cum / r
	frac := (cum % r) * uint64(time.Second) / r
	return time.Duration(sec)*time.Second + time.Duration(frac)
}

// openOK opens against srv.URL with cfg and fails the test on error.
//
//nolint:gocritic // Test helper mirrors Open's documented by-value Config signature.
func openOK(t *testing.T, srv *httptest.Server, cfg Config) *Client {
	t.Helper()
	cfg.URL = srv.URL
	c, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return c
}

// --- tests -----------------------------------------------------------------

func TestOpenWAVHappyPath(t *testing.T) {
	pcm := pcmMono(1000) // 2000 bytes, whole mono frames
	header := stdWAVHeader(wavFormatPCM, 1, 48000, 16, wavUnbounded, wavUnbounded)
	srv := httptest.NewServer(serveStatic("audio/wav", append(header, pcm...)))
	defer srv.Close()

	var col collector
	c := openOK(t, srv, Config{OnFrame: col.onFrame})

	if err := waitResult(t, c, 5*time.Second); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	if got := col.bytes(); !bytes.Equal(got, pcm) {
		t.Fatalf("delivered %d bytes, want %d identical", len(got), len(pcm))
	}
	if f := c.Format(); f.SampleRate != 48000 || f.Channels != 1 || f.Kind != audiostream.KindPCMS16LE {
		t.Fatalf("Format = %+v, want SampleRate 48000 Channels 1 Kind pcm-s16le", f)
	}

	frames := col.snapshot()
	if len(frames) == 0 {
		t.Fatal("no frames delivered")
	}
	var cum uint64
	var prev time.Duration
	for i, f := range frames {
		if f.TrackID != 0 || f.RTPTime != 0 || f.SeqGap != 0 {
			t.Fatalf("frame %d = {track %d rtp %d gap %d}, want all zero", i, f.TrackID, f.RTPTime, f.SeqGap)
		}
		if f.ReceivedAt.IsZero() {
			t.Fatalf("frame %d has zero ReceivedAt", i)
		}
		if want := wantPTS(cum, 48000); f.PTS != want {
			t.Fatalf("frame %d PTS = %v, want %v", i, f.PTS, want)
		}
		if i > 0 && f.PTS <= prev {
			t.Fatalf("frame %d PTS %v not strictly after %v", i, f.PTS, prev)
		}
		prev = f.PTS
		cum += uint64(len(f.Data) / 2)
	}

	if s := c.Stats().Tracks[0]; s.PayloadBytes != uint64(len(pcm)) || s.Malformed != 0 {
		t.Fatalf("Stats = %+v, want PayloadBytes=%d Malformed=0", s, len(pcm))
	}
}

func TestOpenRawL16BigEndian(t *testing.T) {
	be := pcmMono(500) // treated as a spec-strict big-endian source
	srv := httptest.NewServer(serveStatic("audio/l16;rate=24000;channels=1", be))
	defer srv.Close()

	// audio/L16 now defaults to little-endian for embedded-device compatibility,
	// so a spec-strict RFC 3551 big-endian source needs the explicit EndianBig
	// opt-in to be byte-swapped to s16le on delivery.
	var col collector
	c := openOK(t, srv, Config{OnFrame: col.onFrame, Format: PCMFormat{Endian: EndianBig}})
	if err := waitResult(t, c, 5*time.Second); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	if got, want := col.bytes(), swapPairs(be); !bytes.Equal(got, want) {
		t.Fatalf("delivered bytes are not the byte-swapped image")
	}
	if c.Format().SampleRate != 24000 {
		t.Fatalf("SampleRate = %d, want 24000", c.Format().SampleRate)
	}
}

func TestOpenSizedDataChunk(t *testing.T) {
	t.Run("exact with trailing garbage", func(t *testing.T) {
		pcm := pcmMono(1000) // 2000 bytes
		header := stdWAVHeader(wavFormatPCM, 1, 44100, 16, 2000, 0xFFFFFFFF)
		body := append(append(header, pcm...), bytes.Repeat([]byte{0xAA}, 512)...)
		srv := httptest.NewServer(serveStatic("audio/wav", body))
		defer srv.Close()

		var col collector
		c := openOK(t, srv, Config{OnFrame: col.onFrame})
		if err := waitResult(t, c, 5*time.Second); !errors.Is(err, ErrStreamEnded) {
			t.Fatalf("Wait = %v, want ErrStreamEnded", err)
		}
		if got := col.bytes(); !bytes.Equal(got, pcm) {
			t.Fatalf("delivered %d bytes, want exactly the %d data bytes", len(got), len(pcm))
		}
		if s := c.Stats().Tracks[0]; s.Malformed != 0 {
			t.Fatalf("Malformed = %d, want 0", s.Malformed)
		}
	})

	t.Run("odd data size drops a partial frame", func(t *testing.T) {
		pcm := pcmMono(1000)
		pcm = append(pcm, 0x01) // 2001 bytes: one trailing partial frame
		header := stdWAVHeader(wavFormatPCM, 1, 44100, 16, 2001, 0xFFFFFFFF)
		srv := httptest.NewServer(serveStatic("audio/wav", append(header, pcm...)))
		defer srv.Close()

		var col collector
		c := openOK(t, srv, Config{OnFrame: col.onFrame})
		if err := waitResult(t, c, 5*time.Second); !errors.Is(err, ErrStreamEnded) {
			t.Fatalf("Wait = %v, want ErrStreamEnded", err)
		}
		if got := len(col.bytes()); got != 2000 {
			t.Fatalf("delivered %d bytes, want 2000", got)
		}
		if s := c.Stats().Tracks[0]; s.Malformed != 1 {
			t.Fatalf("Malformed = %d, want 1", s.Malformed)
		}
	})
}

func TestOpenStereoSplitReads(t *testing.T) {
	// Stereo frames are 4 bytes. The server dribbles the PCM in 3-byte writes,
	// so reads land mid-frame; the delivered stream must still equal the source
	// exactly, proving L/R alignment survives the split.
	pcm := make([]byte, 0, 4*300)
	for i := range 300 {
		pcm = le16(pcm, uint16(i))       // left
		pcm = le16(pcm, uint16(60000-i)) // right
	}
	header := stdWAVHeader(wavFormatPCM, 2, 32000, 16, wavUnbounded, wavUnbounded)
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(header)
		flush(w)
		for off := 0; off < len(pcm); off += 3 {
			end := min(off+3, len(pcm))
			_, _ = w.Write(pcm[off:end])
			flush(w)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(handler))
	defer srv.Close()

	var col collector
	c := openOK(t, srv, Config{OnFrame: col.onFrame})
	if err := waitResult(t, c, 10*time.Second); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	if got := col.bytes(); !bytes.Equal(got, pcm) {
		t.Fatalf("stereo delivery diverged from source (%d vs %d bytes)", len(got), len(pcm))
	}
	for _, f := range col.snapshot() {
		if len(f.Data)%4 != 0 {
			t.Fatalf("delivered a %d-byte frame, not a whole number of stereo frames", len(f.Data))
		}
	}
}

func TestOpenNilOnFrameStillCounts(t *testing.T) {
	header := stdWAVHeader(wavFormatPCM, 1, 16000, 16, 800, 0xFFFFFFFF)
	srv := httptest.NewServer(serveStatic("audio/wav", append(header, pcmMono(400)...)))
	defer srv.Close()

	c := openOK(t, srv, Config{}) // OnFrame nil
	if err := waitResult(t, c, 5*time.Second); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	if s := c.Stats().Tracks[0]; s.PayloadBytes != 800 || s.Packets == 0 {
		t.Fatalf("Stats = %+v, want PayloadBytes=800 Packets>0", s)
	}
}

func TestCloseMidStream(t *testing.T) {
	header := stdWAVHeader(wavFormatPCM, 1, 8000, 16, wavUnbounded, wavUnbounded)
	frame := pcmMono(80)
	srv := httptest.NewServer(serveStream("audio/wav", header, frame, 5*time.Millisecond))
	defer srv.Close()

	var col collector
	c := openOK(t, srv, Config{OnFrame: col.onFrame})

	// Wait for at least one frame so the Close is genuinely mid-delivery, and
	// fail (rather than silently proceed) if none arrived within the deadline:
	// Close+Wait returns ErrClosed even for a stream that delivered nothing, so
	// without this guard the test would pass without proving a mid-stream close.
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
	if err := c.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil (idempotent)", err)
	}
	if err := waitResult(t, c, 5*time.Second); !errors.Is(err, audiostream.ErrClosed) {
		t.Fatalf("Wait = %v, want ErrClosed", err)
	}
}

func TestCloseFromInsideOnFrame(t *testing.T) {
	header := stdWAVHeader(wavFormatPCM, 1, 8000, 16, wavUnbounded, wavUnbounded)
	frame := pcmMono(80)
	srv := httptest.NewServer(serveStream("audio/wav", header, frame, 5*time.Millisecond))
	defer srv.Close()

	var holder atomic.Pointer[Client]
	cfg := Config{OnFrame: func(_ audiostream.Frame) {
		if cl := holder.Load(); cl != nil {
			_ = cl.Close() // must not deadlock the reader it runs on
		}
	}}
	cfg.URL = srv.URL
	c, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	holder.Store(c)

	if err := waitResult(t, c, 5*time.Second); !errors.Is(err, audiostream.ErrClosed) {
		t.Fatalf("Wait = %v, want ErrClosed", err)
	}
}

func TestWaitContextCancel(t *testing.T) {
	header := stdWAVHeader(wavFormatPCM, 1, 8000, 16, wavUnbounded, wavUnbounded)
	frame := pcmMono(80)
	srv := httptest.NewServer(serveStream("audio/wav", header, frame, 5*time.Millisecond))
	defer srv.Close()

	c := openOK(t, srv, Config{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Wait(ctx) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after ctx cancel")
	}
	_ = c.Close()
}

func TestOpenCtxCancelAfterReturnKeepsStreaming(t *testing.T) {
	header := stdWAVHeader(wavFormatPCM, 1, 8000, 16, wavUnbounded, wavUnbounded)
	frame := pcmMono(80)
	srv := httptest.NewServer(serveStream("audio/wav", header, frame, 5*time.Millisecond))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	var col collector
	cfg := Config{OnFrame: col.onFrame, URL: srv.URL}
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	cancel() // cancelling the open ctx must not end the stream

	before := col.count()
	time.Sleep(150 * time.Millisecond)
	if after := col.count(); after <= before {
		t.Fatalf("stream stalled after open-ctx cancel: %d -> %d frames", before, after)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if err := waitResult(t, c, 5*time.Second); !errors.Is(err, audiostream.ErrClosed) {
		t.Fatalf("Wait = %v, want ErrClosed", err)
	}
}

func TestOpenExpiredContext(t *testing.T) {
	srv := httptest.NewServer(serveStatic("audio/wav", stdWAVHeader(wavFormatPCM, 1, 8000, 16, wavUnbounded, wavUnbounded)))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Open(ctx, Config{URL: srv.URL}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Open = %v, want context.Canceled", err)
	}
}

func TestCleanEOFvsAbort(t *testing.T) {
	header := stdWAVHeader(wavFormatPCM, 1, 8000, 16, wavUnbounded, wavUnbounded)

	t.Run("clean chunked EOF", func(t *testing.T) {
		srv := httptest.NewServer(serveStatic("audio/wav", append(header, pcmMono(200)...)))
		defer srv.Close()
		c := openOK(t, srv, Config{})
		if err := waitResult(t, c, 5*time.Second); !errors.Is(err, ErrStreamEnded) {
			t.Fatalf("Wait = %v, want ErrStreamEnded", err)
		}
	})

	t.Run("mid-body abort", func(t *testing.T) {
		handler := func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "audio/wav")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(header)
			_, _ = w.Write(pcmMono(200))
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
	})
}

func TestRedirectStatusAuth(t *testing.T) {
	t.Run("302 redirect surfaces RedirectError", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "http://elsewhere.example/stream")
			w.WriteHeader(http.StatusFound)
		}))
		defer srv.Close()
		_, err := Open(context.Background(), Config{URL: srv.URL})
		if !errors.Is(err, audiostream.ErrRedirect) {
			t.Fatalf("Open = %v, want ErrRedirect", err)
		}
		var re *audiostream.RedirectError
		if !errors.As(err, &re) || re.Location != "http://elsewhere.example/stream" {
			t.Fatalf("RedirectError = %+v, want Location set", re)
		}
	})

	for _, code := range []int{http.StatusNotFound, http.StatusUnauthorized} {
		t.Run("status error", func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
			}))
			defer srv.Close()
			_, err := Open(context.Background(), Config{URL: srv.URL})
			if !errors.Is(err, ErrBadStatus) {
				t.Fatalf("Open = %v, want ErrBadStatus", err)
			}
			var se *StatusError
			if !errors.As(err, &se) || se.Code != code {
				t.Fatalf("StatusError = %+v, want Code %d", se, code)
			}
		})
	}

	t.Run("basic auth from config and from url userinfo", func(t *testing.T) {
		var auth authCapture
		handler := func(w http.ResponseWriter, r *http.Request) {
			u, p, ok := r.BasicAuth()
			auth.set(u, p, ok)
			w.Header().Set("Content-Type", "audio/wav")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(stdWAVHeader(wavFormatPCM, 1, 8000, 16, 0, 0xFFFFFFFF))
		}
		srv := httptest.NewServer(http.HandlerFunc(handler))
		defer srv.Close()

		// Over plaintext http the caller must opt in to sending credentials.
		c := openOK(t, srv, Config{Username: testUser, Password: testPass, AllowInsecureAuth: true})
		_ = waitResult(t, c, 5*time.Second)
		if gotUser, gotPass, gotOK := auth.get(); !gotOK || gotUser != testUser || gotPass != testPass {
			t.Fatalf("config auth = %q/%q ok=%v, want alice/secret", gotUser, gotPass, gotOK)
		}

		// URL userinfo overrides Config, and Info().URL is credential-free.
		u := "http://bob:pw@" + srv.Listener.Addr().String() + "/s"
		c2, err := Open(context.Background(), Config{URL: u, Username: testUser, Password: testPass, AllowInsecureAuth: true})
		if err != nil {
			t.Fatalf("Open with userinfo: %v", err)
		}
		_ = waitResult(t, c2, 5*time.Second)
		if gotUser, gotPass, _ := auth.get(); gotUser != "bob" || gotPass != "pw" {
			t.Fatalf("url auth = %q/%q, want bob/pw (userinfo wins)", gotUser, gotPass)
		}
		if info := c2.Info(); bytes.ContainsAny([]byte(info.URL), "@") || info.URL != "http://"+srv.Listener.Addr().String()+"/s" {
			t.Fatalf("Info().URL = %q, want credential-free", info.URL)
		}
	})
}

func TestInfoServerHeader(t *testing.T) {
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Server", "acme-streamer/2.0")
		w.Header().Set("Content-Type", "audio/wav")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(stdWAVHeader(wavFormatPCM, 1, 8000, 16, 0, 0xFFFFFFFF))
	}
	srv := httptest.NewServer(http.HandlerFunc(handler))
	defer srv.Close()
	c := openOK(t, srv, Config{})
	if info := c.Info(); info.Server != "acme-streamer/2.0" {
		t.Fatalf("Info().Server = %q, want acme-streamer/2.0", info.Server)
	}
	_ = c.Close()
	_ = waitResult(t, c, 5*time.Second)
}

func TestOpenInvalidURL(t *testing.T) {
	for _, u := range []string{"", "://nope", "ftp://host/x", "http:///nohost"} {
		if _, err := Open(context.Background(), Config{URL: u}); !errors.Is(err, ErrInvalidURL) {
			t.Fatalf("Open(%q) = %v, want ErrInvalidURL", u, err)
		}
	}
}

func TestTLS(t *testing.T) {
	pcm := pcmMono(200)
	header := stdWAVHeader(wavFormatPCM, 1, 8000, 16, wavUnbounded, wavUnbounded)
	srv := httptest.NewTLSServer(serveStatic("audio/wav", append(header, pcm...)))
	defer srv.Close()

	t.Run("insecure skips verification and streams", func(t *testing.T) {
		var col collector
		c := openOK(t, srv, Config{OnFrame: col.onFrame, InsecureTLS: true})
		if err := waitResult(t, c, 5*time.Second); !errors.Is(err, ErrStreamEnded) {
			t.Fatalf("Wait = %v, want ErrStreamEnded", err)
		}
		if !bytes.Equal(col.bytes(), pcm) {
			t.Fatal("TLS delivery diverged from source")
		}
	})

	t.Run("verify failure wraps ErrConnectionClosed", func(t *testing.T) {
		// The httptest server uses a self-signed certificate the system pool
		// does not trust, so verified TLS (no InsecureTLS) must fail Open.
		if _, err := Open(context.Background(), Config{URL: srv.URL}); !errors.Is(err, ErrConnectionClosed) {
			t.Fatalf("Open = %v, want ErrConnectionClosed", err)
		}
	})
}

func TestInsecureAuthPolicy(t *testing.T) {
	var auth authCapture
	handler := func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		auth.set(u, p, ok)
		w.Header().Set("Content-Type", "audio/wav")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(stdWAVHeader(wavFormatPCM, 1, 8000, 16, 0, 0xFFFFFFFF))
	}
	// basicChallenge demands Basic auth. Over plaintext without the opt-in the
	// source sends the first request bare and then refuses to answer this
	// challenge rather than put the password on the wire in the clear.
	basicChallenge := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="camera"`)
		w.WriteHeader(http.StatusUnauthorized)
	}

	t.Run("http config creds refused by default", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(basicChallenge))
		defer srv.Close()
		_, err := Open(context.Background(), Config{URL: srv.URL, Username: testUser, Password: testPass})
		if !errors.Is(err, ErrInsecureAuth) {
			t.Fatalf("Open = %v, want ErrInsecureAuth", err)
		}
	})

	t.Run("http userinfo creds refused by default", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(basicChallenge))
		defer srv.Close()
		u := "http://bob:pw@" + srv.Listener.Addr().String() + "/s"
		if _, err := Open(context.Background(), Config{URL: u}); !errors.Is(err, ErrInsecureAuth) {
			t.Fatalf("Open = %v, want ErrInsecureAuth", err)
		}
	})

	t.Run("http creds with opt-in send auth", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(handler))
		defer srv.Close()
		c := openOK(t, srv, Config{Username: testUser, Password: testPass, AllowInsecureAuth: true})
		_ = waitResult(t, c, 5*time.Second)
		if gotUser, _, gotOK := auth.get(); !gotOK || gotUser != testUser {
			t.Fatalf("auth = %q ok=%v, want %s sent", gotUser, gotOK, testUser)
		}
	})

	t.Run("https creds always allowed", func(t *testing.T) {
		srv := httptest.NewTLSServer(http.HandlerFunc(handler))
		defer srv.Close()
		c := openOK(t, srv, Config{Username: testUser, Password: testPass, InsecureTLS: true})
		_ = waitResult(t, c, 5*time.Second)
		if gotUser, _, gotOK := auth.get(); !gotOK || gotUser != testUser {
			t.Fatalf("https auth = %q ok=%v, want %s sent", gotUser, gotOK, testUser)
		}
	})

	t.Run("http without creds unaffected", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(handler))
		defer srv.Close()
		c := openOK(t, srv, Config{})
		if err := waitResult(t, c, 5*time.Second); !errors.Is(err, ErrStreamEnded) {
			t.Fatalf("Wait = %v, want ErrStreamEnded", err)
		}
	})
}

func TestOpenTimeoutDoesNotKillPublishedStream(t *testing.T) {
	// Config.Timeout bounds only the open phase. Once the stream is published the
	// open timer must not tear it down: with a short Timeout and a stream that
	// runs well past it, delivery must continue and Wait must end on Close, never
	// with a timeout-induced ErrConnectionClosed.
	header := stdWAVHeader(wavFormatPCM, 1, 8000, 16, wavUnbounded, wavUnbounded)
	frame := pcmMono(80)
	srv := httptest.NewServer(serveStream("audio/wav", header, frame, 5*time.Millisecond))
	defer srv.Close()

	var col collector
	c := openOK(t, srv, Config{OnFrame: col.onFrame, Timeout: 60 * time.Millisecond})

	time.Sleep(200 * time.Millisecond) // well past the open Timeout
	before := col.count()
	if before == 0 {
		t.Fatal("no frames delivered")
	}
	time.Sleep(100 * time.Millisecond)
	if after := col.count(); after <= before {
		t.Fatalf("stream stalled past Timeout: %d -> %d frames", before, after)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if err := waitResult(t, c, 5*time.Second); !errors.Is(err, audiostream.ErrClosed) {
		t.Fatalf("Wait = %v, want ErrClosed", err)
	}
}
