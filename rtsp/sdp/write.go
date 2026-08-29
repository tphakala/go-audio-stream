package sdp

import (
	"errors"
	"strconv"
	"strings"
)

var (
	// ErrInjection is returned when a free-text field contains a CR, LF, or NUL
	// that would inject extra SDP lines.
	ErrInjection = errors.New("sdp: field contains a forbidden control character")
	// ErrBadPayloadType is returned when PayloadType is outside 0..127.
	ErrBadPayloadType = errors.New("sdp: payload type out of range 0..127")
)

// WriteSpec describes a single-track audio session to serialize. The parse
// side's Session struct is receive-shaped (it drops o=/c=/t=), so the writer
// takes its own minimal spec rather than trying to invert Parse exactly.
type WriteSpec struct {
	// Name is the s= session name; empty writes "s= " (RFC 4566: no meaningful
	// name uses a single space).
	Name string
	// SessionID fills the o= sess-id and sess-version.
	SessionID uint64
	// PayloadType is the RTP payload type (96..127 for dynamic; 10/11 are legal
	// only for 44.1 kHz L16, not enforced here).
	PayloadType int
	// EncodingName is the rtpmap encoding, written verbatim ("L16", "opus").
	EncodingName string
	// ClockRate is the rtpmap clock rate: the sample rate for L16, always 48000
	// for Opus.
	ClockRate int
	// Channels is the rtpmap channel count; Opus must pass 2 (RFC 7587)
	// regardless of the true source channel count. Omitted from the rtpmap when
	// not positive.
	Channels int
	// Control is the a=control value for the track (e.g. "trackID=0"); omitted
	// when empty.
	Control string
	// FMTP is the a=fmtp parameter string (e.g. "sprop-stereo=0"); omitted when
	// empty.
	FMTP string
	// Ptime is the a=ptime value in milliseconds; omitted when not positive.
	Ptime int
}

// WriteSession serializes spec to a complete RFC 4566 SDP body with CRLF line
// endings, in the order v, o, s, c, t, m, rtpmap, fmtp, ptime, control. The
// connection line is written as IN IP4 0.0.0.0 because TCP-interleaved delivery
// ignores it. It returns ErrBadPayloadType for a payload type outside 0..127
// and ErrInjection if any free-text field would inject a line break.
//
//nolint:gocritic // WriteSpec by value is the config-struct API; WriteSession runs once at DESCRIBE time, not on a hot path.
func WriteSession(spec WriteSpec) ([]byte, error) {
	if spec.PayloadType < 0 || spec.PayloadType > 127 {
		return nil, ErrBadPayloadType
	}
	for _, f := range []string{spec.Name, spec.EncodingName, spec.Control, spec.FMTP} {
		if strings.ContainsAny(f, "\r\n\x00") {
			return nil, ErrInjection
		}
	}

	pt := strconv.Itoa(spec.PayloadType)
	id := strconv.FormatUint(spec.SessionID, 10)
	name := spec.Name
	if name == "" {
		name = " " // RFC 4566: a session with no meaningful name uses "s= "
	}

	var b strings.Builder
	writeLine(&b, "v=0")
	writeLine(&b, "o=- "+id+" "+id+" IN IP4 0.0.0.0")
	writeLine(&b, "s="+name)
	writeLine(&b, "c=IN IP4 0.0.0.0")
	writeLine(&b, "t=0 0")
	writeLine(&b, "m=audio 0 RTP/AVP "+pt)

	rtpmap := "a=rtpmap:" + pt + " " + spec.EncodingName + "/" + strconv.Itoa(spec.ClockRate)
	if spec.Channels > 0 {
		rtpmap += "/" + strconv.Itoa(spec.Channels)
	}
	writeLine(&b, rtpmap)

	if spec.FMTP != "" {
		writeLine(&b, "a=fmtp:"+pt+" "+spec.FMTP)
	}
	if spec.Ptime > 0 {
		writeLine(&b, "a=ptime:"+strconv.Itoa(spec.Ptime))
	}
	if spec.Control != "" {
		writeLine(&b, "a=control:"+spec.Control)
	}
	return []byte(b.String()), nil
}

func writeLine(b *strings.Builder, line string) {
	b.WriteString(line)
	b.WriteString("\r\n")
}
