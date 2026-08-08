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

// extractBits copies n bits from buf starting at bit offset start (MSB
// first) into a freshly allocated byte-aligned slice, left-justifying the
// bits and zero-padding any trailing bits of the final byte. The caller
// guarantees start+n <= len(buf)*8.
func extractBits(buf []byte, start, n int) []byte {
	out := make([]byte, (n+7)/8)
	for i := range n {
		bit := start + i
		if buf[bit/8]&(1<<uint(7-bit%8)) == 0 {
			continue
		}
		out[i/8] |= 1 << uint(7-i%8)
	}
	return out
}

// streamMuxConfig holds the mux parameters this package retains from a
// parsed StreamMuxConfig. audioMuxVersion, allStreamsSameTimeFraming,
// numProgram, and numLayer are validated during parse and not retained,
// since this package requires them fixed at 0, 1, 0, and 0 respectively.
type streamMuxConfig struct {
	numSubFrames    int
	frameLengthType int
}

// parseStreamMuxConfig parses a StreamMuxConfig bitstream (the SDP config=
// bytes out-of-band, or the inline bits of an in-band AudioMuxElement). It
// supports exactly the shape this package targets: audioMuxVersion 0,
// allStreamsSameTimeFraming 1, one program, one layer, frameLengthType 0,
// and a GA-family AudioSpecificConfig; any other shape returns
// ErrUnsupportedMux or ErrUnsupportedASC. It returns ErrTruncated when buf
// ends before a declared field is complete.
//
// buf is capped at MaxStreamMuxConfigBytes bytes before parsing, so an
// escape-coded field (otherDataPresent) cannot walk an unbounded input.
func parseStreamMuxConfig(buf []byte) (smc streamMuxConfig, asc []byte, frameLength uint32, err error) {
	if len(buf) > MaxStreamMuxConfigBytes {
		buf = buf[:MaxStreamMuxConfigBytes]
	}
	r := &bitReader{buf: buf}

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

	asc, frameLength, err = parseASC(buf, r)
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
// escape codes (31 and 15), but this minimal parse does not interpret the
// SBR/PS extension config that some object types carry beyond the plain GA
// fields; those still decode correctly here since the extension bits are
// not read, matching the object types this package targets (AAC-LC and its
// GA siblings, whose extensionFlag is 0). It returns the ASC bits repacked
// MSB-first into a fresh byte-aligned slice, and the per-AU sample count
// derived from frameLengthFlag (1024 or 960).
func parseASC(buf []byte, r *bitReader) (asc []byte, frameLength uint32, err error) {
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

	if _, ok := r.read(4); !ok { // channelConfiguration.
		return nil, 0, ErrTruncated
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

	if _, ok := r.read(1); !ok { // extensionFlag.
		return nil, 0, ErrTruncated
	}

	frameLength = 1024
	if frameLengthFlag == 1 {
		frameLength = 960
	}

	asc = extractBits(buf, start, r.pos-start)
	return asc, frameLength, nil
}
