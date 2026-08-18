package latm

import "fmt"

// bitReader reads bits MSB-first from a byte slice, tracking a bit
// position. It is modeled on the depacket/aac AU-header bit reader: a read
// loads the whole bytes a field spans into a uint64 accumulator instead of
// indexing one bit at a time, then shifts and masks the field out.
type bitReader struct {
	buf []byte
	pos int // next bit to read, counted from the start of buf
}

// read reads the next n bits (0 <= n <= 32) MSB-first and returns them as
// the low n bits of v. ok is false when fewer than n bits remain, in which
// case the reader's position is left unchanged and the caller should treat
// the input as truncated.
func (r *bitReader) read(n int) (v uint64, ok bool) {
	if n == 0 {
		return 0, true
	}
	if r.pos+n > len(r.buf)*8 {
		return 0, false
	}
	firstByte := r.pos / 8
	lastByte := (r.pos + n - 1) / 8
	var acc uint64
	// Range over the exact byte span so the compiler elides the per-iteration
	// bounds check; lastByte < len(r.buf) by the guard above. An index loop, or
	// a pre-loop `_ = r.buf[lastByte]` hint, does not eliminate it.
	for _, x := range r.buf[firstByte : lastByte+1] {
		acc = acc<<8 | uint64(x)
	}
	rightPad := (lastByte+1)*8 - (r.pos + n)
	v = (acc >> uint(rightPad)) & ((uint64(1) << uint(n)) - 1)
	r.pos += n
	return v, true
}

// byteAlign advances r's bit position to the next byte boundary, discarding
// any unread bits of the current byte. It is a no-op when r is already
// byte-aligned. Used at the end of an in-band AudioMuxElement, where the
// bitstream is not byte-aligned to begin with.
func (r *bitReader) byteAlign() {
	if rem := r.pos % 8; rem != 0 {
		r.pos += 8 - rem
	}
}

// blitBits copies n bits from buf starting at bit offset start (MSB-first)
// into dst[0:len(dst)], left-justified, zero-padding the trailing bits of the
// final byte. len(dst) must equal (n+7)/8. It works one destination byte at a
// time: each output byte is the source window [start+8j, start+8j+8) shifted
// into place (a plain copy when the source is byte-aligned), rather than
// testing one source bit at a time, so it touches roughly 8x fewer bytes on a
// large in-band AU. The caller guarantees start+n <= len(buf)*8.
func blitBits(dst, buf []byte, start, n int) {
	if n == 0 {
		return
	}
	sb := start / 8
	shift := start % 8
	size := len(dst)
	if shift == 0 {
		copy(dst, buf[sb:sb+size])
	} else {
		for j := range size {
			hi := buf[sb+j] << shift
			var lo byte
			if sb+j+1 < len(buf) {
				lo = buf[sb+j+1] >> (8 - shift)
			}
			dst[j] = hi | lo
		}
	}
	// Zero-pad the bits after the field within the final byte. Direct
	// assignment above already set every byte, so no separate clear is needed.
	if rem := n & 7; rem != 0 {
		dst[size-1] &= ^byte(0xFF >> rem)
	}
}

// extractBitsInto copies n bits from buf starting at bit offset start (MSB
// first) into a byte-aligned slice via blitBits, left-justifying the bits and
// zero-padding any trailing bits of the final byte. It reuses dst's backing
// array when that already has enough capacity; pass nil to always allocate a
// fresh slice. The caller guarantees start+n <= len(buf)*8.
func extractBitsInto(dst, buf []byte, start, n int) []byte {
	size := (n + 7) / 8
	if cap(dst) < size {
		dst = make([]byte, size)
	} else {
		dst = dst[:size]
	}
	blitBits(dst, buf, start, n)
	return dst
}

// appendBits grows dst by n bits worth of bytes (via append) and writes n bits
// from buf starting at bit offset start (MSB first) into that freshly appended
// region via blitBits, left-justified. dst's prior content is left untouched.
// The caller guarantees start+n <= len(buf)*8.
func appendBits(dst, buf []byte, start, n int) []byte {
	size := (n + 7) / 8
	base := len(dst)
	dst = append(dst, make([]byte, size)...)
	blitBits(dst[base:base+size], buf, start, n)
	return dst
}

// streamMuxConfig holds the mux parameters this package retains from a
// parsed StreamMuxConfig. audioMuxVersion, allStreamsSameTimeFraming,
// numProgram, and numLayer are validated during parse and not retained,
// since this package requires them fixed at 0, 1, 0, and 0 respectively.
type streamMuxConfig struct {
	numSubFrames    int
	frameLengthType int
}

