package httpsource

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// wavReader wraps body in the same buffered reader the client uses.
func wavReader(body []byte) *bufio.Reader {
	return bufio.NewReaderSize(bytes.NewReader(body), readBufSize)
}

// nonPCMSubFormatGUID is KSDATAFORMAT_SUBTYPE_IEEE_FLOAT, used to exercise
// the "wrong SubFormat" rejection path. It differs from pcmSubFormatGUID only
// in the first byte of data1.
var nonPCMSubFormatGUID = [16]byte{
	0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00,
	0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71,
}

// extensibleFmtBody returns the audioFormat-through-SubFormat bytes of a
// WAVE_FORMAT_EXTENSIBLE fmt chunk (40 bytes: the 16-byte base plus the
// cbSize, valid bits per sample, channel mask, and SubFormat GUID
// extension), so a test can wrap it with a declared chunk size that lies
// about the true length, truncate it mid-stream, or append trailing bytes
// past the GUID.
func extensibleFmtBody(channels uint16, rate uint32, bits, validBits uint16, subFormat [16]byte) []byte {
	b := le16(nil, wavFormatExtensible)
	b = le16(b, channels)
	b = le32(b, rate)
	b = le32(b, rate*uint32(channels)*uint32(bits/8))
	b = le16(b, channels*bits/8)
	b = le16(b, bits)
	b = le16(b, fmtExtensibleCbSize)
	b = le16(b, validBits)
	b = le32(b, 0) // dwChannelMask, ignored by this parser
	b = append(b, subFormat[:]...)
	return b
}

// wavWithFmtBody assembles a RIFF/WAVE stream from a raw fmt chunk body, an
// explicit declared fmt chunk size, and a data payload. declaredFmtSize must
// equal len(fmtBody) for a well-formed stream; the pad byte after an odd size
// is added automatically. The unbounded data size (0xFFFFFFFF) is used so
// callers need not compute a length.
func wavWithFmtBody(fmtBody []byte, declaredFmtSize uint32, payload []byte) []byte {
	b := []byte(riffMagic)
	b = le32(b, 0xFFFFFFFF)
	b = append(b, waveMagic...)
	b = append(b, fmtChunkID...)
	b = le32(b, declaredFmtSize)
	b = append(b, fmtBody...)
	if declaredFmtSize%2 == 1 {
		b = append(b, 0x00)
	}
	b = append(b, dataChunkID...)
	b = le32(b, 0xFFFFFFFF)
	b = append(b, payload...)
	return b
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

func TestParseWAVHeaderExtensiblePCM16(t *testing.T) {
	// WAVE_FORMAT_EXTENSIBLE wrapping plain PCM16 with the KSDATAFORMAT_SUBTYPE_PCM
	// SubFormat GUID must parse exactly like classic PCM: same rate, same
	// channels, reader left at the first data byte.
	cases := []struct {
		name         string
		channels     uint16
		rate         uint32
		wantChannels int
	}{
		{"mono", 1, 48000, 1},
		{"stereo", 2, 44100, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte("PCMDATA!")
			fmtBody := extensibleFmtBody(tc.channels, tc.rate, 16, 16, pcmSubFormatGUID)
			body := wavWithFmtBody(fmtBody, uint32(len(fmtBody)), payload)
			br := wavReader(body)
			info, err := parseWAVHeader(br)
			if err != nil {
				t.Fatalf("parseWAVHeader: %v", err)
			}
			if info.rate != int(tc.rate) || info.channels != tc.wantChannels {
				t.Fatalf("got rate=%d channels=%d, want %d/%d", info.rate, info.channels, tc.rate, tc.wantChannels)
			}
			got, err := io.ReadAll(br)
			if err != nil {
				t.Fatalf("read after header: %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("reader left at %q, want it positioned at %q", got, payload)
			}
		})
	}
}

func TestParseWAVHeaderExtensibleMultichannel(t *testing.T) {
	// WAVE_FORMAT_EXTENSIBLE PCM16 with a channel count above the mono/stereo
	// cases already covered must still parse correctly. maxChannels (8) is
	// used, since it is both within range and the parser's ceiling.
	const channels = maxChannels
	payload := []byte("PCMDATA!")
	fmtBody := extensibleFmtBody(channels, 48000, 16, 16, pcmSubFormatGUID)
	body := wavWithFmtBody(fmtBody, uint32(len(fmtBody)), payload)
	br := wavReader(body)
	info, err := parseWAVHeader(br)
	if err != nil {
		t.Fatalf("parseWAVHeader: %v", err)
	}
	if info.rate != 48000 || info.channels != channels {
		t.Fatalf("got rate=%d channels=%d, want 48000/%d", info.rate, info.channels, channels)
	}
	got, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("read after header: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("reader left at %q, want it positioned at %q", got, payload)
	}
}

func TestParseWAVHeaderExtensibleTrailingBytesAndPad(t *testing.T) {
	// A fmt chunk larger than the required 40 bytes (trailing bytes past the
	// GUID, plus the pad byte an odd declared size requires) must still be
	// accepted, with the trailing bytes and pad skipped so the data chunk that
	// follows is found correctly.
	fmtBody := extensibleFmtBody(2, 44100, 16, 16, pcmSubFormatGUID)
	fmtBody = append(fmtBody, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE) // 5 trailing bytes, odd total
	payload := []byte("TAIL")
	body := wavWithFmtBody(fmtBody, uint32(len(fmtBody)), payload)

	br := wavReader(body)
	info, err := parseWAVHeader(br)
	if err != nil {
		t.Fatalf("parseWAVHeader: %v", err)
	}
	if info.rate != 44100 || info.channels != 2 {
		t.Fatalf("got %+v, want rate 44100 channels 2", info)
	}
	got, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("read after header: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("reader left at %q, want it positioned at %q", got, payload)
	}
}

func TestParseWAVHeaderExtensibleTrailingBytesTruncated(t *testing.T) {
	// A fmt chunk declaring more than the required 40 bytes (trailing bytes
	// past the 24-byte extension), whose stream is cut partway through those
	// trailing bytes, must be a truncation. This exercises the skip(br, extra)
	// error branch on the extensible path, where consumed is already 40.
	fmtBody := extensibleFmtBody(1, 8000, 16, 16, pcmSubFormatGUID)
	fmtBody = append(fmtBody, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x11, 0x22, 0x33, 0x44) // 10 trailing bytes
	full := wavWithFmtBody(fmtBody, uint32(len(fmtBody)), []byte("DATAPAYLOAD"))
	// Cut partway through the 10 trailing bytes: riff header (12) + chunk
	// header (8) + fmt base (16) + extension (24) + 5 of the 10 trailing bytes.
	cut := riffHeaderSize + chunkHeaderSize + fmtChunkMinSize + fmtExtensibleExtSize + 5
	body := full[:cut]
	_, err := parseWAVHeader(wavReader(body))
	if !errors.Is(err, ErrMalformedWAV) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("parseWAVHeader = %v, want ErrMalformedWAV wrapping io.ErrUnexpectedEOF", err)
	}
}

