package aac

import "encoding/binary"

// auHeader is one parsed AU-header: the access-unit size in bytes and the
// AU-Index (first header) or AU-Index-delta (subsequent headers).
type auHeader struct {
	size  int
	index int
}

// parseAUHeaders reads the AU-headers section of an AAC-hbr payload. It
// returns the parsed headers, the payload offset where access-unit data
// begins, and a sentinel error on any inconsistency. It reads bits
// MSB-first from the header region and validates that the declared
// AU-headers-length divides into whole headers without leftover bits.
//
// The returned slice aliases the reused d.headers scratch: it is valid
// only until the next call and never escapes the enclosing Depacketize
// call, which fully consumes it before returning.
func (d *Depacketizer) parseAUHeaders(payload []byte) (headers []auHeader, dataStart int, err error) {
	if len(payload) < 2 {
		return nil, 0, ErrTruncatedHeader
	}
	l := int(binary.BigEndian.Uint16(payload[:2])) // AU-headers-length, in bits
	if l == 0 {
		return nil, 0, ErrTruncatedHeader
	}
	headerBytes := (l + 7) / 8
	dataStart = 2 + headerBytes
	if len(payload) < dataStart {
		return nil, 0, ErrTruncatedHeader
	}

	// hdr is the AU-headers region payload[2 : 2+headerBytes]. Every bit the
	// parse reads lies below l, hence within hdr.
	hdr := payload[2:dataStart]

	// readBits reads n bits MSB-first starting at bit offset bitPos within
	// hdr. It loads the whole bytes the field spans into an accumulator (at
	// most five bytes, since n <= 32) instead of indexing the array once per
	// bit, then shifts the field down and masks it. The loop guard in the
	// caller keeps bitPos+n <= l, so every hdr index is in range and the
	// result is bit-identical to a bit-at-a-time MSB-first read.
	readBits := func(bitPos, n int) int {
		if n == 0 {
			return 0
		}
		firstByte := bitPos / 8
		lastByte := (bitPos + n - 1) / 8
		var acc uint64
		for b := firstByte; b <= lastByte; b++ {
			acc = acc<<8 | uint64(hdr[b])
		}
		// Drop the bits after the field within the loaded bytes, then keep
		// the low n bits (masking off any leading bits before the field).
		rightPad := (lastByte+1)*8 - (bitPos + n)
		return int((acc >> uint(rightPad)) & ((uint64(1) << uint(n)) - 1))
	}

	d.headers = d.headers[:0]
	consumed := 0
	for consumed < l {
		if len(d.headers) >= MaxAUsPerPacket {
			return nil, 0, ErrTruncatedHeader
		}
		idxBits := d.cfg.IndexDeltaLength
		if len(d.headers) == 0 {
			idxBits = d.cfg.IndexLength
		}
		headerBits := d.cfg.SizeLength + idxBits
		if consumed+headerBits > l {
			return nil, 0, ErrTruncatedHeader
		}
		size := readBits(consumed, d.cfg.SizeLength)
		index := readBits(consumed+d.cfg.SizeLength, idxBits)
		consumed += headerBits
		// A non-zero index signals interleaving in either position: the
		// first header carries AU-Index, the rest AU-Index-delta, and a
		// non-interleaved stream leaves both at zero. Rejecting the
		// first one too means an interleaved stream is refused rather
		// than silently delivered in the wrong order.
		if index != 0 {
			return nil, 0, ErrInterleavingUnsupported
		}
		d.headers = append(d.headers, auHeader{size: size, index: index})
	}
	if consumed != l {
		// Defensive backstop: the loop guard consumed+headerBits > l above
		// already forces consumed == l on a clean exit, so this cannot fire
		// today. It stays as a guard against a future header-shape change.
		return nil, 0, ErrTruncatedHeader
	}
	return d.headers, dataStart, nil
}

