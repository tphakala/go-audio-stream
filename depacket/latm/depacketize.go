package latm

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

// Depacketize processes one RTP payload (one AudioMuxElement) and returns the
// access units it carries, in order: one for the single-subframe case, several
// for a multi-subframe AudioMuxElement. Any malformed input returns a sentinel
// error and never panics. marker and rtpTime are accepted for signature
// parity with the AAC depacketizer; LATM does not fragment across packets, so
// they are unused by the transform (the caller owns the absolute clock).
//
// This package currently covers the single-subframe case, both out-of-band
// (Config.MuxConfigPresent == false, payload starts directly at
// PayloadLengthInfo, byte-aligned) and in-band (Config.MuxConfigPresent ==
// true, payload starts at useSameStreamMux, not byte-aligned). Multi-subframe
// AudioMuxElements return ErrUnsupportedMux until a later task adds them.
func (d *Depacketizer) Depacketize(payload []byte, marker bool, rtpTime uint32) ([]AU, error) {
	_ = marker  // LATM does not fragment across packets; unused by the transform.
	_ = rtpTime // relative offsets only; the caller owns the absolute clock.

	if d.cfg.MuxConfigPresent {
		return d.depacketizeInBand(payload)
	}
	if !d.haveSMC {
		return nil, ErrNoConfig
	}
	if d.smc.numSubFrames > 0 {
		return nil, ErrUnsupportedMux // multi-subframe: a later task.
	}

	data, _, err := readMuxSlot(payload, 0)
	if err != nil {
		return nil, err
	}

	d.aus = append(d.aus[:0], AU{Data: data, RTPOffset: 0})
	return d.aus, nil
}

// depacketizeInBand handles Config.MuxConfigPresent == true: the whole
// AudioMuxElement, starting at useSameStreamMux, is driven through a
// bitReader rather than treated as byte-aligned. It reads useSameStreamMux;
// when 0 it parses and retains the inline StreamMuxConfig, when 1 it
// requires a previously retained one (ErrNoConfig otherwise). It then reads
// the single subframe's PayloadLengthInfo as a bit-level byte sum and
// extracts that many bytes of payload from the current bit position,
// repacked MSB-first into d.inBandData since the bytes are not aligned to
// the input's byte boundaries and cannot alias it directly. Multi-subframe
// AudioMuxElements (numSubFrames > 0) return ErrUnsupportedMux until a
// later task adds them.
func (d *Depacketizer) depacketizeInBand(payload []byte) ([]AU, error) {
	br := &bitReader{buf: payload}

	useSameStreamMux, ok := br.read(1)
	if !ok {
		return nil, ErrTruncated
	}
	if useSameStreamMux == 0 {
		smc, asc, frameLength, err := parseStreamMuxConfigBits(br)
		if err != nil {
			return nil, err
		}
		d.smc = smc
		d.asc = asc
		d.frameLength = frameLength
		d.haveSMC = true
	} else if !d.haveSMC {
		return nil, ErrNoConfig
	}

	if d.smc.numSubFrames > 0 {
		return nil, ErrUnsupportedMux // multi-subframe: a later task.
	}

	length := 0
	for {
		b, ok := br.read(8)
		if !ok {
			return nil, ErrTruncated
		}
		length += int(b)
		if length > MaxMuxSlotBytes {
			return nil, ErrPayloadOverflow
		}
		if b != 0xFF {
			break
		}
	}
	if br.pos+length*8 > len(payload)*8 {
		return nil, ErrPayloadOverflow
	}

	d.inBandData = extractBitsInto(d.inBandData[:0], payload, br.pos, length*8)
	br.pos += length * 8
	br.byteAlign()

	d.aus = append(d.aus[:0], AU{Data: d.inBandData, RTPOffset: 0})
	return d.aus, nil
}
