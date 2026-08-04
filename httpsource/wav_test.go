package httpsource

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"testing"
)

// wavReader wraps body in the same buffered reader the client uses.
func wavReader(body []byte) *bufio.Reader {
	return bufio.NewReaderSize(bytes.NewReader(body), readBufSize)
}

func TestParseWAVHeaderHappy(t *testing.T) {
	cases := []struct {
		name         string
		body         []byte
		wantRate     int
		wantChannels int
		wantBounded  bool
		wantData     uint32
	}{
		{
			name:         "mono unbounded",
			body:         stdWAVHeader(wavFormatPCM, 1, 48000, 16, wavUnbounded, wavUnbounded),
			wantRate:     48000,
			wantChannels: 1,
			wantBounded:  false,
		},
		{
			name:         "stereo bounded",
			body:         stdWAVHeader(wavFormatPCM, 2, 44100, 16, 16, 0xFFFFFFFF),
			wantRate:     44100,
			wantChannels: 2,
			wantBounded:  true,
			wantData:     16,
		},
		{
			name:         "eight channels at the limit",
			body:         stdWAVHeader(wavFormatPCM, 8, 96000, 16, 0, 0xFFFFFFFF),
			wantRate:     96000,
			wantChannels: 8,
			wantBounded:  false, // size 0 is unbounded
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info, err := parseWAVHeader(wavReader(tc.body))
			if err != nil {
				t.Fatalf("parseWAVHeader: %v", err)
			}
			if info.rate != tc.wantRate || info.channels != tc.wantChannels {
				t.Fatalf("got rate=%d channels=%d, want %d/%d", info.rate, info.channels, tc.wantRate, tc.wantChannels)
			}
			if info.bounded != tc.wantBounded || (tc.wantBounded && info.dataSize != tc.wantData) {
				t.Fatalf("got bounded=%v data=%d, want %v/%d", info.bounded, info.dataSize, tc.wantBounded, tc.wantData)
			}
		})
	}
}

func TestParseWAVHeaderPositionsAtData(t *testing.T) {
	payload := []byte("PCMPAYLOAD!!")
	body := append(stdWAVHeader(wavFormatPCM, 1, 8000, 16, uint32(len(payload)), 0xFFFFFFFF), payload...)
	br := wavReader(body)
	if _, err := parseWAVHeader(br); err != nil {
		t.Fatalf("parseWAVHeader: %v", err)
	}
	got, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("read after header: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("reader left at %q, want it positioned at %q", got, payload)
	}
}

func TestParseWAVHeaderSkipsUnknownChunks(t *testing.T) {
	// A JUNK chunk with an odd size (needs a pad byte) and a LIST chunk, both
	// before fmt, must be skipped, leaving the reader at the data payload.
	body := append([]byte(riffMagic), le32(nil, 0xFFFFFFFF)...)
	body = append(body, waveMagic...)
	// JUNK, size 3, 3 bytes + 1 pad.
	body = append(body, "JUNK"...)
	body = le32(body, 3)
	body = append(body, 0x01, 0x02, 0x03, 0x00)
	// LIST, size 4, 4 bytes.
	body = append(body, "LIST"...)
	body = le32(body, 4)
	body = append(body, "INFO"...)
	// fmt chunk (mono/8000/16).
	fmtBody := stdWAVHeader(wavFormatPCM, 1, 8000, 16, 4, 0xFFFFFFFF)
	body = append(body, fmtBody[12:]...) // skip the RIFF/WAVE prefix of the helper
	payload := []byte("DATA")
	body = append(body, payload...)

	br := wavReader(body)
	info, err := parseWAVHeader(br)
	if err != nil {
		t.Fatalf("parseWAVHeader: %v", err)
	}
	if info.rate != 8000 || info.channels != 1 {
		t.Fatalf("got %+v, want rate 8000 channels 1", info)
	}
	got, _ := io.ReadAll(br)
	if !bytes.Equal(got, payload) {
		t.Fatalf("after skipping chunks reader at %q, want %q", got, payload)
	}
}

