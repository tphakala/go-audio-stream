package latm

import "fmt"

// readMuxSlot reads one PayloadLengthInfo / PayloadMux pair starting at
// payload[offset]: MuxSlotLengthBytes as a byte sum (each 0xFF byte adds
// 255 and continues the sum; the terminating byte below 0xFF adds its own
// value and ends it), then that many payload bytes. It returns the AU bytes
// (aliasing payload) and the offset immediately after them.
func readMuxSlot(payload []byte, offset int) (data []byte, next int, err error) {
	length := 0
	for {
		if offset >= len(payload) {
			return nil, 0, ErrTruncated
		}
		b := payload[offset]
		offset++
		length += int(b)
		if length > MaxMuxSlotBytes {
			return nil, 0, ErrPayloadOverflow
		}
		if b != 0xFF {
			break
		}
	}
	if offset+length > len(payload) {
		return nil, 0, ErrPayloadOverflow
	}
	return payload[offset : offset+length], offset + length, nil
}

// subFrameLayout validates the retained numSubFrames against MaxSubFrames and
// returns the per-subframe RTP tick increment: Config.SamplesPerFrame when
// nonzero, otherwise the ASC-derived frame length. The out-of-band and in-band
// paths compute this identically once a StreamMuxConfig is in hand, so they
// share it. The cap guard is a structural no-op today (numSubFrames is a 6-bit
// field, so numSubFrames+1 tops out at exactly MaxSubFrames), kept as defensive
// coding against the named constant.
func (d *Depacketizer) subFrameLayout() (frameLenTicks uint32, err error) {
	if d.smc.numSubFrames+1 > MaxSubFrames {
		return 0, fmt.Errorf("%w: numSubFrames %d exceeds cap", ErrUnsupportedMux, d.smc.numSubFrames)
	}
	frameLenTicks = uint32(d.cfg.SamplesPerFrame)
	if frameLenTicks == 0 {
		frameLenTicks = d.frameLength
	}
	return frameLenTicks, nil
}

// Depacketize processes one RTP payload and returns the access units it carries,
// in order. A payload holds one AudioMuxElement in the common case (one access
// unit for the single-subframe case, numSubFrames+1 for a multi-subframe one),
// but RFC 3016 permits more than one AudioMuxElement per payload, and every
// element is now consumed rather than the first alone with the rest silently
// dropped. Each AU's RTPOffset accumulates across the whole payload: within an
// element it is the subframe index times the per-frame RTP tick count
// (Config.SamplesPerFrame when nonzero, otherwise the ASC-derived frame length),
// and each element continues from the tick span of the ones before it. A wholly
// zero trailing remainder is treated as byte-alignment or RTP padding and
// dropped. A malformed leading (first) element returns a sentinel error with no
// access units; a malformed trailing element delivers the complete leading
// elements and stops. More than MaxMuxElements elements returns ErrTooManyElements
// after delivering the leading ones. Any error is a sentinel and never a panic.
//
// A limitation of consuming several elements in one call: an inline config change
// mid-payload (a second in-band element with useSameStreamMux==0 carrying a
// different StreamMuxConfig) is honored for that element's access units, but a
// caller reading AudioSpecificConfig once after Depacketize returns sees only the
// last element's config. A config change mid-RTP-packet is pathological and this
// keeps the callback contract simple.
//
// marker and rtpTime are accepted for signature parity with the AAC depacketizer;
// LATM does not fragment across packets, so they are unused by the transform (the
// caller owns the absolute clock).
//
// This package covers both out-of-band (Config.MuxConfigPresent == false,
// payload starts directly at PayloadLengthInfo, byte-aligned) and in-band
// (Config.MuxConfigPresent == true, payload starts at useSameStreamMux, not
// byte-aligned).
func (d *Depacketizer) Depacketize(payload []byte, marker bool, rtpTime uint32) ([]AU, error) {
	_ = marker  // LATM does not fragment across packets; unused by the transform.
	_ = rtpTime // relative offsets only; the caller owns the absolute clock.

	if d.cfg.MuxConfigPresent {
		return d.depacketizeInBand(payload)
	}
	// Defensive: New always sets haveSMC on out-of-band success, so this is
	// structurally unreachable here, like the numSubFrames cap guard in
	// subFrameLayout.
	if !d.haveSMC {
		return nil, ErrNoConfig
	}
	frameLenTicks, err := d.subFrameLayout()
	if err != nil {
		return nil, err
	}

	// RFC 3016 permits more than one AudioMuxElement per RTP payload. The common
	// case is exactly one, exiting at the first `offset >= len(payload)` below
	// before the padding scan or the tick accumulator ever run, so the
	// single-element hot path is unchanged. elemBaseTicks continues each element's
	// RTPOffset from the end of the previous one (out-of-band shares one fixed
	// StreamMuxConfig, so frameLenTicks is constant across elements).
	d.aus = d.aus[:0]
	offset := 0
	var elemBaseTicks uint32
	for elem := 0; ; elem++ {
		if elem >= MaxMuxElements {
			// The leading elements are valid audio; deliver them and stop rather
			// than dropping the whole packet or looping unbounded on a crafted one.
			return d.aus, ErrTooManyElements
		}
		auMark := len(d.aus)
		for i := 0; i <= d.smc.numSubFrames; i++ {
			data, next, rerr := readMuxSlot(payload, offset)
			if rerr != nil {
				if elem == 0 {
					return nil, rerr
				}
				// A malformed trailing element: roll back its partial access units
				// and deliver the complete leading elements (better for a real-time
				// stream than dropping everything already parsed).
				d.aus = d.aus[:auMark]
				return d.aus, nil
			}
			offset = next
			d.aus = append(d.aus, AU{Data: data, RTPOffset: elemBaseTicks + uint32(i)*frameLenTicks})
		}
		elemBaseTicks += uint32(d.smc.numSubFrames+1) * frameLenTicks

		if offset >= len(payload) {
			break // payload exhausted: the common single-element exit
		}
		if allZero(payload[offset:]) {
			break // trailing byte-alignment / RTP padding, not another element
		}
	}
	return d.aus, nil
}

