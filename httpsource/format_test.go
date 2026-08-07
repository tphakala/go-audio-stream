package httpsource

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFormatPrecedence(t *testing.T) {
	t.Run("bare L16 falls back to Config.Format", func(t *testing.T) {
		src := pcmMono(256)
		srv := httptest.NewServer(serveStatic("audio/l16", src))
		defer srv.Close()
		var col collector
		c := openOK(t, srv, Config{OnFrame: col.onFrame, Format: PCMFormat{SampleRate: 22050, Channels: 1}})
		_ = waitResult(t, c, 5*time.Second)
		if f := c.Format(); f.SampleRate != 22050 || f.Channels != 1 {
			t.Fatalf("Format = %+v, want SampleRate 22050 Channels 1", c.Format())
		}
		if !bytes.Equal(col.bytes(), src) {
			t.Fatal("L16 default byte order should be little-endian (delivered verbatim)")
		}
	})

	t.Run("Content-Type params beat Config.Format", func(t *testing.T) {
		srv := httptest.NewServer(serveStatic("audio/l16;rate=8000;channels=1", pcmMono(64)))
		defer srv.Close()
		c := openOK(t, srv, Config{Format: PCMFormat{SampleRate: 48000, Channels: 2}})
		_ = waitResult(t, c, 5*time.Second)
		if f := c.Format(); f.SampleRate != 8000 || f.Channels != 1 {
			t.Fatalf("Format = %+v, want SampleRate 8000 Channels 1 (params win)", c.Format())
		}
	})

	t.Run("explicit little-endian delivers L16 verbatim", func(t *testing.T) {
		src := pcmMono(256)
		srv := httptest.NewServer(serveStatic("audio/l16;rate=8000;channels=1", src))
		defer srv.Close()
		var col collector
		c := openOK(t, srv, Config{OnFrame: col.onFrame, Format: PCMFormat{Endian: EndianLittle}})
		_ = waitResult(t, c, 5*time.Second)
		if !bytes.Equal(col.bytes(), src) {
			t.Fatal("EndianLittle should deliver source bytes verbatim")
		}
	})

	t.Run("explicit big-endian byte-swaps L16 to s16le", func(t *testing.T) {
		be := pcmMono(256) // treated as a spec-strict big-endian source
		srv := httptest.NewServer(serveStatic("audio/l16;rate=8000;channels=1", be))
		defer srv.Close()
		var col collector
		c := openOK(t, srv, Config{OnFrame: col.onFrame, Format: PCMFormat{Endian: EndianBig}})
		_ = waitResult(t, c, 5*time.Second)
		if !bytes.Equal(col.bytes(), swapPairs(be)) {
			t.Fatal("EndianBig should byte-swap a big-endian source to little-endian s16le")
		}
	})

	t.Run("esp32-style L16 little-endian delivered verbatim", func(t *testing.T) {
		// esp32-audio-streamer's /stream.pcm labels the body audio/L16 but sends
		// native little-endian s16le. With no override it must be delivered
		// verbatim, not byte-swapped as an RFC 3551 reading would imply.
		src := pcmMono(256)
		srv := httptest.NewServer(serveStatic("audio/l16; rate=48000; channels=1", src))
		defer srv.Close()
		var col collector
		c := openOK(t, srv, Config{OnFrame: col.onFrame})
		_ = waitResult(t, c, 5*time.Second)
		if f := c.Format(); f.SampleRate != 48000 || f.Channels != 1 {
			t.Fatalf("Format = %+v, want SampleRate 48000 Channels 1", c.Format())
		}
		if !bytes.Equal(col.bytes(), src) {
			t.Fatal("esp32-style little-endian L16 should be delivered verbatim as s16le")
		}
	})

	t.Run("octet-stream sniffs a RIFF body as WAV", func(t *testing.T) {
		pcm := pcmMono(200)
		header := stdWAVHeader(wavFormatPCM, 1, 44100, 16, wavUnbounded, wavUnbounded)
		srv := httptest.NewServer(serveStatic("application/octet-stream", append(header, pcm...)))
		defer srv.Close()
		var col collector
		c := openOK(t, srv, Config{OnFrame: col.onFrame})
		if err := waitResult(t, c, 5*time.Second); !errors.Is(err, ErrStreamEnded) {
			t.Fatalf("Wait = %v, want ErrStreamEnded", err)
		}
		if c.Format().SampleRate != 44100 {
			t.Fatalf("sniffed WAV SampleRate = %d, want 44100", c.Format().SampleRate)
		}
		if !bytes.Equal(col.bytes(), pcm) {
			t.Fatal("sniffed WAV delivery diverged from source")
		}
	})

	t.Run("octet-stream without RIFF uses Config.Format little-endian", func(t *testing.T) {
		src := pcmMono(200)
		srv := httptest.NewServer(serveStatic("application/octet-stream", src))
		defer srv.Close()
		var col collector
		c := openOK(t, srv, Config{OnFrame: col.onFrame, Format: PCMFormat{SampleRate: 16000, Channels: 1}})
		_ = waitResult(t, c, 5*time.Second)
		if !bytes.Equal(col.bytes(), src) {
			t.Fatal("unlabeled embedded PCM should deliver little-endian source unswapped")
		}
	})

	t.Run("raw without a resolvable rate fails Open", func(t *testing.T) {
		srv := httptest.NewServer(serveStatic("audio/pcm", pcmMono(100)))
		defer srv.Close()
		if _, err := Open(context.Background(), Config{URL: srv.URL}); !errors.Is(err, ErrFormatUnknown) {
			t.Fatalf("Open = %v, want ErrFormatUnknown", err)
		}
	})

	t.Run("compressed media type fails Open", func(t *testing.T) {
		srv := httptest.NewServer(serveStatic("audio/mpeg", []byte{0xFF, 0xFB, 0x00}))
		defer srv.Close()
		if _, err := Open(context.Background(), Config{URL: srv.URL}); !errors.Is(err, ErrUnsupportedFormat) {
			t.Fatalf("Open = %v, want ErrUnsupportedFormat", err)
		}
	})
}

