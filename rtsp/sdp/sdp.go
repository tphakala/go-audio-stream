package sdp

import (
	"errors"

	audiostream "github.com/tphakala/go-audio-stream"
)

// Size and count limits. All SDP input is untrusted; these bounds keep a
// hostile body from exhausting memory or CPU.
const (
	// MaxInputSize is the largest SDP body Parse accepts, in bytes.
	MaxInputSize = 64 * 1024
	// MaxMediaSections is the largest number of m= sections Parse accepts.
	MaxMediaSections = 16
	// MaxAttributesPerSection is the largest number of a= lines Parse
	// accepts in the session block or in any single media section.
	MaxAttributesPerSection = 128
)

// Sentinel errors. Parse returns one of these (never any other error
// value) and never panics.
var (
	// ErrInputTooLarge is returned when the body exceeds MaxInputSize.
	ErrInputTooLarge = errors.New("sdp: input exceeds maximum size")
	// ErrTooManyMedia is returned when the body has more than
	// MaxMediaSections m= sections.
	ErrTooManyMedia = errors.New("sdp: too many media sections")
	// ErrTooManyAttributes is returned when the session block or a media
	// section has more than MaxAttributesPerSection a= lines.
	ErrTooManyAttributes = errors.New("sdp: too many attributes in a section")
)

// Session is a parsed SDP body: the session-level control attribute and
// the media sections in document order. Session and per-media fields that
// were absent are their zero values.
type Session struct {
	// Control is the session-level a=control value, "" if absent. Stored
	// verbatim (no URL resolution; that is the RTSP client's job in M4).
	Control string
	// Name is the session name from the s= line, "" if absent. It is
	// server-controlled free text (for example a camera's stream label),
	// stored verbatim and never interpreted; a consumer that displays it must
	// treat it as untrusted.
	Name string
	// Tool is the session-level a=tool value, "" if absent. Cameras and RTSP
	// servers commonly set it to their streaming stack and version (for
	// example "BC Streaming Media v..."), which is a useful identity hint.
	// Server-controlled free text, stored verbatim and never interpreted.
	Tool string
	// Media holds the m= sections in the order they appeared.
	Media []Media
}

// Media is one parsed m= section.
type Media struct {
	// Kind is the media type from the m= line: MediaAudio, MediaVideo, or
	// MediaOther for anything else.
	Kind audiostream.MediaKind
	// Proto is the transport protocol token from the m= line, for example
	// "RTP/AVP" or "RTP/SAVP".
	Proto string
	// Formats lists the RTP payload type numbers from the m= line, in
	// order. Non-numeric format tokens are skipped.
	Formats []int
	// Control is the media-level a=control value, "" if absent.
	Control string
	// RTPMaps holds parsed a=rtpmap lines keyed by payload type.
	RTPMaps map[int]RTPMap
	// FMTPs holds the raw a=fmtp parameter string (everything after the
	// payload type and its separating space) keyed by payload type.
	FMTPs map[int]string
}

// RTPMap is a parsed a=rtpmap line.
type RTPMap struct {
	// PayloadType is the dynamic or static payload type number.
	PayloadType int
	// EncodingName is the encoding token, for example "MPEG4-GENERIC",
	// "opus", or "PCMU". Case is preserved as received.
	EncodingName string
	// ClockRate is the RTP clock rate in Hz.
	ClockRate int
	// Channels is the channel count, or 0 when the rtpmap omitted it.
	Channels int
}