func TestParseWAVHeaderFmtExtension(t *testing.T) {
	// A fmt chunk of size 18 (two extension bytes) must be accepted and its
	// extension skipped.
	body := append([]byte(riffMagic), le32(nil, 0xFFFFFFFF)...)
	body = append(body, waveMagic...)
	body = append(body, fmtChunkID...)
	body = le32(body, 18)
	body = le16(body, wavFormatPCM)
	body = le16(body, 1)
	body = le32(body, 8000)
	body = le32(body, 16000)
	body = le16(body, 2)
	body = le16(body, 16)
	body = le16(body, 0) // 2-byte extension size (cbSize)
	body = append(body, dataChunkID...)
	body = le32(body, 0xFFFFFFFF)

	info, err := parseWAVHeader(wavReader(body))
	if err != nil {
		t.Fatalf("parseWAVHeader: %v", err)
	}
	if info.rate != 8000 || info.channels != 1 {
		t.Fatalf("got %+v, want rate 8000 channels 1", info)
	}
}

func TestParseWAVHeaderErrors(t *testing.T) {
	riffWave := func() []byte {
		b := append([]byte(riffMagic), le32(nil, 0xFFFFFFFF)...)
		return append(b, waveMagic...)
	}
	cases := []struct {
		name string
		body []byte
		want error
	}{
		{"not riff", []byte("XXXX\x00\x00\x00\x00WAVE"), ErrMalformedWAV},
		{"riff not wave", append([]byte(riffMagic), append(le32(nil, 0xFFFFFFFF), []byte("AVI ")...)...), ErrMalformedWAV},
		{"rf64", append([]byte(rf64Magic), append(le32(nil, 0xFFFFFFFF), []byte(waveMagic)...)...), ErrUnsupportedFormat},
		{"bw64", append([]byte(bw64Magic), append(le32(nil, 0xFFFFFFFF), []byte(waveMagic)...)...), ErrUnsupportedFormat},
		{"float fmt", stdWAVHeader(3, 1, 8000, 32, 0xFFFFFFFF, 0xFFFFFFFF), ErrUnsupportedFormat},
		{"eight bit", stdWAVHeader(wavFormatPCM, 1, 8000, 8, 0xFFFFFFFF, 0xFFFFFFFF), ErrUnsupportedFormat},
		{"zero channels", stdWAVHeader(wavFormatPCM, 0, 8000, 16, 0xFFFFFFFF, 0xFFFFFFFF), ErrUnsupportedFormat},
		{"too many channels", stdWAVHeader(wavFormatPCM, 9, 8000, 16, 0xFFFFFFFF, 0xFFFFFFFF), ErrUnsupportedFormat},
		{"zero sample rate", stdWAVHeader(wavFormatPCM, 1, 0, 16, 0xFFFFFFFF, 0xFFFFFFFF), ErrMalformedWAV},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseWAVHeader(wavReader(tc.body)); !errors.Is(err, tc.want) {
				t.Fatalf("parseWAVHeader = %v, want %v", err, tc.want)
			}
		})
	}

	t.Run("fmt too small", func(t *testing.T) {
		body := riffWave()
		body = append(body, fmtChunkID...)
		body = le32(body, 10) // < 16
		body = append(body, make([]byte, 10)...)
		if _, err := parseWAVHeader(wavReader(body)); !errors.Is(err, ErrMalformedWAV) {
			t.Fatalf("parseWAVHeader = %v, want ErrMalformedWAV", err)
		}
	})

	t.Run("header exceeds budget", func(t *testing.T) {
		// A JUNK chunk claiming 2 MiB, without the bytes: the budget check trips
		// before any read, so this stays a tiny buffer.
		body := riffWave()
		body = append(body, "JUNK"...)
		body = le32(body, 2<<20)
		if _, err := parseWAVHeader(wavReader(body)); !errors.Is(err, ErrMalformedWAV) {
			t.Fatalf("parseWAVHeader = %v, want ErrMalformedWAV (budget)", err)
		}
	})
}

func TestParseWAVHeaderTruncations(t *testing.T) {
	full := append(stdWAVHeader(wavFormatPCM, 2, 44100, 16, 32, 0xFFFFFFFF), make([]byte, 4)...)
	// Cut at several points: mid RIFF header, mid a chunk header, and mid the
	// fmt body. Every one is a truncation wrapping io.ErrUnexpectedEOF.
	for _, cut := range []int{4, 10, 16, 30} {
		body := full[:cut]
		_, err := parseWAVHeader(wavReader(body))
		if !errors.Is(err, ErrMalformedWAV) || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("cut %d: parseWAVHeader = %v, want ErrMalformedWAV wrapping io.ErrUnexpectedEOF", cut, err)
		}
	}
}