// parseStreamMuxConfig parses an out-of-band StreamMuxConfig (the SDP
// config= bytes): a byte-aligned bitstream starting at audioMuxVersion. It
// wraps parseStreamMuxConfigBits with a fresh bitReader positioned at bit 0,
// after capping buf at MaxStreamMuxConfigBytes so a malformed escape-coded
// field cannot walk an unbounded input.
func parseStreamMuxConfig(buf []byte) (smc streamMuxConfig, asc []byte, frameLength uint32, err error) {
	if len(buf) > MaxStreamMuxConfigBytes {
		buf = buf[:MaxStreamMuxConfigBytes]
	}
	// The out-of-band ASC is parsed once at New and must stay stable, so it
	// takes a freshly allocated buffer (nil scratch) rather than a reused one.
	return parseStreamMuxConfigBits(&bitReader{buf: buf}, nil)
}

// parseStreamMuxConfigBits parses a StreamMuxConfig from r's current bit
// position, which need not be byte-aligned: the in-band case starts right
// after the leading useSameStreamMux bit, reading directly from the RTP
// payload buffer. It supports exactly the shape this package targets:
// audioMuxVersion 0, allStreamsSameTimeFraming 1, one program, one layer,
// frameLengthType 0, and a GA-family AudioSpecificConfig; any other shape
// returns ErrUnsupportedMux or ErrUnsupportedASC. It returns ErrTruncated
// when r's buffer ends before a declared field is complete.
//
// ascDst is an optional scratch buffer the extracted ASC is packed into,
// reused when it already has enough capacity; pass nil to always allocate.
func parseStreamMuxConfigBits(r *bitReader, ascDst []byte) (smc streamMuxConfig, asc []byte, frameLength uint32, err error) {
	audioMuxVersion, ok := r.read(1)
	if !ok {
		return streamMuxConfig{}, nil, 0, ErrTruncated
	}
	if audioMuxVersion != 0 {
		return streamMuxConfig{}, nil, 0, fmt.Errorf("%w: audioMuxVersion %d", ErrUnsupportedMux, audioMuxVersion)
	}

	allStreamsSameTimeFraming, ok := r.read(1)
	if !ok {
		return streamMuxConfig{}, nil, 0, ErrTruncated
	}
	if allStreamsSameTimeFraming != 1 {
		return streamMuxConfig{}, nil, 0, fmt.Errorf("%w: allStreamsSameTimeFraming %d", ErrUnsupportedMux, allStreamsSameTimeFraming)
	}

	numSubFrames, ok := r.read(6)
	if !ok {
		return streamMuxConfig{}, nil, 0, ErrTruncated
	}
	// numSubFrames is a 6-bit field, so numSubFrames+1 tops out at 64,
	// exactly MaxSubFrames: this guard is currently a structural no-op. It
	// stays as defensive coding against the named constant, so a future
	// widening of the field (or a lowered MaxSubFrames) stays enforced.
	if numSubFrames+1 > MaxSubFrames {
		return streamMuxConfig{}, nil, 0, fmt.Errorf("%w: numSubFrames %d exceeds cap", ErrUnsupportedMux, numSubFrames)
	}

	numProgram, ok := r.read(4)
	if !ok {
		return streamMuxConfig{}, nil, 0, ErrTruncated
	}
	if numProgram != 0 {
		return streamMuxConfig{}, nil, 0, fmt.Errorf("%w: numProgram %d", ErrUnsupportedMux, numProgram)
	}

	numLayer, ok := r.read(3)
	if !ok {
		return streamMuxConfig{}, nil, 0, ErrTruncated
	}
	if numLayer != 0 {
		return streamMuxConfig{}, nil, 0, fmt.Errorf("%w: numLayer %d", ErrUnsupportedMux, numLayer)
	}

	asc, frameLength, err = parseASC(r, ascDst)
	if err != nil {
		return streamMuxConfig{}, nil, 0, err
	}

	frameLengthType, ok := r.read(3)
	if !ok {
		return streamMuxConfig{}, nil, 0, ErrTruncated
	}
	if frameLengthType != 0 {
		return streamMuxConfig{}, nil, 0, fmt.Errorf("%w: frameLengthType %d", ErrUnsupportedMux, frameLengthType)
	}

	if _, ok := r.read(8); !ok { // latmBufferFullness, not retained.
		return streamMuxConfig{}, nil, 0, ErrTruncated
	}

	otherDataPresent, ok := r.read(1)
	if !ok {
		return streamMuxConfig{}, nil, 0, ErrTruncated
	}
	if otherDataPresent == 1 {
		for {
			esc, ok := r.read(1)
			if !ok {
				return streamMuxConfig{}, nil, 0, ErrTruncated
			}
			if _, ok := r.read(8); !ok { // otherDataLenTmp, not retained.
				return streamMuxConfig{}, nil, 0, ErrTruncated
			}
			if esc == 0 {
				break
			}
		}
	}

	crcCheckPresent, ok := r.read(1)
	if !ok {
		return streamMuxConfig{}, nil, 0, ErrTruncated
	}
	if crcCheckPresent == 1 {
		if _, ok := r.read(8); !ok { // crcCheckSum, not retained.
			return streamMuxConfig{}, nil, 0, ErrTruncated
		}
	}

	smc = streamMuxConfig{numSubFrames: int(numSubFrames), frameLengthType: int(frameLengthType)}
	return smc, asc, frameLength, nil
}

