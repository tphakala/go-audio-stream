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
	for b := firstByte; b <= lastByte; b++ {
		acc = acc<<8 | uint64(r.buf[b])
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

// extractBits copies n bits from buf starting at bit offset start (MSB
// first) into a freshly allocated byte-aligned slice, left-justifying the
// bits and zero-padding any trailing bits of the final byte. The caller
// guarantees start+n <= len(buf)*8.
func extractBits(buf []byte, start, n int) []byte {
	return extractBitsInto(nil, buf, start, n)
}

// extractBitsInto behaves like extractBits, but reuses dst's backing array
// when it already has enough capacity, instead of always allocating a fresh
// slice. The caller guarantees start+n <= len(buf)*8. Pass dst[:0] (or nil)
// to force a fresh allocation only when dst is too small.
func extractBitsInto(dst, buf []byte, start, n int) []byte {
	size := (n + 7) / 8
	if cap(dst) < size {
		dst = make([]byte, size)
	} else {
		dst = dst[:size]
		for i := range dst {
			dst[i] = 0
		}
	}
	for i := range n {
		bit := start + i
		if buf[bit/8]&(1<<uint(7-bit%8)) == 0 {
			continue
		}
		dst[i/8] |= 1 << uint(7-i%8)
	}
	return dst
}

// appendBits grows dst by n bits worth of bytes (via append, which
// zero-fills a newly grown region on its own) and OR's in n bits from buf
// starting at bit offset start (MSB first), left-justified into that
// freshly appended region. Unlike extractBitsInto, appendBits never
// re-zeros its destination: the region it writes into is always newly
// appended, so it is already zero, and dst's prior content (if any) is left
// untouched. The caller guarantees start+n <= len(buf)*8.
func appendBits(dst, buf []byte, start, n int) []byte {
	size := (n + 7) / 8
	base := len(dst)
	dst = append(dst, make([]byte, size)...)
	for i := range n {
		bit := start + i
		if buf[bit/8]&(1<<uint(7-bit%8)) == 0 {
			continue
		}
		dst[base+i/8] |= 1 << uint(7-i%8)
	}
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
	return parseStreamMuxConfigBits(&bitReader{buf: buf})
}

// parseStreamMuxConfigBits parses a StreamMuxConfig from r's current bit
// position, which need not be byte-aligned: the in-band case starts right
// after the leading useSameStreamMux bit, reading directly from the RTP
// payload buffer. It supports exactly the shape this package targets:
// audioMuxVersion 0, allStreamsSameTimeFraming 1, one program, one layer,
// frameLengthType 0, and a GA-family AudioSpecificConfig; any other shape
// returns ErrUnsupportedMux or ErrUnsupportedASC. It returns ErrTruncated
// when r's buffer ends before a declared field is complete.
func parseStreamMuxConfigBits(r *bitReader) (smc streamMuxConfig, asc []byte, frameLength uint32, err error) {
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

	asc, frameLength, err = parseASC(r)
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
// into a fresh byte-aligned slice, and the per-AU sample count derived from
// frameLengthFlag (1024 or 960).
func parseASC(r *bitReader) (asc []byte, frameLength uint32, err error) {
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

	asc = extractBits(r.buf, start, r.pos-start)
	return asc, frameLength, nil
}
