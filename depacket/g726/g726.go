package g726

import (
	"encoding/binary"
	"errors"
	"math/bits"

	audiostream "github.com/tphakala/go-audio-stream"
)

// ErrShortBuffer is returned by Decode when dst is too small to hold the
// expanded PCM. The output is 2 bytes per decoded sample, and one sample is
// produced per whole codeword in the payload.
var ErrShortBuffer = errors.New("g726: destination buffer too small")

// ErrUnknownBitRate is returned by New for a bit rate that is not one of the
// four ITU-T G.726 rates. Decoding with the wrong codeword width produces
// plausible but wrong audio, so an unrecognized rate is refused rather than
// defaulted.
var ErrUnknownBitRate = errors.New("g726: unknown bit rate")

// ErrUnknownPacking is returned by New for a packing order that is neither
// audiostream.G726PackingRFC3551 nor audiostream.G726PackingAAL2. Unpacking with
// the wrong bit order recovers different codewords and so produces plausible but
// wrong audio, so an unrecognized packing is refused rather than defaulted,
// matching ErrUnknownBitRate and g711's ErrUnknownLaw.
var ErrUnknownPacking = errors.New("g726: unknown codeword packing")

// ErrIncompletePayload is returned by Decode when the payload does not hold a
// whole number of RFC 3551 codeword groups: its bit length is not a multiple of
// the codeword width, so the final octet is not completely packed. RFC 3551
// section 4.5.4 requires the codeword count to be a multiple of 8, 2, 8, and 4
// for G726-40/32/24/16, which for an octet payload is exactly this divisibility
// (always satisfied at 16 and 32 kbps; length a multiple of 3 or 5 octets at 24
// or 40 kbps). Such a payload is malformed, so it is refused rather than decoded
// with its trailing bits silently dropped. The rule is a property of the codeword
// geometry rather than of the bit order, so it governs the AAL2 packing
// identically and this error is returned for either.
var ErrIncompletePayload = errors.New("g726: payload is not a whole number of codewords")

// float11 is the ITU-T internal floating-point representation of a signal
// history value: a sign, a 4-bit exponent, and a 6-bit mantissa. The predictor
// multiply works in this form.
type float11 struct {
	sign int32 // 0 for non-negative, 1 for negative
	exp  int32
	mant int32
}

// int16Min is the sentinel the inverse-quantizer table carries for the two
// lowest-magnitude codewords, forcing the reconstructed difference to zero.
const int16Min = -32768

// rateTable holds the codeword width and the inverse-quantizer, scale-factor,
// and adaptation-speed tables for one G.726 bit rate. The tables are indexed by
// the whole codeword and carry the plain (unscaled) ITU-T values.
type rateTable struct {
	bits   int
	iquant []int32
	w      []int32
	f      []int32
}

var rateTables = map[audiostream.G726BitRate]*rateTable{
	audiostream.G726Rate16: {
		bits:   2,
		iquant: []int32{116, 365, 365, 116},
		w:      []int32{-22, 439, 439, -22},
		f:      []int32{0, 7, 7, 0},
	},
	audiostream.G726Rate24: {
		bits:   3,
		iquant: []int32{int16Min, 135, 273, 373, 373, 273, 135, int16Min},
		w:      []int32{-4, 30, 137, 582, 582, 137, 30, -4},
		f:      []int32{0, 1, 2, 7, 7, 2, 1, 0},
	},
	audiostream.G726Rate32: {
		bits: 4,
		iquant: []int32{
			int16Min, 4, 135, 213, 273, 323, 373, 425,
			425, 373, 323, 273, 213, 135, 4, int16Min,
		},
		w: []int32{
			-12, 18, 41, 64, 112, 198, 355, 1122,
			1122, 355, 198, 112, 64, 41, 18, -12,
		},
		f: []int32{
			0, 0, 0, 1, 1, 1, 3, 7,
			7, 3, 1, 1, 1, 0, 0, 0,
		},
	},
	audiostream.G726Rate40: {
		bits: 5,
		iquant: []int32{
			int16Min, -66, 28, 104, 169, 224, 274, 318,
			358, 395, 429, 459, 488, 514, 539, 566,
			566, 539, 514, 488, 459, 429, 395, 358,
			318, 274, 224, 169, 104, 28, -66, int16Min,
		},
		w: []int32{
			14, 14, 24, 39, 40, 41, 58, 100,
			141, 179, 219, 280, 358, 440, 529, 696,
			696, 529, 440, 358, 280, 219, 179, 141,
			100, 58, 41, 40, 39, 24, 14, 14,
		},
		f: []int32{
			0, 0, 0, 0, 0, 1, 1, 1, 1, 1, 2, 3, 4, 5, 6, 6,
			6, 6, 5, 4, 3, 2, 1, 1, 1, 1, 1, 0, 0, 0, 0, 0,
		},
	},
}

