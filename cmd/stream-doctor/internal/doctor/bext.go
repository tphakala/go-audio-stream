package doctor

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"time"
)

// go-wav v0.3.0's encoder does not write a bext (Broadcast Wave Format)
// chunk; its README lists bext among the metadata chunks it does not
// implement. The listen check still wants to anchor a WAV to the RTCP
// sender clock's wall-clock capture time, so this file builds a bext chunk
// by hand and splices it into the finished RIFF stream go-wav already
// wrote, after encoding and before the atomic rename. See runner.listen in
// doctor.go for the splice-or-fall-back call site.

// bext chunk layout constants (EBU Tech 3285, the Broadcast Wave Format
// bext chunk). The body is a fixed 602 bytes, which is even, so the chunk
// never needs a RIFF pad byte: bextChunkSize (610) is itself even.
const (
	bextChunkID    = "bext"
	bextBodySize   = 602
	bextHeaderSize = 8 // 4-byte ASCII id plus a uint32 LE size.
	bextChunkSize  = bextHeaderSize + bextBodySize
)

// riffChunkID and waveFormType are the fixed identifiers spliceBext checks
// at the start of a plain RIFF/WAVE stream: the 4-byte ASCII magic at
// offset 0 and the form type at offset 8.
const (
	riffChunkID  = "RIFF"
	waveFormType = "WAVE"
)

// Byte offsets of the bext body's fixed fields, in wire order. Kept as named
// constants (rather than accumulated at runtime) so the layout is easy to
// check against EBU Tech 3285 by eye, and so TestBuildBextBodyOriginationDateTime
// and friends can index the body directly without re-deriving the offsets.
const (
	bextOffDescription         = 0   // [256]byte, ASCII, NUL-padded.
	bextOffOriginator          = 256 // [32]byte, ASCII, NUL-padded.
	bextOffOriginatorReference = 288 // [32]byte, left zero: unknown.
	bextOffOriginationDate     = 320 // [10]byte, "YYYY-MM-DD".
	bextOffOriginationTime     = 330 // [8]byte, "HH:MM:SS".
	bextOffTimeReferenceLow    = 338 // uint32 LE.
	bextOffTimeReferenceHigh   = 342 // uint32 LE.
	bextOffVersion             = 346 // uint16 LE, 0.
	bextOffUMID                = 348 // [64]byte, left zero: unknown.
	bextOffLoudnessValue       = 412 // int16, left zero: unmeasured.
	// LoudnessRange, MaxTruePeakLevel, MaxMomentaryLoudness, and
	// MaxShortTermLoudness follow LoudnessValue as four more zeroed int16
	// fields (offsets 414, 416, 418, 420), needing no named offset of their
	// own since nothing indexes them individually.
	bextOffReserved = 422 // [180]byte, left zero. 422+180 = 602 = bextBodySize.
)

// bextOriginator identifies stream-doctor as the bext chunk's Originator: a
// fixed, non-PII string naming the tool that wrote the WAV, never the camera
// or its address.
const bextOriginator = "stream-doctor"

// bextDescriptionPrefix opens the bext Description field; buildBextBody
// appends the millisecond-precision sender clock start.
const bextDescriptionPrefix = "Captured by stream-doctor. Sender clock start (UTC): "

// Layout formats for the bext body's text and Description timestamp fields,
// per EBU Tech 3285's OriginationDate ("YYYY-MM-DD") and OriginationTime
// ("HH:MM:SS"), plus the millisecond-precision RFC 3339 style timestamp
// embedded in Description.
const (
	bextDateFormat            = "2006-01-02"
	bextTimeFormat            = "15:04:05"
	bextDescriptionTimeFormat = "2006-01-02T15:04:05.000Z"
)

// putFixedASCII copies s into dst, truncated to len(dst) if s is longer.
// dst is a slice of a body already allocated by make, so it starts zeroed;
// copy alone therefore also NUL-pads any remainder when s is shorter, with
// no separate zero-fill needed.
//
// putFixedASCII does not validate its input: it is a plain byte-level copy
// with no awareness of UTF-8 or multi-byte characters, so a truncation could
// in principle split one. Every caller in this file passes ASCII text that
// already fits within the field width (a fixed tool name, or digits and
// hyphens/colons from time.Format), which is the contract this function
// relies on rather than one it enforces.
func putFixedASCII(dst []byte, s string) {
	copy(dst, s)
}