// parseASC reads an AudioSpecificConfig from r starting at its current bit
// position, for the GA object types this package supports (1, 2, 3, 4, 6,
// 7). audioObjectType and samplingFrequencyIndex read their respective
// escape codes (31 and 15). This minimal parse reads only the plain GA fields
// that AAC-LC and its GA siblings carry, whose extensionFlag is 0; an ASC that
// sets extensionFlag (additional GASpecificConfig bits this parse does not
// consume) is rejected with ErrUnsupportedASC rather than mis-parsed, matching
// the audioObjectType rejection. It returns the ASC bits repacked MSB-first
// into a byte-aligned slice (ascDst when it has capacity, else a fresh
// allocation), and the per-AU sample count derived from frameLengthFlag (1024
// or 960).
func parseASC(r *bitReader, ascDst []byte) (asc []byte, frameLength uint32, err error) {
	start := r.pos

	aot, ok := r.read(5)
	if !ok {
		return nil, 0, ErrTruncated
	}
	if aot == 31 {
		escAOT, ok := r.read(6)
		if !ok {
			return nil, 0, ErrTruncated
		}
		aot = 32 + escAOT
	}
	switch aot {
	case 1, 2, 3, 4, 6, 7:
		// GA object types this parse supports.
	default:
		return nil, 0, fmt.Errorf("%w: audioObjectType %d", ErrUnsupportedASC, aot)
	}

	sfi, ok := r.read(4)
	if !ok {
		return nil, 0, ErrTruncated
	}
	if sfi == 15 {
		if _, ok := r.read(24); !ok { // explicit samplingFrequency.
			return nil, 0, ErrTruncated
		}
	}

	channelConfiguration, ok := r.read(4)
	if !ok {
		return nil, 0, ErrTruncated
	}
	if channelConfiguration == 0 {
		// ISO 14496-3: channelConfiguration == 0 means the channel layout is
		// carried in a program_config_element() that GASpecificConfig parses
		// immediately after extensionFlag. This minimal parse does not decode
		// the PCE, so reading on would interpret its bits as frameLengthType
		// and the following mux fields. Reject it, matching the AOT 5/29 and
		// extensionFlag rejections.
		return nil, 0, fmt.Errorf("%w: channelConfiguration 0 (program_config_element)", ErrUnsupportedASC)
	}

	frameLengthFlag, ok := r.read(1)
	if !ok {
		return nil, 0, ErrTruncated
	}

	dependsOnCoreCoder, ok := r.read(1)
	if !ok {
		return nil, 0, ErrTruncated
	}
	if dependsOnCoreCoder == 1 {
		if _, ok := r.read(14); !ok { // coreCoderDelay.
			return nil, 0, ErrTruncated
		}
	}

	extensionFlag, ok := r.read(1)
	if !ok {
		return nil, 0, ErrTruncated
	}
	if extensionFlag == 1 {
		// ISO 14496-3: extensionFlag == 1 means additional GASpecificConfig
		// bits follow (for the AAC-scalable / ER object types, or the AOT-22
		// extension) that this minimal parse does not consume. Reading on would
		// leave the extracted ASC short and misalign the following
		// frameLengthType read, so reject it outright, matching the AOT 5/29
		// rejection philosophy above.
		return nil, 0, fmt.Errorf("%w: extensionFlag set", ErrUnsupportedASC)
	}

	frameLength = 1024
	if frameLengthFlag == 1 {
		frameLength = 960
	}

	asc = extractBitsInto(ascDst, r.buf, start, r.pos-start)
	return asc, frameLength, nil
}