// allZero reports whether every byte of b is zero. A wholly-zero remainder after
// a complete AudioMuxElement is treated as byte-alignment or RTP padding and
// dropped rather than parsed as another element. A trailing element consisting
// only of zero-length access units is therefore indistinguishable from padding
// and is likewise dropped, which is lossless since a zero-length access unit
// carries no audio.
func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

// depacketizeInBand handles Config.MuxConfigPresent == true: the whole
// AudioMuxElement, starting at useSameStreamMux, is driven through a
// bitReader rather than treated as byte-aligned. It reads useSameStreamMux;
// when 0 it parses and retains the inline StreamMuxConfig, when 1 it
// requires a previously retained one (ErrNoConfig otherwise). It then reads
// numSubFrames+1 PayloadLengthInfo/payload pairs as bit-level byte sums,
// repacking each subframe's payload bytes MSB-first into a distinct region
// of d.inBandData: the bytes are not aligned to the input's byte boundaries
// and cannot alias it directly, and every AU returned from one call must
// stay simultaneously valid, so each subframe is appended onto the shared
// buffer rather than overwriting the previous one.
func (d *Depacketizer) depacketizeInBand(payload []byte) ([]AU, error) {
	br := &bitReader{buf: payload}

	// One RTP payload may carry more than one AudioMuxElement (RFC 3016). The
	// common case is exactly one, breaking at the first `br.pos >= len` below
	// before the padding scan runs, so the single-element hot path is unchanged.
	// d.aus and d.inBandData are reset once and appended across every element;
	// elemBaseTicks continues each element's RTPOffset from the previous one, and
	// frameLenTicks is recomputed per element since an inline config may change it.
	d.aus = d.aus[:0]
	d.inBandData = d.inBandData[:0]
	var elemBaseTicks uint32
	for elem := 0; ; elem++ {
		if elem >= MaxMuxElements {
			return d.aus, ErrTooManyElements
		}
		auMark := len(d.aus)
		span, err := d.depacketizeInBandElement(br, elemBaseTicks)
		if err != nil {
			if elem == 0 {
				return nil, err
			}
			// A malformed trailing element: roll back its partial access units
			// (d.inBandData scratch need not shrink; it is reset next call and the
			// abandoned bytes are unreferenced) and deliver the leading elements.
			d.aus = d.aus[:auMark]
			return d.aus, nil
		}
		elemBaseTicks += span

		if br.pos >= len(payload)*8 {
			break // payload exhausted: the common single-element exit
		}
		if allZero(payload[br.pos/8:]) {
			break // trailing byte-alignment / RTP padding, not another element
		}
	}
	return d.aus, nil
}

// depacketizeInBandElement parses one in-band AudioMuxElement from br, appending
// its access units to d.aus (backed by d.inBandData) with each RTPOffset based at
// baseTicks, and returns this element's total RTP tick span for the next
// element's base. It reads the leading useSameStreamMux bit and, when 0, the
// inline StreamMuxConfig (retained); useSameStreamMux==1 requires a previously
// retained config. It leaves br byte-aligned at the element's end. On any error
// it returns the sentinel and leaves the partial appends for the caller to roll
// back; br is left wherever the failure occurred.
func (d *Depacketizer) depacketizeInBandElement(br *bitReader, baseTicks uint32) (spanTicks uint32, err error) {
	useSameStreamMux, ok := br.read(1)
	if !ok {
		return 0, ErrTruncated
	}
	if useSameStreamMux == 0 {
		smc, asc, frameLength, perr := parseStreamMuxConfigBits(br, d.ascBuf)
		if perr != nil {
			return 0, perr
		}
		d.smc = smc
		// asc was packed into d.ascBuf's backing array. Recycle the previously
		// active buffer as the next parse's scratch and promote asc to active, so
		// the two never share a backing array. A parse that fails above leaves d.asc
		// untouched, so the in-place mutation extractBitsInto performed on the
		// scratch buffer cannot corrupt the retained config.
		d.ascBuf = d.asc
		d.asc = asc
		d.frameLength = frameLength
		d.haveSMC = true
	} else if !d.haveSMC {
		return 0, ErrNoConfig
	}

	frameLenTicks, err := d.subFrameLayout()
	if err != nil {
		return 0, err
	}

	for i := 0; i <= d.smc.numSubFrames; i++ {
		length := 0
		for {
			b, ok := br.read(8)
			if !ok {
				return 0, ErrTruncated
			}
			length += int(b)
			if length > MaxMuxSlotBytes {
				return 0, ErrPayloadOverflow
			}
			if b != 0xFF {
				break
			}
		}
		if br.pos+length*8 > len(br.buf)*8 {
			return 0, ErrPayloadOverflow
		}

		start := len(d.inBandData)
		d.inBandData = appendBits(d.inBandData, br.buf, br.pos, length*8)
		data := d.inBandData[start:]
		br.pos += length * 8

		d.aus = append(d.aus, AU{Data: data, RTPOffset: baseTicks + uint32(i)*frameLenTicks})
	}
	br.byteAlign()

	return uint32(d.smc.numSubFrames+1) * frameLenTicks, nil
}