// maxBextSampleRate bounds the sample rate timeReferenceSamples will trust.
// sampleRate traces back to a camera-supplied SDP clock rate, parsed with an
// unbounded strconv.Atoi elsewhere in this module, so an implausibly large
// value must not be allowed to reach the float64-to-uint64 conversion below,
// where it could overflow and corrupt the TimeReference field (the audio
// bytes themselves are untouched either way). 10 MHz is far above any real
// audio sample rate, and since seconds-since-midnight is always under 86400,
// the largest product this bound allows (86400 * 10_000_000) stays well
// within both a float64's exact-integer range and uint64.
const maxBextSampleRate = 10_000_000 // 10 MHz, far above any real audio hardware.

// timeReferenceSamples returns the bext TimeReference: the number of audio
// samples at sampleRate from 00:00:00 UTC on u's calendar date to u itself,
// rounded to the nearest sample rather than truncated. u must already be in
// UTC; buildBextBody converts before calling this, so the calendar date used
// for the midnight anchor agrees with the OriginationDate field it writes.
//
// sampleRate is untrusted input (see maxBextSampleRate); an implausible
// value, zero, or negative yields 0 (TimeReference unavailable) rather than
// a computed value, while OriginationDate and OriginationTime still
// populate from senderStart regardless.
func timeReferenceSamples(u time.Time, sampleRate int) uint64 {
	if sampleRate <= 0 || sampleRate > maxBextSampleRate {
		return 0
	}
	midnight := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	seconds := u.Sub(midnight).Seconds()
	samples := math.Round(seconds * float64(sampleRate))
	if samples <= 0 {
		return 0
	}
	return uint64(samples)
}

// buildBextBody builds the 602-byte body of a Broadcast Wave Format bext
// chunk (EBU Tech 3285) anchoring a WAV to the sender's absolute wall-clock
// capture time. senderStart is the sender clock's wall-clock time of the
// WAV's first sample (RTCP Sender-Report derived, runner.listen's
// SenderClock.WallClock result); sampleRate is the written WAV's sample
// rate, used to compute the TimeReference sample count. senderStart is
// converted to UTC internally, so callers may pass any location.
//
// Description, Originator, OriginationDate, OriginationTime, TimeReference,
// and Version are populated. Version is deliberately written as 0: EBU Tech
// 3285 defines that value as a bext chunk carrying none of the version-1
// (UMID) or version-2 (loudness) fields, which matches this chunk exactly,
// so 0 is the correct wire value rather than a placeholder. OriginatorReference,
// UMID, the five loudness metrics, and Reserved are left at their zero value
// for a different reason: this doctor has no value to put there (no UMID is
// assigned, no loudness is measured), and a reader interprets that zero
// value as "not present" per the format.
func buildBextBody(senderStart time.Time, sampleRate int) []byte {
	u := senderStart.UTC()
	body := make([]byte, bextBodySize)

	putFixedASCII(body[bextOffDescription:bextOffDescription+256], bextDescriptionPrefix+u.Format(bextDescriptionTimeFormat))
	putFixedASCII(body[bextOffOriginator:bextOffOriginator+32], bextOriginator)
	putFixedASCII(body[bextOffOriginationDate:bextOffOriginationDate+10], u.Format(bextDateFormat))
	putFixedASCII(body[bextOffOriginationTime:bextOffOriginationTime+8], u.Format(bextTimeFormat))

	ref := timeReferenceSamples(u, sampleRate)
	binary.LittleEndian.PutUint32(body[bextOffTimeReferenceLow:], uint32(ref))
	binary.LittleEndian.PutUint32(body[bextOffTimeReferenceHigh:], uint32(ref>>32))

	// Version is deliberately 0 (see the doc comment above); UMID, the
	// loudness fields, and Reserved are left zero because their true values
	// are unknown to this doctor. Neither needs an explicit write here: body
	// was allocated by make above and is already zero throughout.

	return body
}

// buildBextChunk builds a complete bext chunk: the 4-byte ASCII id, the
// uint32 LE body size, and the 602-byte body from buildBextBody. See
// buildBextBody for the senderStart and sampleRate parameters.
func buildBextChunk(senderStart time.Time, sampleRate int) []byte {
	chunk := make([]byte, bextChunkSize)
	copy(chunk[0:4], bextChunkID)
	binary.LittleEndian.PutUint32(chunk[4:8], bextBodySize)
	copy(chunk[bextHeaderSize:], buildBextBody(senderStart, sampleRate))
	return chunk
}

