package doctor

import (
	"math"
	"time"

	wavpcm "github.com/tphakala/go-wav/pcm"
)

// go-wav v0.4.0's pcm encoder writes a bext (Broadcast Wave Format) chunk
// natively when Config.Bext is set, so the listen check anchors a WAV to the
// RTCP sender clock's wall-clock capture time by handing the encoder a
// descriptor rather than splicing a chunk into the finished RIFF by hand.
// buildBext produces that descriptor; the writeWAV* paths in listen.go pass
// it through Config.Bext.

// bextOriginator identifies stream-doctor as the bext chunk's Originator: a
// fixed, non-PII string naming the tool that wrote the WAV, never the camera
// or its address.
const bextOriginator = "stream-doctor"

// bextDescriptionPrefix opens the bext Description field; buildBext appends
// the millisecond-precision sender clock start.
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

// maxBextSampleRate bounds the sample rate timeReferenceSamples will trust.
// sampleRate traces back to a camera-supplied SDP clock rate, parsed with an
// unbounded strconv.Atoi elsewhere in this module, so an implausibly large
// value must not be allowed to reach the float64-to-uint64 conversion below,
// where it could overflow and corrupt the TimeReference field. 10 MHz is far
// above any real audio sample rate, and since seconds-since-midnight is always
// under 86400, the largest product this bound allows (86400 * 10_000_000)
// stays well within both a float64's exact-integer range and uint64.
const maxBextSampleRate = 10_000_000 // 10 MHz, far above any real audio hardware.

// timeReferenceSamples returns the bext TimeReference: the number of audio
// samples at sampleRate from 00:00:00 UTC on u's calendar date to u itself,
// rounded to the nearest sample rather than truncated. u must already be in
// UTC; buildBext converts before calling this, so the calendar date used for
// the midnight anchor agrees with the OriginationDate field it writes.
//
// sampleRate is untrusted input (see maxBextSampleRate); an implausible
// value, zero, or negative yields 0 (TimeReference unavailable) rather than
// a computed value, while OriginationDate and OriginationTime still populate
// from senderStart regardless.
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

// buildBext returns the bext (Broadcast Wave Format, EBU Tech 3285) chunk
// descriptor that anchors a listen WAV to the sender's absolute wall-clock
// capture time, or nil when senderStart is the zero value (no valid RTCP
// sender clock), in which case the encoder writes a plain RIFF/WAVE stream
// with no bext chunk, matching HTTP sources and cameras that send no Sender
// Reports.
//
// senderStart is the sender clock's wall-clock time of the WAV's first sample
// (RTCP Sender-Report derived, runner.listen's SenderClock.WallClock result);
// sampleRate is the written WAV's sample rate, used to compute the
// TimeReference sample count. senderStart is converted to UTC internally, so
// callers may pass any location.
//
// Every field is populated from fully controlled inputs: a fixed ASCII
// Description and Originator, EBU Tech 3285 date/time strings from
// time.Format, and a numeric TimeReference. senderStart derives from a 32-bit
// NTP timestamp (bounded to roughly 2036), so the formatted date and time
// always fit their fixed field widths. The descriptor therefore always
// satisfies go-wav's bext validation, so passing it through Config.Bext never
// fails the encode; that is why the doctor no longer needs a splice-and-fall-
// back path to protect the capture, and the atomic temp-file-plus-rename in
// runner.listen already guarantees the destination is never left partial.
//
// Version is deliberately left 0: this chunk carries none of the version-1
// (UMID) or version-2 (loudness) fields, go-wav writes those zero, and a
// reader interprets that zero value as "not present".
func buildBext(senderStart time.Time, sampleRate int) *wavpcm.Bext {
	if senderStart.IsZero() {
		return nil
	}
	u := senderStart.UTC()
	return &wavpcm.Bext{
		Description:     bextDescriptionPrefix + u.Format(bextDescriptionTimeFormat),
		Originator:      bextOriginator,
		OriginationDate: u.Format(bextDateFormat),
		OriginationTime: u.Format(bextTimeFormat),
		TimeReference:   timeReferenceSamples(u, sampleRate),
	}
}
