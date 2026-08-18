package httpsource

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// WAV RIFF constants. Only 16-bit integer PCM is accepted, whether declared as
// classic PCM (audioFormat 1) or wrapped in a WAVE_FORMAT_EXTENSIBLE fmt chunk
// carrying the PCM SubFormat GUID; every other shape is rejected rather than
// delivered.
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
	// wavFormatExtensible is the fmt audioFormat code for WAVE_FORMAT_EXTENSIBLE,
	// the container some devices use to wrap plain integer PCM behind a
	// SubFormat GUID instead of declaring audioFormat 1 directly. It is accepted
	// only when cbSize is at least 22 (carrying the SubFormat GUID), the
	// SubFormat GUID is KSDATAFORMAT_SUBTYPE_PCM, and both the container bits
	// per sample and the valid bits per sample are 16, so this stays
	// byte-identical 16-bit integer PCM once past the header; every other
	// EXTENSIBLE subformat (float, A-law, a compressed codec, ...) is rejected.
	// A chunk smaller than 40 bytes, or whose cbSize overruns the declared
	// chunk size, is ErrMalformedWAV rather than an unsupported format.
	wavFormatExtensible = 0xFFFE
	// wavBitsPerSample is the only sample width this source delivers.
	wavBitsPerSample = 16

	// fmtExtensibleMinSize is the WAVE_FORMAT_EXTENSIBLE fmt body this parser
	// requires: the 16-byte base plus the 24-byte extension (cbSize, the valid
	// bits per sample, the channel mask, and the 16-byte SubFormat GUID).
	fmtExtensibleMinSize = 40
	// fmtExtensibleExtSize is that 24-byte extension on its own, the part read
	// after the 16-byte base.
	fmtExtensibleExtSize = fmtExtensibleMinSize - fmtChunkMinSize
	// fmtExtensibleCbSize is the extension size (cbSize) a WAVE_FORMAT_EXTENSIBLE
	// fmt chunk must declare: wValidBitsPerSample(2) + dwChannelMask(4) +
	// SubFormat GUID(16) = 22 bytes. A smaller cbSize means the chunk does not
	// self-describe as carrying the fields this parser needs to validate.
	fmtExtensibleCbSize = 22

	// wavUnbounded is the data-chunk size a progressive streamer writes when the
	// total length is unknown ahead of time; 0 is treated the same way.
	wavUnbounded = 0xFFFFFFFF

	// wavMaxPreData bounds the bytes a header may consume before the data chunk,
	// so a hostile stream of junk chunks cannot make the parser read forever.
	wavMaxPreData = 1 << 20

	// wavMaxSampleRate bounds the sample rate this parser trusts, mirroring
	// go-wav's riff.MaxSampleRate. It is math.MaxInt32: a rate above it narrows
	// to a negative int on a 32-bit build (where int is 32-bit) and would flow
	// into buffer-sizing and resample math as a negative length, so it is
	// refused as malformed rather than passed on. On a 64-bit build the
	// narrowing is lossless, but the bound is applied on every build for one
	// portable definition of a valid rate.
	wavMaxSampleRate uint32 = 1<<31 - 1
)

// pcmSubFormatGUID is KSDATAFORMAT_SUBTYPE_PCM, the SubFormat GUID a
// WAVE_FORMAT_EXTENSIBLE fmt chunk must carry for this parser to treat the
// stream as plain integer PCM rather than rejecting it. On the wire a GUID is
// data1 (uint32 little-endian), data2 (uint16 little-endian), data3 (uint16
// little-endian), then the 8 remaining bytes (data4) verbatim, so this is not
// a left-to-right copy of the canonical 00000001-0000-0010-8000-00aa00389b71
// text form.
var pcmSubFormatGUID = [16]byte{
	0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00,
	0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71,
}

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
// chunk, either classic PCM (audioFormat 1) or an accepted
// WAVE_FORMAT_EXTENSIBLE PCM16 chunk; see readFmtChunk for the acceptance
// rule. Unknown chunks (LIST, JUNK, fact, ...) are skipped, including the pad
// byte after an odd size. RF64 and BW64 (64-bit RIFF) are reported as
// unsupported; any other structural fault, including a truncation, is
// ErrMalformedWAV.
func (c *Client) parseWAVHeader(br *bufio.Reader) (wavInfo, error) {
	var riff [riffHeaderSize]byte
	if _, err := io.ReadFull(br, riff[:]); err != nil {
		return wavInfo{}, c.malformedWAV(err)
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
			return wavInfo{}, c.malformedWAV(err)
		}
		consumed += chunkHeaderSize
		id := string(hdr[0:4])
		size := binary.LittleEndian.Uint32(hdr[4:8])

		switch id {
		case fmtChunkID:
			read, ferr := c.readFmtChunk(br, size, consumed, &info)
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
				return wavInfo{}, c.malformedWAV(err)
			}
			consumed += skipLen
		}
	}
}