// spliceBext inserts chunk (a complete bext chunk built by buildBextChunk)
// into a plain RIFF/WAVE byte stream in, immediately after the 12-byte RIFF
// header and before the first chunk go-wav wrote (fmt, then data). Chunk
// order beyond "fmt before data" is not significant to a WAV reader, and a
// leading bext is common practice; go-wav/pcm's decoder walks and skips any
// chunk id it does not recognize, so a bext chunk ahead of fmt does not
// disturb decoding.
//
// in must begin with the 4-byte ASCII "RIFF" and, at offset 8, "WAVE"; any
// other input, including an RF64/BW64 64-bit WAVE stream (which begins
// "RF64" or "BW64" instead of "RIFF"), returns an error rather than
// attempting a splice. A bounded stream-doctor listen capture is always
// plain RIFF (go-wav only promotes to RF64/BW64 well past what a listen
// window ever captures), so RF64/BW64 support is deliberately out of scope;
// the caller (runner.listen) treats any such error as "splice not
// applicable" and falls back to the plain WAV already on disk.
//
// On success it returns a new byte slice: the original RIFF header with
// ckSize increased by len(chunk), the chunk itself, then the rest of in
// unchanged (starting at offset 12).
func spliceBext(in, chunk []byte) ([]byte, error) {
	if len(in) < 12 {
		return nil, fmt.Errorf("bext splice: input too short for a RIFF header (%d bytes)", len(in))
	}
	if string(in[0:4]) != riffChunkID {
		return nil, fmt.Errorf("bext splice: not a RIFF stream (id %q)", in[0:4])
	}
	if string(in[8:12]) != waveFormType {
		return nil, fmt.Errorf("bext splice: not a WAVE form (form type %q)", in[8:12])
	}

	oldSize := binary.LittleEndian.Uint32(in[4:8])
	grow := uint32(len(chunk)) //nolint:gosec // len(chunk) is bextChunkSize (610), a small compile-time constant.
	if oldSize > math.MaxUint32-grow {
		return nil, fmt.Errorf("bext splice: RIFF size overflow (old %d, grow %d)", oldSize, grow)
	}

	out := make([]byte, 0, len(in)+len(chunk))
	out = append(out, in[0:4]...)
	var sizeBuf [4]byte
	binary.LittleEndian.PutUint32(sizeBuf[:], oldSize+grow)
	out = append(out, sizeBuf[:]...)
	out = append(out, in[8:12]...)
	out = append(out, chunk...)
	out = append(out, in[12:]...)
	return out, nil
}

// spliceBextFile reads the plain WAV go-wav wrote at tmpName, splices in a
// bext chunk anchoring it to senderStart at sampleRate, and writes the
// result to a new temp file in dir. It returns the new temp file's name on
// success; tmpName itself is left untouched, and the caller decides which
// of the two temp files gets renamed into place and which gets removed.
//
// Any failure (a read error, tmpName not being plain RIFF/WAVE, a write
// error) is returned rather than handled here, so the caller can fall back
// to the plain WAV already at tmpName: a capture must never be lost over a
// missing bext chunk. This function cleans up its own new temp file on a
// failure partway through the write, so a failed splice leaves no orphaned
// file for the caller to find.
func spliceBextFile(tmpName, dir string, senderStart time.Time, sampleRate int) (string, error) {
	raw, err := os.ReadFile(tmpName) //nolint:gosec // tmpName is the doctor's own os.CreateTemp file in the listen temp/rename flow, not user input.
	if err != nil {
		return "", err
	}
	spliced, err := spliceBext(raw, buildBextChunk(senderStart, sampleRate))
	if err != nil {
		return "", err
	}

	out, err := os.CreateTemp(dir, ".stream-doctor-bext-*.wav")
	if err != nil {
		return "", err
	}
	outName := out.Name()
	if _, werr := out.Write(spliced); werr != nil {
		_ = out.Close()
		_ = os.Remove(outName)
		return "", werr
	}
	if cerr := out.Close(); cerr != nil {
		_ = os.Remove(outName)
		return "", cerr
	}
	return outName, nil
}
