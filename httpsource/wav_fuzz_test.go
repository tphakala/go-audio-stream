package httpsource

import "testing"

// FuzzParseWAVHeader drives the streaming WAV header parser with arbitrary
// bytes. It must never panic, and it must always return either a non-nil
// error or a nil error paired with a wavInfo whose channels and rate are in
// range; the parser's format-specific acceptance and rejection rules already
// have dedicated tests, so this is a totality check, not a behavioral one.
func FuzzParseWAVHeader(f *testing.F) {
	// A valid classic-PCM WAV header.
	f.Add(stdWAVHeader(wavFormatPCM, 2, 44100, 16, 32, 0xFFFFFFFF))
	// A valid WAVE_FORMAT_EXTENSIBLE PCM16 header.
	f.Add(wavWithFmtBody(
		extensibleFmtBody(2, 48000, 16, 16, pcmSubFormatGUID),
		fmtExtensibleMinSize,
		[]byte("PCMDATA!"),
	))
	// A WAVE_FORMAT_EXTENSIBLE header with trailing bytes past the 24-byte
	// extension.
	f.Add(func() []byte {
		fmtBody := extensibleFmtBody(1, 8000, 16, 16, pcmSubFormatGUID)
		fmtBody = append(fmtBody, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE)
		return wavWithFmtBody(fmtBody, uint32(len(fmtBody)), []byte("TAIL"))
	}())
	// A truncated header, cut mid fmt chunk.
	f.Add(stdWAVHeader(wavFormatPCM, 1, 8000, 16, 16, 0xFFFFFFFF)[:20])
	// Pure junk.
	f.Add([]byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07})
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, data []byte) {
		info, err := parseWAVHeader(wavReader(data))
		if err != nil {
			return // a typed error is fine; the contract is no panic
		}
		if info.channels < 1 || info.channels > maxChannels {
			t.Fatalf("channels %d out of range with nil error", info.channels)
		}
		if info.rate < 1 {
			t.Fatalf("rate %d out of range with nil error", info.rate)
		}
	})
}
