package g711

import (
	"encoding/binary"
	"errors"

	audiostream "github.com/tphakala/go-audio-stream"
)

// ErrShortBuffer is returned by Depacketize when dst is too small to hold
// the expanded PCM (it needs 2*len(payload) bytes).
var ErrShortBuffer = errors.New("g711: destination buffer too small")

// ErrUnknownLaw is returned when law is neither audiostream.MuLaw nor
// audiostream.ALaw. Decoding with the wrong table produces plausible but
// wrong audio, which is worse than a clear failure, so an unrecognized
// law is refused rather than defaulted.
var ErrUnknownLaw = errors.New("g711: unknown companding law")

// muLawExpand expands one mu-law companded byte to a signed 16-bit PCM
// sample, following the ITU-T G.711 (Sun/CCITT g711.c) reference decode:
// invert all bits, split into sign, 3-bit exponent, and 4-bit mantissa,
// then reconstruct the linear sample and remove the mu-law bias (0x84).
func muLawExpand(b byte) int16 {
	const bias = 0x84
	u := ^b
	sign := u & 0x80
	exponent := (u >> 4) & 0x07
	mantissa := u & 0x0F
	t := ((int(mantissa) << 3) + bias) << exponent
	if sign != 0 {
		return int16(bias - t)
	}
	return int16(t - bias)
}

// aLawExpand expands one A-law companded byte to a signed 16-bit PCM
// sample, following the ITU-T G.711 (Sun/CCITT g711.c) reference decode.
// A-law's sign convention is the opposite of mu-law: the sign bit set
// means positive.
func aLawExpand(b byte) int16 {
	a := b ^ 0x55
	seg := (a >> 4) & 0x07
	t := int(a&0x0F) << 4
	switch seg {
	case 0:
		t += 8
	case 1:
		t += 0x108
	default:
		t += 0x108
		t <<= seg - 1
	}
	if a&0x80 != 0 {
		return int16(t)
	}
	return int16(-t)
}

// muLawTable and aLawTable are 256-entry lookup tables built once at
// package init from muLawExpand and aLawExpand, the single source of
// truth for the expansion.
var (
	muLawTable [256]int16
	aLawTable  [256]int16
)

func init() {
	for i := range 256 {
		muLawTable[i] = muLawExpand(byte(i))
		aLawTable[i] = aLawExpand(byte(i))
	}
}

// Depacketize expands a G.711 RTP payload (one companded byte per sample)
// into signed 16-bit little-endian PCM, writing into dst and returning
// the number of bytes written (2*len(payload)). law selects mu-law
// (audiostream.MuLaw, PCMU) or A-law (audiostream.ALaw, PCMA). If dst is
// shorter than 2*len(payload), Depacketize writes nothing and returns
// (0, ErrShortBuffer). An empty payload writes nothing and returns
// (0, nil). A law that is neither of the two returns (0, ErrUnknownLaw)
// rather than decoding with an arbitrary table. No allocation occurs;
// dst is caller-owned and reusable across packets.
//
// dst and payload must not overlap. Each input byte expands to two
// output bytes, so a shared buffer would overwrite input that has not
// been read yet and silently corrupt the samples after the first.
func Depacketize(dst, payload []byte, law audiostream.Law) (int, error) {
	if len(payload) == 0 {
		return 0, nil
	}
	need := 2 * len(payload)
	if len(dst) < need {
		return 0, ErrShortBuffer
	}
	var table *[256]int16
	switch law {
	case audiostream.MuLaw:
		table = &muLawTable
	case audiostream.ALaw:
		table = &aLawTable
	default:
		return 0, ErrUnknownLaw
	}
	for i, b := range payload {
		binary.LittleEndian.PutUint16(dst[2*i:], uint16(table[b]))
	}
	return need, nil
}

// DepacketizeAlloc expands a G.711 payload into a freshly allocated s16le
// PCM slice of length 2*len(payload). It is the allocating convenience
// wrapper over Depacketize for callers that do not manage their own
// buffer. It never constructs audiostream.Frame.
//
// The buffer it allocates is always correctly sized, so the only error it
// can return is ErrUnknownLaw, and it returns a nil slice with it.
func DepacketizeAlloc(payload []byte, law audiostream.Law) ([]byte, error) {
	dst := make([]byte, 2*len(payload))
	if _, err := Depacketize(dst, payload, law); err != nil {
		return nil, err
	}
	return dst, nil
}