func TestFormatSniffedRF64BW64Rejected(t *testing.T) {
	// A 64-bit RIFF WAVE body reaching the sniff path (unlabeled or
	// octet-stream) must fail with ErrUnsupportedFormat, not fall through to the
	// raw fallback and get delivered as PCM. Config.Format is set so a wrong
	// fall-through would wrongly succeed, making this a real regression guard.
	for _, magic := range []string{rf64Magic, bw64Magic} {
		for _, ct := range []string{"application/octet-stream", ""} {
			t.Run(magic+"/"+ct, func(t *testing.T) {
				body := append([]byte(magic), le32(nil, 0xFFFFFFFF)...)
				body = append(body, waveMagic...)
				body = append(body, make([]byte, 32)...) // enough for Peek(12)
				srv := httptest.NewServer(serveStatic(ct, body))
				defer srv.Close()
				_, err := Open(context.Background(), Config{URL: srv.URL, Format: PCMFormat{SampleRate: 8000, Channels: 1}})
				if !errors.Is(err, ErrUnsupportedFormat) {
					t.Fatalf("Open = %v, want ErrUnsupportedFormat", err)
				}
			})
		}
	}
}

func TestFormatShortBodySniffFallthrough(t *testing.T) {
	// A body shorter than the RIFF signature makes Peek error; that must be
	// treated as "not WAV" and fall through to Config.Format, not fail Open.
	t.Run("short body with Config.Format opens", func(t *testing.T) {
		srv := httptest.NewServer(serveStatic("application/octet-stream", []byte{0x01, 0x02}))
		defer srv.Close()
		var col collector
		c := openOK(t, srv, Config{OnFrame: col.onFrame, Format: PCMFormat{SampleRate: 8000, Channels: 1}})
		if err := waitResult(t, c, 5*time.Second); !errors.Is(err, ErrStreamEnded) {
			t.Fatalf("Wait = %v, want ErrStreamEnded", err)
		}
		if got := col.bytes(); !bytes.Equal(got, []byte{0x01, 0x02}) {
			t.Fatalf("delivered %v, want the 2 source bytes", got)
		}
	})

	t.Run("short body without Config.Format fails cleanly", func(t *testing.T) {
		srv := httptest.NewServer(serveStatic("application/octet-stream", []byte{0x01, 0x02}))
		defer srv.Close()
		if _, err := Open(context.Background(), Config{URL: srv.URL}); !errors.Is(err, ErrFormatUnknown) {
			t.Fatalf("Open = %v, want ErrFormatUnknown", err)
		}
	})
}

func TestFormatWAVErrorsViaOpen(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		want error
	}{
		{"float fmt", stdWAVHeader(3, 1, 8000, 32, wavUnbounded, wavUnbounded), ErrUnsupportedFormat},
		{"eight-bit", stdWAVHeader(wavFormatPCM, 1, 8000, 8, wavUnbounded, wavUnbounded), ErrUnsupportedFormat},
		{"rf64", append([]byte("RF64"), append(le32(nil, 0xFFFFFFFF), []byte("WAVE")...)...), ErrUnsupportedFormat},
		{"truncated header", stdWAVHeader(wavFormatPCM, 1, 8000, 16, wavUnbounded, wavUnbounded)[:20], ErrMalformedWAV},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(serveStatic("audio/wav", tc.body))
			defer srv.Close()
			if _, err := Open(context.Background(), Config{URL: srv.URL}); !errors.Is(err, tc.want) {
				t.Fatalf("Open = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestFormatDataBeforeFmt(t *testing.T) {
	// RIFF/WAVE then a data chunk with no preceding fmt chunk.
	body := append([]byte(riffMagic), le32(nil, 0xFFFFFFFF)...)
	body = append(body, waveMagic...)
	body = append(body, dataChunkID...)
	body = le32(body, 0xFFFFFFFF)
	srv := httptest.NewServer(serveStatic("audio/wav", body))
	defer srv.Close()
	if _, err := Open(context.Background(), Config{URL: srv.URL}); !errors.Is(err, ErrMalformedWAV) {
		t.Fatalf("Open = %v, want ErrMalformedWAV", err)
	}
}