// Decoder holds the ITU-T G.726 ADPCM decoder state for one RTP stream. It is
// not safe for concurrent use; use one Decoder per stream and feed its packets
// in order. Reset it only when the stream restarts (an SSRC change).
//
// The field layout follows the ITU-T G.726 section 4 decoder: the adaptive
// quantizer scale factors (yu, yl), speed-control state (dms, dml, ap), the
// pole and zero predictor coefficients (a, b) and their signal histories (sr,
// dq, pk), the tone detector (td), and the estimate and scale factor
// precomputed for the next sample (se, sez, y).
type Decoder struct {
	rt *rateTable
	// msbFirst selects the AAL2 (ITU-T I.366.2) codeword packing, in which the
	// first codeword's most significant bit is the most significant bit of the
	// first octet, over the plain RFC 3551 least-significant-bit-first order.
	// Decode branches on it once per payload, never per sample.
	msbFirst bool

	sr  [2]float11
	dq  [6]float11
	a   [2]int32
	b   [6]int32
	pk  [2]int32
	ap  int32
	yu  int32
	yl  int32
	dms int32
	dml int32
	td  int32
	se  int32
	sez int32
	y   int32
}

// New returns a G.726 decoder for the given bit rate and codeword packing. It
// returns ErrUnknownBitRate if br is not one of the four ITU-T rates and
// ErrUnknownPacking if packing is neither defined order. The returned Decoder is
// reset and ready to decode the first packet of a stream.
//
// packing selects the wire bit order, not a different codec:
// audiostream.G726PackingRFC3551 (the zero value) is the plain G726-NN RTP form
// of RFC 3551 section 4.5.4, and audiostream.G726PackingAAL2 is the
// AAL2-G726-NN form of ITU-T I.366.2 Annex E. Both carry the same codeword sequence
// through the same ADPCM state machine, so the two differ only in how the
// codewords are unpacked from the payload octets.
func New(br audiostream.G726BitRate, packing audiostream.G726Packing) (*Decoder, error) {
	rt, ok := rateTables[br]
	if !ok {
		return nil, ErrUnknownBitRate
	}
	var msbFirst bool
	switch packing {
	case audiostream.G726PackingRFC3551:
		msbFirst = false
	case audiostream.G726PackingAAL2:
		msbFirst = true
	default:
		return nil, ErrUnknownPacking
	}
	d := &Decoder{rt: rt, msbFirst: msbFirst}
	d.Reset()
	return d, nil
}

// Reset returns the decoder to the ITU-T initial state, discarding the adapted
// predictor and quantizer history. Call it when a stream restarts (an SSRC
// change), not on a plain sequence gap: G.726 has no clean resync point mid
// stream and a gap re-converges on its own, so resetting there sounds worse.
func (d *Decoder) Reset() {
	d.sr = [2]float11{{mant: 1 << 5}, {mant: 1 << 5}}
	for i := range d.dq {
		d.dq[i] = float11{mant: 1 << 5}
	}
	d.a = [2]int32{}
	d.b = [6]int32{}
	d.pk = [2]int32{1, 1}
	d.ap = 0
	d.yu = 544
	d.yl = 34816
	d.dms = 0
	d.dml = 0
	d.td = 0
	d.se = 0
	d.sez = 0
	d.y = 544
}

// Decode expands a G.726 RTP payload into signed 16-bit little-endian PCM,
// writing into dst and returning the number of bytes written. The payload is
// unpacked in the codeword order the decoder was constructed with: RFC 3551
// section 4.5.4 (first codeword in the least significant bits) by default, or
// the AAL2 order of ITU-T I.366.2 Annex E (first codeword in the most significant
// bits) for a decoder built with audiostream.G726PackingAAL2. The two orders
// carry the same codewords, so the choice does not change the payload's length
// or its framing rules, only how it is unpacked. It must hold a whole number of
// codeword groups: if its bit
// length is not a multiple of the codeword width (so the final octet is not
// completely packed, a malformed packet under RFC 3551 section 4.5.4), Decode
// writes nothing, leaves the adaptive state untouched, and returns
// (0, ErrIncompletePayload). dst must hold 2*n bytes where n is the codeword
// count, else Decode writes nothing and returns (0, ErrShortBuffer). An empty
// payload writes nothing and returns (0, nil). No allocation occurs; dst is
// caller-owned and reusable across packets. dst and payload must not overlap.
//
// Decode advances the decoder's adaptive state, so consecutive calls on the
// same Decoder continue one logical stream: decoding a stream split across two
// calls yields the same PCM as decoding it in one.
func (d *Decoder) Decode(dst, payload []byte) (int, error) {
	if len(payload) == 0 {
		return 0, nil
	}
	if (len(payload)*8)%d.rt.bits != 0 {
		return 0, ErrIncompletePayload
	}
	nsamp := (len(payload) * 8) / d.rt.bits
	need := 2 * nsamp
	if len(dst) < need {
		return 0, ErrShortBuffer
	}
	// The packing branch is hoisted out of the per-sample loop: the two orders
	// share the whole ADPCM state machine and differ only in the codeword
	// reader, so branching once per payload keeps the hot path free of a
	// per-sample test or indirect call.
	pos := 0
	if d.msbFirst {
		for k := 0; k < nsamp; k++ {
			code := readCodewordMSB(payload, pos, d.rt.bits)
			pos += d.rt.bits
			binary.LittleEndian.PutUint16(dst[2*k:], uint16(d.decodeSample(code)))
		}
		return need, nil
	}
	for k := 0; k < nsamp; k++ {
		code := readCodewordLSB(payload, pos, d.rt.bits)
		pos += d.rt.bits
		binary.LittleEndian.PutUint16(dst[2*k:], uint16(d.decodeSample(code)))
	}
	return need, nil
}