// Depacketize processes one RTP payload and returns the access units it
// completes, in order. For a normal packet it returns one AU (single-AU
// packet) or several (multi-AU packet). For an access unit fragmented
// across packets it buffers the bytes and returns (nil, nil) until the
// final fragment arrives, then returns the single reassembled AU.
//
// marker is the RTP marker bit; rtpTime is unused by the transform and is
// accepted only to document the per-AU offset contract (offsets are
// relative, so the caller adds rtpTime itself). Any malformed input
// returns a sentinel error and never panics; on a fragment error the
// reassembly state is reset so the next packet starts clean.
func (d *Depacketizer) Depacketize(payload []byte, marker bool, rtpTime uint32) ([]AU, error) {
	_ = rtpTime // relative offsets only; the caller owns the absolute clock.

	headers, dataStart, err := d.parseAUHeaders(payload)
	if err != nil {
		if d.fragActive {
			d.Reset() // a malformed packet mid-reassembly abandons the fragment.
		}
		return nil, err
	}

	d.aus = d.aus[:0]

	if d.fragActive {
		if len(headers) != 1 {
			d.Reset()
			return nil, ErrTruncatedHeader
		}
		// Every fragment's AU-header declares the size of the complete
		// access unit, not of the piece it carries, so a continuation
		// declaring anything else belongs to a different unit and must
		// not be concatenated onto this one.
		if headers[0].size != d.fragTotalSize {
			d.Reset()
			return nil, ErrAUSizeOverflow
		}
		// Check the incoming length before appending: a continuation far
		// larger than the declared unit would otherwise be copied into
		// the buffer just to be rejected on the next line.
		if len(d.frag)+(len(payload)-dataStart) > d.fragTotalSize ||
			len(d.frag)+(len(payload)-dataStart) > MaxFragmentSize {
			d.Reset()
			return nil, ErrFragmentOverflow
		}
		d.frag = append(d.frag, payload[dataStart:]...)
		switch {
		case marker && len(d.frag) < d.fragTotalSize:
			// Sender flagged the last fragment but the accumulated bytes
			// fall short of the declared size.
			d.Reset()
			return nil, ErrAUSizeOverflow
		case marker: // len(d.frag) == d.fragTotalSize: completion.
			d.aus = append(d.aus, AU{Data: d.frag, RTPOffset: 0})
			d.fragActive = false
			d.fragTotalSize = 0
			// Do not clear d.frag: the returned AU aliases it until the
			// next call.
			return d.aus, nil
		case len(d.frag) == d.fragTotalSize:
			// Declared size exhausted with no final marker: inconsistent.
			d.Reset()
			return nil, ErrAUSizeOverflow
		default:
			return nil, nil // still buffering.
		}
	}

	dataLen := len(payload) - dataStart
	if len(headers) == 1 && headers[0].size > dataLen {
		// Fragment start candidate: the single AU is larger than the data
		// present in this packet.
		if marker {
			// The packet claims to be complete but is short.
			return nil, ErrAUSizeOverflow
		}
		if headers[0].size > MaxFragmentSize {
			return nil, ErrFragmentOverflow
		}
		d.fragTotalSize = headers[0].size
		d.frag = append(d.frag[:0], payload[dataStart:]...)
		d.fragActive = true
		return nil, nil
	}

	// Complete packet: all AUs are present in this payload.
	offset := dataStart
	for i := range headers {
		// Subtraction form, not offset+size > len(payload): a size near
		// int max would overflow the addition (and go negative) where int
		// is 32 bits, slipping past the guard into the slice below.
		if headers[i].size > len(payload)-offset {
			return nil, ErrAUSizeOverflow
		}
		d.aus = append(d.aus, AU{
			Data:      payload[offset : offset+headers[i].size],
			RTPOffset: uint32(i) * uint32(d.cfg.SamplesPerFrame),
		})
		offset += headers[i].size
	}
	// Trailing bytes after the last AU (offset < len(payload)) are ignored:
	// padding or auxiliary data this mode does not use.
	return d.aus, nil
}