func TestParseWAVHeaderExtensibleRejections(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		want error
	}{
		{
			"wrong subformat",
			wavWithFmtBody(extensibleFmtBody(1, 8000, 16, 16, nonPCMSubFormatGUID), fmtExtensibleMinSize, []byte("D")),
			ErrUnsupportedFormat,
		},
		{
			"valid bits 24",
			wavWithFmtBody(extensibleFmtBody(1, 8000, 16, 24, pcmSubFormatGUID), fmtExtensibleMinSize, []byte("D")),
			ErrUnsupportedFormat,
		},
		{
			"container bits 24",
			wavWithFmtBody(extensibleFmtBody(1, 8000, 24, 16, pcmSubFormatGUID), fmtExtensibleMinSize, []byte("D")),
			ErrUnsupportedFormat,
		},
		{
			"cbSize too small",
			func() []byte {
				fmtBody := extensibleFmtBody(1, 8000, 16, 16, pcmSubFormatGUID)
				// Overwrite cbSize (bytes 16:18 of the fmt body) with 16, below the
				// required 22.
				binary.LittleEndian.PutUint16(fmtBody[16:18], 16)
				return wavWithFmtBody(fmtBody, fmtExtensibleMinSize, []byte("D"))
			}(),
			ErrUnsupportedFormat,
		},
		{
			"cbSize overruns chunk size",
			func() []byte {
				fmtBody := extensibleFmtBody(1, 8000, 16, 16, pcmSubFormatGUID)
				// Overwrite cbSize (bytes 16:18 of the fmt body) with 100, so
				// 18+cbSize overruns the declared 40-byte chunk size.
				binary.LittleEndian.PutUint16(fmtBody[16:18], 100)
				return wavWithFmtBody(fmtBody, fmtExtensibleMinSize, []byte("D"))
			}(),
			ErrMalformedWAV,
		},
		{
			"fmt chunk smaller than 40",
			wavWithFmtBody(extensibleFmtBody(1, 8000, 16, 16, pcmSubFormatGUID)[:20], 20, []byte("D")),
			ErrMalformedWAV,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseWAVHeader(wavReader(tc.body)); !errors.Is(err, tc.want) {
				t.Fatalf("parseWAVHeader = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestParseWAVHeaderExtensibleTruncatedExtension(t *testing.T) {
	// A fmt chunk that declares the full 40-byte EXTENSIBLE size but whose
	// stream ends partway through the 24-byte extension is a truncation, not a
	// format rejection: it must map to ErrMalformedWAV wrapping
	// io.ErrUnexpectedEOF, matching every other short-read case.
	fmtBody := extensibleFmtBody(1, 8000, 16, 16, pcmSubFormatGUID)
	full := wavWithFmtBody(fmtBody, uint32(len(fmtBody)), []byte("DATAPAYLOAD"))
	// Cut partway through the 24-byte extension: 12 bytes base RIFF+fmt header
	// (RIFF+size+WAVE+fmt +size = 12+8=20) plus the 16-byte base plus 10 of the
	// 24 extension bytes.
	cut := riffHeaderSize + chunkHeaderSize + fmtChunkMinSize + 10
	body := full[:cut]
	_, err := parseWAVHeader(wavReader(body))
	if !errors.Is(err, ErrMalformedWAV) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("parseWAVHeader = %v, want ErrMalformedWAV wrapping io.ErrUnexpectedEOF", err)
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

// TestParseWAVHeaderFmtTrailingBudgetIsGlobal pins the pre-data budget across
// the whole header, not just within readFmtChunk. A JUNK chunk spends a visible
// slice of the 1 MiB budget, then a fmt chunk declares a trailing region that,
// measured on its own, fits under the cap, yet whose true cost once the RIFF
// header and the JUNK chunk are counted exceeds it. The parser must reject it
// with the budget error before it ever tries to read the (deliberately absent)
// trailing bytes. The earlier gap measured the trailing skip against a counter
// local to readFmtChunk, so it waved the skip through and then tripped on EOF
// reading a region the budget should have forbidden.
func TestParseWAVHeaderFmtTrailingBudgetIsGlobal(t *testing.T) {
	const junkPayload = 16 << 10 // 16 KiB of JUNK ahead of fmt.

	// Leave readFmtChunk's own counter (16-byte body + trailing) just under the
	// cap, so its local accounting alone would accept the skip.
	declaredFmtSize := uint32(wavMaxPreData - 4096)

	body := []byte(riffMagic)
	body = le32(body, 0xFFFFFFFF)
	body = append(body, waveMagic...)
	// A JUNK chunk consuming part of the budget before fmt.
	body = append(body, "JUNK"...)
	body = le32(body, junkPayload)
	body = append(body, make([]byte, junkPayload)...)
	// A fmt header declaring a large trailing region, followed by only the
	// 16-byte PCM body: the declared trailing bytes are intentionally absent, so
	// a parser that respects the global budget never reaches for them.
	body = append(body, fmtChunkID...)
	body = le32(body, declaredFmtSize)
	body = le16(body, wavFormatPCM)
	body = le16(body, 1)     // channels
	body = le32(body, 8000)  // sample rate
	body = le32(body, 16000) // byte rate
	body = le16(body, 2)     // block align
	body = le16(body, wavBitsPerSample)

	_, err := parseWAVHeader(wavReader(body))
	if !errors.Is(err, ErrMalformedWAV) {
		t.Fatalf("parseWAVHeader error = %v, want ErrMalformedWAV", err)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("parseWAVHeader reached for the oversized trailing region (got %v); the "+
			"budget must be enforced globally, before the skip", err)
	}
}

// TestParseWAVHeaderSampleRateUpperBound refuses a sample rate above the
// portable maximum, math.MaxInt32. Such a rate narrows to a negative int on a
// 32-bit build (where int is 32-bit) and would flow into buffer-sizing and
// resample math; the exact maximum stays accepted, so the boundary is pinned
// on both sides. The literals below independently encode math.MaxInt32 rather
// than referencing the parser's own constant, so a wrong bound is caught.
func TestParseWAVHeaderSampleRateUpperBound(t *testing.T) {
	tests := []struct {
		name    string
		rate    uint32
		wantErr bool
	}{
		{"max int32 accepted", 1<<31 - 1, false},
		{"one past max int32", 1 << 31, true},
		{"all ones", 0xFFFFFFFF, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			header := stdWAVHeader(wavFormatPCM, 1, tt.rate, 16, 0, 0xFFFFFFFF)
			info, err := parseWAVHeader(wavReader(header))
			switch {
			case tt.wantErr && !errors.Is(err, ErrMalformedWAV):
				t.Fatalf("rate %d: error = %v, want ErrMalformedWAV", tt.rate, err)
			case !tt.wantErr && err != nil:
				t.Fatalf("rate %d: unexpected error %v", tt.rate, err)
			case !tt.wantErr && info.rate != int(tt.rate):
				t.Fatalf("rate %d: info.rate = %d, want %d", tt.rate, info.rate, tt.rate)
			}
		})
	}
}