// DecodeAlloc expands a G.726 payload into a freshly allocated s16le PCM slice.
// It is the allocating convenience wrapper over Decode for callers that do not
// manage their own buffer, and it never constructs audiostream.Frame. The
// buffer it allocates is always correctly sized, so it never returns
// ErrShortBuffer; it returns ErrIncompletePayload (and a nil slice) for a
// payload that is not a whole number of codeword groups.
func (d *Decoder) DecodeAlloc(payload []byte) ([]byte, error) {
	nsamp := (len(payload) * 8) / d.rt.bits
	dst := make([]byte, 2*nsamp)
	if _, err := d.Decode(dst, payload); err != nil {
		return nil, err
	}
	return dst, nil
}

// OutputLen returns the number of bytes Decode writes for a payload of this
// length: 2 bytes per whole codeword. A caller can size its destination buffer
// with it to keep the delivery path allocation-free.
func (d *Decoder) OutputLen(payload []byte) int {
	return 2 * ((len(payload) * 8) / d.rt.bits)
}

// readCodewordLSB extracts one width-bit codeword whose first transmitted bit is
// at global bit index pos, packed least-significant-bit-first (RFC 3551 section
// 4.5.4 order): the bit transmitted first is the codeword's least significant,
// and octet bit 0 is transmitted before octet bit 7.
func readCodewordLSB(payload []byte, pos, width int) int32 {
	var v int32
	for i := 0; i < width; i++ {
		g := pos + i
		bit := (payload[g>>3] >> uint(g&7)) & 1
		v |= int32(bit) << uint(i)
	}
	return v
}

// readCodewordMSB extracts one width-bit codeword whose first transmitted bit is
// at global bit index pos, packed most-significant-bit-first (the AAL2-G726 form
// of ITU-T I.366.2 Annex E): the bit transmitted
// first is the codeword's most significant, and octet bit 7 is transmitted
// before octet bit 0. Both orders lay the codewords out in the same sequence at
// the same bit offsets, so only the two bit numberings are reversed; a payload
// packed either way yields the same codewords through the matching reader.
func readCodewordMSB(payload []byte, pos, width int) int32 {
	var v int32
	for i := 0; i < width; i++ {
		g := pos + i
		bit := (payload[g>>3] >> uint(7-(g&7))) & 1
		v = v<<1 | int32(bit)
	}
	return v
}