// readFmtChunk reads and validates a fmt chunk of declared size, skipping any
// extension bytes and the pad byte after an odd size, and records the rate and
// channels into info. It returns the total bytes consumed (the 16-byte base,
// plus a WAVE_FORMAT_EXTENSIBLE extension when present, plus any further
// trailing bytes and pad) so the caller can charge them against the pre-data
// budget.
//
// Two audioFormat values are accepted, both requiring 16-bit integer PCM: 1
// (classic PCM, the bitsPerSample field is authoritative) and 0xFFFE
// (WAVE_FORMAT_EXTENSIBLE, accepted only when the fmt chunk is at least 40
// bytes, cbSize is at least 22, the SubFormat GUID is
// KSDATAFORMAT_SUBTYPE_PCM, and both the container and valid bits per sample
// are 16). This keeps the "16-bit integer PCM only" contract intact: an
// EXTENSIBLE chunk is accepted only when it is, byte for byte, the same PCM16
// this parser already delivers. Every other audioFormat, and an EXTENSIBLE
// chunk that fails that gate, is ErrUnsupportedFormat; a fmt chunk too small
// for the format it declares, or a WAVE_FORMAT_EXTENSIBLE cbSize that overruns
// the chunk, is ErrMalformedWAV.
//
// outerConsumed is the pre-data byte total parseWAVHeader has already spent (the
// RIFF header and every chunk before this fmt), so the trailing-skip budget below
// is enforced against the whole header rather than this chunk alone.
func (c *Client) readFmtChunk(br *bufio.Reader, size uint32, outerConsumed int64, info *wavInfo) (int64, error) {
	if size < fmtChunkMinSize {
		return 0, fmt.Errorf("%w: fmt chunk is %d bytes, need at least %d", ErrMalformedWAV, size, fmtChunkMinSize)
	}
	var body [fmtChunkMinSize]byte
	if _, err := io.ReadFull(br, body[:]); err != nil {
		return 0, c.malformedWAV(err)
	}
	consumed := int64(fmtChunkMinSize)

	audioFormat := binary.LittleEndian.Uint16(body[0:2])
	channels := binary.LittleEndian.Uint16(body[2:4])
	sampleRate := binary.LittleEndian.Uint32(body[4:8])
	bits := binary.LittleEndian.Uint16(body[14:16])

	switch audioFormat {
	case wavFormatPCM:
		if bits != wavBitsPerSample {
			return 0, fmt.Errorf("%w: WAV sample width %d bits is not 16", ErrUnsupportedFormat, bits)
		}
	case wavFormatExtensible:
		if size < fmtExtensibleMinSize {
			return 0, fmt.Errorf("%w: WAVE_FORMAT_EXTENSIBLE fmt chunk is %d bytes, need at least %d", ErrMalformedWAV, size, fmtExtensibleMinSize)
		}
		var ext [fmtExtensibleExtSize]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return 0, c.malformedWAV(err)
		}
		consumed += int64(len(ext))

		cbSize := binary.LittleEndian.Uint16(ext[0:2])

		// The declared extension (cbSize bytes, starting after the 16-byte base and
		// the 2-byte cbSize field) must fit within the declared chunk size. A cbSize
		// that overruns the chunk is a malformed header, not merely an unsupported
		// one, so it is ErrMalformedWAV like the size < 40 case above.
		if int64(fmtChunkMinSize)+2+int64(cbSize) > int64(size) {
			return 0, fmt.Errorf("%w: WAVE_FORMAT_EXTENSIBLE cbSize %d overruns the %d-byte fmt chunk", ErrMalformedWAV, cbSize, size)
		}

		validBits := binary.LittleEndian.Uint16(ext[2:4])
		// ext[4:8] is dwChannelMask, not needed to deliver PCM and not validated.
		var subFormat [16]byte
		copy(subFormat[:], ext[8:24])

		switch {
		case cbSize < fmtExtensibleCbSize:
			return 0, fmt.Errorf("%w: WAVE_FORMAT_EXTENSIBLE cbSize %d is smaller than %d", ErrUnsupportedFormat, cbSize, fmtExtensibleCbSize)
		case bits != wavBitsPerSample:
			return 0, fmt.Errorf("%w: WAV sample width %d bits is not 16", ErrUnsupportedFormat, bits)
		case validBits != wavBitsPerSample:
			return 0, fmt.Errorf("%w: WAVE_FORMAT_EXTENSIBLE valid bits per sample %d is not 16", ErrUnsupportedFormat, validBits)
		case subFormat != pcmSubFormatGUID:
			return 0, fmt.Errorf("%w: WAVE_FORMAT_EXTENSIBLE SubFormat is not KSDATAFORMAT_SUBTYPE_PCM", ErrUnsupportedFormat)
		}
	default:
		return 0, fmt.Errorf("%w: WAV audio format %d is not integer PCM", ErrUnsupportedFormat, audioFormat)
	}

	if channels < 1 || channels > maxChannels {
		return 0, fmt.Errorf("%w: WAV channel count %d out of range", ErrUnsupportedFormat, channels)
	}
	if sampleRate < 1 {
		return 0, fmt.Errorf("%w: WAV sample rate is zero", ErrMalformedWAV)
	}
	if sampleRate > wavMaxSampleRate {
		return 0, fmt.Errorf("%w: WAV sample rate %d exceeds the maximum %d", ErrMalformedWAV, sampleRate, wavMaxSampleRate)
	}
	info.rate = int(sampleRate)
	info.channels = int(channels)

	// Any fmt chunk larger than what this parser required to read (a
	// classic-PCM extension it does not need, or bytes trailing a
	// WAVE_FORMAT_EXTENSIBLE SubFormat GUID) is skipped along with the pad byte
	// that word-aligns an odd chunk size.
	if extra := chunkPaddedSize(size) - consumed; extra > 0 {
		// Measure the trailing skip against the whole header, not just the bytes
		// this fmt chunk has consumed, so the pre-data budget stays a global
		// ceiling: outerConsumed already counts the RIFF header and every prior
		// chunk, and consumed counts this fmt chunk's body read so far.
		if outerConsumed+consumed+extra > wavMaxPreData {
			return 0, errWAVHeaderTooLarge()
		}
		if err := skip(br, extra); err != nil {
			return 0, c.malformedWAV(err)
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
func (c *Client) malformedWAV(err error) error {
	// A stall that tripped the open deadline or a caller cancellation mid-header
	// is a transient, retryable open-phase failure, not a malformed stream, so
	// classify it through the open-phase taxonomy (issue #92). A clean short read
	// on a well-formed but truncated stream (or a zero Client in a unit test)
	// falls through to ErrMalformedWAV, preserving the pre-#92 behavior.
	if c.classifyOpenRead != nil {
		if oe := c.classifyOpenRead(err); oe != nil {
			return oe
		}
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: %w", ErrMalformedWAV, io.ErrUnexpectedEOF)
	}
	return fmt.Errorf("%w: %w", ErrMalformedWAV, err)
}

// errWAVHeaderTooLarge reports a header that would exceed the pre-data budget.
func errWAVHeaderTooLarge() error {
	return fmt.Errorf("%w: header exceeds %d bytes before the data chunk", ErrMalformedWAV, wavMaxPreData)
}
