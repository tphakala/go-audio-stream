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
// This package currently covers the out-of-band (Config.MuxConfigPresent ==
// false), single-subframe case: the payload starts directly at
// PayloadLengthInfo, byte-aligned. The in-band path and multi-subframe
// AudioMuxElements return ErrUnsupportedMux until a later task adds them.
func (d *Depacketizer) Depacketize(payload []byte, marker bool, rtpTime uint32) ([]AU, error) {
	_ = marker  // LATM does not fragment across packets; unused by the transform.
	_ = rtpTime // relative offsets only; the caller owns the absolute clock.

	if d.cfg.MuxConfigPresent {
		return nil, ErrUnsupportedMux // in-band path: a later task.
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