// decodeSample runs one ADPCM step for codeword code and returns the s16le PCM
// sample, advancing the adaptive state. It follows ITU-T G.726 section 4.
func (d *Decoder) decodeSample(code int32) int16 {
	sign := code >> uint(d.rt.bits-1)

	dq := d.inverseQuant(code)

	// Transition detector: fires on a large difference while a tone was
	// already latched.
	ylint := d.yl >> 15
	ylfrac := (d.yl >> 10) & 0x1F
	thr2 := (0x20 + ylfrac) << uint(ylint)
	if ylint > 9 {
		thr2 = 0x1F << 10
	}
	tr := int32(0)
	if d.td == 1 && dq > (3*thr2)>>2 {
		tr = 1
	}

	if sign != 0 {
		dq = -dq
	}
	reSignal := int32(int16(d.se + dq))

	pk0 := int32(0)
	if d.sez+dq != 0 {
		pk0 = sgn(d.sez + dq)
	}
	dq0 := int32(0)
	if dq != 0 {
		dq0 = sgn(dq)
	}

	if tr != 0 {
		d.a = [2]int32{}
		d.b = [6]int32{}
	} else {
		fa1 := clipIntp2((-d.a[0]*d.pk[0]*pk0)>>5, 8)
		d.a[1] += 128*pk0*d.pk[1] + fa1 - (d.a[1] >> 7)
		d.a[1] = clip(d.a[1], -12288, 12288)
		d.a[0] += 64*3*pk0*d.pk[0] - (d.a[0] >> 8)
		lim := 15360 - d.a[1]
		d.a[0] = clip(d.a[0], -lim, lim)
		for i := 0; i < 6; i++ {
			d.b[i] += 128*dq0*sgn(-d.dq[i].sign) - (d.b[i] >> 8)
		}
	}

	// Shift the signal history and re-encode the newest sample and difference.
	d.pk[1] = d.pk[0]
	if pk0 != 0 {
		d.pk[0] = pk0
	} else {
		d.pk[0] = 1
	}
	d.sr[1] = d.sr[0]
	d.sr[0] = i2f(reSignal)
	for i := 5; i > 0; i-- {
		d.dq[i] = d.dq[i-1]
	}
	d.dq[0] = i2f(dq)
	d.dq[0].sign = sign

	d.td = 0
	if d.a[1] < -11776 {
		d.td = 1
	}

	// Speed control.
	d.dms += (d.rt.f[code] << 4) + ((-d.dms) >> 5)
	d.dml += (d.rt.f[code] << 4) + ((-d.dml) >> 7)
	if tr != 0 {
		d.ap = 256
	} else {
		d.ap += (-d.ap) >> 4
		if d.y <= 1535 || d.td != 0 || abs32((d.dms<<2)-d.dml) >= (d.dml>>3) {
			d.ap += 0x20
		}
	}

	// Quantizer scale factors, then the scale factor for the next sample.
	d.yu = clip(d.y+d.rt.w[code]+((-d.y)>>5), 544, 5120)
	d.yl += d.yu + ((-d.yl) >> 6)
	al := d.ap >> 2
	if d.ap >= 256 {
		al = 1 << 6
	}
	d.y = (d.yl + (d.yu-(d.yl>>6))*al) >> 6

	// Predictor estimate for the next sample.
	d.se = 0
	for i := 0; i < 6; i++ {
		d.se += mult(i2f(d.b[i]>>2), d.dq[i])
	}
	d.sez = d.se >> 1
	for i := 0; i < 2; i++ {
		d.se += mult(i2f(d.a[i]>>2), d.sr[i])
	}
	d.se >>= 1

	return int16(clip(reSignal*4, -0xFFFF, 0xFFFF))
}

// inverseQuant is the inverse adaptive quantizer: it maps a codeword to the
// magnitude of the reconstructed difference signal, given the current scale
// factor. The sign is applied by the caller.
func (d *Decoder) inverseQuant(i int32) int32 {
	dql := d.rt.iquant[i] + (d.y >> 2)
	if dql < 0 {
		return 0
	}
	dex := (dql >> 7) & 0xF
	dqt := (1 << 7) + (dql & 0x7F)
	return (dqt << uint(dex)) >> 7
}

// i2f converts a signed integer to the ITU-T internal floating-point form
// (sign, 4-bit exponent, 6-bit mantissa).
func i2f(i int32) float11 {
	var f float11
	if i < 0 {
		f.sign = 1
		i = -i
	}
	if i == 0 {
		f.mant = 1 << 5
		return f
	}
	f.exp = log2Of(i) + 1
	f.mant = (i << 6) >> uint(f.exp)
	return f
}

// mult multiplies two floating-point values and returns a signed 16-bit result,
// the predictor's coefficient-by-history product.
func mult(f1, f2 float11) int32 {
	exp := f1.exp + f2.exp
	res := ((f1.mant * f2.mant) + 0x30) >> 4
	if exp > 19 {
		res <<= uint(exp - 19)
	} else {
		res >>= uint(19 - exp)
	}
	if f1.sign^f2.sign != 0 {
		res = -res
	}
	return int32(int16(res))
}

// sgn returns -1 for a negative value and 1 otherwise, matching the ITU-T
// reference (zero maps to 1).
func sgn(v int32) int32 {
	if v < 0 {
		return -1
	}
	return 1
}

// log2Of returns floor(log2(v)) for v >= 1, the position of the highest set
// bit.
func log2Of(v int32) int32 {
	return int32(bits.Len32(uint32(v))) - 1
}

// clip constrains v to the inclusive range [lo, hi].
func clip(v, lo, hi int32) int32 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// clipIntp2 constrains v to a signed p-bit range, [-2^p, 2^p - 1].
func clipIntp2(v, p int32) int32 {
	return clip(v, -(1 << uint(p)), (1<<uint(p))-1)
}

// abs32 returns the absolute value of a 32-bit integer.
func abs32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}
