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

// Depacketize processes one RTP payload (one AudioMuxElement) and returns the
// access units it carries, in order: one for the single-subframe case,
// numSubFrames+1 for a multi-subframe AudioMuxElement. Each AU's RTPOffset is
// i times the per-frame RTP tick count, i being the AU's subframe index:
// Config.SamplesPerFrame when nonzero, otherwise the frame length derived
// from the ASC's frameLengthFlag. Any malformed input returns a sentinel
// error and never panics. marker and rtpTime are accepted for signature
// parity with the AAC depacketizer; LATM does not fragment across packets, so
// they are unused by the transform (the caller owns the absolute clock).
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
	// structurally unreachable here, like the numSubFrames cap guard below.
	if !d.haveSMC {
		return nil, ErrNoConfig
	}
	if d.smc.numSubFrames+1 > MaxSubFrames {
		return nil, fmt.Errorf("%w: numSubFrames %d exceeds cap", ErrUnsupportedMux, d.smc.numSubFrames)
	}

	frameLenTicks := uint32(d.cfg.SamplesPerFrame)
	if frameLenTicks == 0 {
		frameLenTicks = d.frameLength
	}

	d.aus = d.aus[:0]
	offset := 0
	for i := 0; i <= d.smc.numSubFrames; i++ {
		data, next, err := readMuxSlot(payload, offset)
		if err != nil {
			return nil, err
		}
		offset = next
		d.aus = append(d.aus, AU{Data: data, RTPOffset: uint32(i) * frameLenTicks})
	}
	return d.aus, nil
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

	useSameStreamMux, ok := br.read(1)
	if !ok {
		return nil, ErrTruncated
	}
	if useSameStreamMux == 0 {
		smc, asc, frameLength, err := parseStreamMuxConfigBits(br, d.ascBuf)
		if err != nil {
			return nil, err
		}
		d.smc = smc
		// asc was packed into d.ascBuf's backing array. Recycle the
		// previously active buffer as the next packet's scratch and promote
		// asc to active, so the two never share a backing array. A packet
		// that fails mid-parse returns above with d.asc untouched, so the
		// in-place mutation extractBitsInto performed on the scratch buffer
		// cannot corrupt the retained config.
		d.ascBuf = d.asc
		d.asc = asc
		d.frameLength = frameLength
		d.haveSMC = true
	} else if !d.haveSMC {
		return nil, ErrNoConfig
	}

	if d.smc.numSubFrames+1 > MaxSubFrames {
		return nil, fmt.Errorf("%w: numSubFrames %d exceeds cap", ErrUnsupportedMux, d.smc.numSubFrames)
	}

	frameLenTicks := uint32(d.cfg.SamplesPerFrame)
	if frameLenTicks == 0 {
		frameLenTicks = d.frameLength
	}

	d.aus = d.aus[:0]
	d.inBandData = d.inBandData[:0]
	for i := 0; i <= d.smc.numSubFrames; i++ {
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

		start := len(d.inBandData)
		d.inBandData = appendBits(d.inBandData, payload, br.pos, length*8)
		data := d.inBandData[start:]
		br.pos += length * 8

		d.aus = append(d.aus, AU{Data: data, RTPOffset: uint32(i) * frameLenTicks})
	}
	br.byteAlign()

	return d.aus, nil
}
