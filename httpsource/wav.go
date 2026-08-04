package httpsource

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// WAV RIFF constants. Only 16-bit integer PCM is accepted; every other shape is
// rejected rather than delivered.
const (
	riffMagic   = "RIFF"
	waveMagic   = "WAVE"
	fmtChunkID  = "fmt "
	dataChunkID = "data"
	// rf64Magic and bw64Magic are the 64-bit RIFF form types, rejected as
	// unsupported wherever a WAVE body is recognized.
	rf64Magic = "RF64"
	bw64Magic = "BW64"

	// riffHeaderSize is the leading "RIFF" <size> "WAVE" block.
	riffHeaderSize = 12
	// chunkHeaderSize is a chunk id plus its uint32 little-endian size.
	chunkHeaderSize = 8
	// fmtChunkMinSize is the PCM fmt body this parser reads and requires.
	fmtChunkMinSize = 16

	// wavFormatPCM is the fmt audioFormat code for integer PCM.
	wavFormatPCM = 1
	// wavBitsPerSample is the only sample width this source delivers.
	wavBitsPerSample = 16

	// wavUnbounded is the data-chunk size a progressive streamer writes when the
	// total length is unknown ahead of time; 0 is treated the same way.
	wavUnbounded = 0xFFFFFFFF

	// wavMaxPreData bounds the bytes a header may consume before the data chunk,
	// so a hostile stream of junk chunks cannot make the parser read forever.
	wavMaxPreData = 1 << 20
)

// wavInfo is the geometry parseWAVHeader resolves from a WAV stream. dataSize
// is meaningful only when bounded; an unbounded data chunk streams until EOF.
type wavInfo struct {
	rate     int
	channels int
	dataSize uint32
	bounded  bool
}

// parseWAVHeader reads the RIFF header and chunk sequence from br, stopping at
// the data chunk with br positioned at the first PCM byte. It requires a
// RIFF/WAVE container carrying a 16-bit integer PCM fmt chunk before the data
// chunk. Unknown chunks (LIST, JUNK, fact, ...) are skipped, including the pad
// byte after an odd size. RF64 and BW64 (64-bit RIFF) are reported as
// unsupported; any other structural fault, including a truncation, is
// ErrMalformedWAV.
func parseWAVHeader(br *bufio.Reader) (wavInfo, error) {
	var riff [riffHeaderSize]byte
	if _, err := io.ReadFull(br, riff[:]); err != nil {
		return wavInfo{}, malformedWAV(err)
	}
	switch string(riff[0:4]) {
	case riffMagic:
		// ok
	case rf64Magic, bw64Magic:
		return wavInfo{}, fmt.Errorf("%w: %s (64-bit RIFF) is not supported", ErrUnsupportedFormat, string(riff[0:4]))
	default:
		return wavInfo{}, fmt.Errorf("%w: not a RIFF stream", ErrMalformedWAV)
	}
	if string(riff[8:12]) != waveMagic {
		return wavInfo{}, fmt.Errorf("%w: RIFF form is not WAVE", ErrMalformedWAV)
	}

	var info wavInfo
	haveFmt := false
	consumed := int64(riffHeaderSize)
	for {
		// Account for the chunk header about to be read, so the budget bounds the
		// bytes actually consumed rather than permitting one more header once
		// consumed has already reached the cap.
		if consumed+chunkHeaderSize > wavMaxPreData {
			return wavInfo{}, errWAVHeaderTooLarge()
		}
		var hdr [chunkHeaderSize]byte
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return wavInfo{}, malformedWAV(err)
		}
		consumed += chunkHeaderSize
		id := string(hdr[0:4])
		size := binary.LittleEndian.Uint32(hdr[4:8])

		switch id {
		case fmtChunkID:
			read, ferr := readFmtChunk(br, size, &info)
			if ferr != nil {
				return wavInfo{}, ferr
			}
			consumed += read
			haveFmt = true
		case dataChunkID:
			if !haveFmt {
				return wavInfo{}, fmt.Errorf("%w: data chunk before fmt chunk", ErrMalformedWAV)
			}
			if size == 0 || size == wavUnbounded {
				info.bounded = false
				info.dataSize = 0
			} else {
				info.bounded = true
				info.dataSize = size
			}
			return info, nil
		default:
			skipLen := chunkPaddedSize(size)
			if consumed+skipLen > wavMaxPreData {
				return wavInfo{}, errWAVHeaderTooLarge()
			}
			if err := skip(br, skipLen); err != nil {
				return wavInfo{}, malformedWAV(err)
			}
			consumed += skipLen
		}
	}
}

// readFmtChunk reads and validates a PCM fmt chunk of declared size, skipping
// any extension bytes and the pad byte after an odd size, and records the rate
// and channels into info. It returns the total bytes consumed (the 16-byte body
// plus any extension and pad) so the caller can charge them against the
// pre-data budget.
func readFmtChunk(br *bufio.Reader, size uint32, info *wavInfo) (int64, error) {
	if size < fmtChunkMinSize {
		return 0, fmt.Errorf("%w: fmt chunk is %d bytes, need at least %d", ErrMalformedWAV, size, fmtChunkMinSize)
	}
	var body [fmtChunkMinSize]byte
	if _, err := io.ReadFull(br, body[:]); err != nil {
		return 0, malformedWAV(err)
	}
	audioFormat := binary.LittleEndian.Uint16(body[0:2])
	channels := binary.LittleEndian.Uint16(body[2:4])
	sampleRate := binary.LittleEndian.Uint32(body[4:8])
	bits := binary.LittleEndian.Uint16(body[14:16])

	if audioFormat != wavFormatPCM {
		return 0, fmt.Errorf("%w: WAV audio format %d is not integer PCM", ErrUnsupportedFormat, audioFormat)
	}
	if bits != wavBitsPerSample {
		return 0, fmt.Errorf("%w: WAV sample width %d bits is not 16", ErrUnsupportedFormat, bits)
	}
	if channels < 1 || channels > maxChannels {
		return 0, fmt.Errorf("%w: WAV channel count %d out of range", ErrUnsupportedFormat, channels)
	}
	if sampleRate < 1 {
		return 0, fmt.Errorf("%w: WAV sample rate is zero", ErrMalformedWAV)
	}
	info.rate = int(sampleRate)
	info.channels = int(channels)

	consumed := int64(fmtChunkMinSize)
	// A fmt chunk larger than 16 bytes carries an extension (WAVE_FORMAT_EX or
	// EXTENSIBLE fields) this integer-PCM parser does not need; skip it and the
	// pad byte that word-aligns an odd chunk.
	if extra := chunkPaddedSize(size) - fmtChunkMinSize; extra > 0 {
		if consumed+extra > wavMaxPreData {
			return 0, errWAVHeaderTooLarge()
		}
		if err := skip(br, extra); err != nil {
			return 0, malformedWAV(err)
		}
		consumed += extra
	}
	return consumed, nil
}

// chunkPaddedSize is a chunk's body size plus the pad byte that word-aligns an
// odd size, as an int64 so a 0xFFFFFFFF size cannot overflow the addition.
func chunkPaddedSize(size uint32) int64 {
	n := int64(size)
	if size%2 == 1 {
		n++
	}
	return n
}

// skip discards n bytes from br, mapping a short read to io.ErrUnexpectedEOF so
// the caller reports a truncation. n is bounded by the pre-data budget, so it
// fits an int.
func skip(br *bufio.Reader, n int64) error {
	discarded, err := br.Discard(int(n))
	if err != nil {
		return err
	}
	if int64(discarded) < n {
		return io.ErrUnexpectedEOF
	}
	return nil
}

// malformedWAV wraps a read error as ErrMalformedWAV, normalizing any EOF-class
// error to io.ErrUnexpectedEOF so a caller can match the truncation with
// errors.Is(err, io.ErrUnexpectedEOF) regardless of which read hit the end.
func malformedWAV(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: %w", ErrMalformedWAV, io.ErrUnexpectedEOF)
	}
	return fmt.Errorf("%w: %w", ErrMalformedWAV, err)
}

// errWAVHeaderTooLarge reports a header that would exceed the pre-data budget.
func errWAVHeaderTooLarge() error {
	return fmt.Errorf("%w: header exceeds %d bytes before the data chunk", ErrMalformedWAV, wavMaxPreData)
}
