package sdp

import (
	"strconv"
	"strings"

	audiostream "github.com/tphakala/go-audio-stream"
)

// Parse parses an SDP body into a Session. It is total: it never panics.
// It enforces MaxInputSize, MaxMediaSections, and MaxAttributesPerSection
// as hard errors; every other malformation is handled leniently by
// skipping the offending line, so a single quirky attribute never fails
// the whole parse.
func Parse(body []byte) (*Session, error) {
	if len(body) > MaxInputSize {
		return nil, ErrInputTooLarge
	}

	s := &Session{}
	var current *Media
	sessionAttrs := 0
	mediaAttrs := 0

	// SplitSeq walks the lines without materializing a slice of every
	// line first, so a body that is nothing but newlines costs no more
	// than the body itself.
	for raw := range strings.SplitSeq(string(body), "\n") {
		line := strings.TrimSuffix(raw, "\r")
		if line == "" {
			continue
		}
		if len(line) < 2 || line[1] != '=' {
			continue
		}
		typ := line[0]
		val := line[2:]

		switch typ {
		case 's':
			// The session name is session-level and appears once; keep the
			// first, ignore any stray later s= line. Stored verbatim as
			// untrusted free text.
			if s.Name == "" {
				s.Name = val
			}
		case 'm':
			if len(s.Media) >= MaxMediaSections {
				return nil, ErrTooManyMedia
			}
			s.Media = append(s.Media, parseMediaLine(val))
			current = &s.Media[len(s.Media)-1]
			mediaAttrs = 0
		case 'a':
			if current == nil {
				sessionAttrs++
				if sessionAttrs > MaxAttributesPerSection {
					return nil, ErrTooManyAttributes
				}
			} else {
				mediaAttrs++
				if mediaAttrs > MaxAttributesPerSection {
					return nil, ErrTooManyAttributes
				}
			}
			parseAttribute(val, s, current)
		default:
			// v, o, c, t, b, e, and unknown types carry nothing this parser
			// needs (s is handled above, a below).
		}
	}

	return s, nil
}

// parseMediaLine parses the value of an m= line: "<media> <port> <proto>
// <fmt>...". It never fails; unparseable fields fall back to their zero
// value or are skipped.
func parseMediaLine(val string) Media {
	m := Media{
		Kind:    audiostream.MediaOther,
		RTPMaps: make(map[int]RTPMap),
		FMTPs:   make(map[int]string),
	}

	fields := strings.Fields(val)
	if len(fields) == 0 {
		return m
	}

	switch fields[0] {
	case "audio":
		m.Kind = audiostream.MediaAudio
	case "video":
		m.Kind = audiostream.MediaVideo
	default:
		m.Kind = audiostream.MediaOther
	}

	if len(fields) > 2 {
		m.Proto = fields[2]
	}

	if len(fields) > 3 {
		for _, tok := range fields[3:] {
			pt, err := strconv.Atoi(tok)
			if err != nil || !validPayloadType(pt) {
				continue
			}
			m.Formats = append(m.Formats, pt)
		}
	}

	return m
}

// parseAttribute dispatches one a= line value to the session (current ==
// nil) or the current media section.
func parseAttribute(val string, s *Session, current *Media) {
	// BINDING TOTALITY RULE (split guard): a valueless attribute such as
	// "a=recvonly" splits to a single element. Only read parts[1] once
	// its presence is confirmed.
	parts := strings.SplitN(val, ":", 2)
	name := parts[0]
	if len(parts) < 2 {
		return
	}
	value := parts[1]

	switch name {
	case "control":
		if current == nil {
			s.Control = value
		} else {
			current.Control = value
		}
	case "rtpmap":
		if current == nil {
			return
		}
		parseRTPMap(value, current)
	case "fmtp":
		if current == nil {
			return
		}
		parseFMTP(value, current)
	case "tool":
		// Session-level only; a per-media a=tool is not standard and is
		// ignored. Keep the first, stored verbatim as untrusted free text.
		if current == nil && s.Tool == "" {
			s.Tool = value
		}
	default:
		// unknown attribute names are ignored
	}
}

// parseRTPMap parses "<pt> <encoding>/<clock>[/<channels>]" and stores the
// result in current.RTPMaps. It skips the whole rtpmap on any malformation.
func parseRTPMap(value string, current *Media) {
	// BINDING TOTALITY RULE (split guard): "a=rtpmap:97" with no space
	// after the payload type splits to a single element.
	parts := strings.SplitN(value, " ", 2)
	if len(parts) < 2 {
		return
	}
	pt, err := strconv.Atoi(parts[0])
	if err != nil || !validPayloadType(pt) {
		return
	}

	// BINDING TOTALITY RULE (split guard): ClockRate is element 1 and
	// Channels is element 2; both must be confirmed present before use.
	segs := strings.Split(parts[1], "/")
	if len(segs) < 2 {
		return
	}
	clock, err := strconv.Atoi(segs[1])
	if err != nil {
		return
	}
	channels := 0
	if len(segs) >= 3 {
		if c, err := strconv.Atoi(segs[2]); err == nil {
			channels = c
		}
	}

	current.RTPMaps[pt] = RTPMap{
		PayloadType:  pt,
		EncodingName: segs[0],
		ClockRate:    clock,
		Channels:     channels,
	}
}

// maxPayloadType is the largest RTP payload type, which the RTP header
// carries in 7 bits (RFC 3550 section 5.1).
const maxPayloadType = 127

// validPayloadType reports whether pt is a payload type an m= line could
// have declared. Attributes naming anything else are skipped, so every
// key in RTPMaps and FMTPs is one Formats can actually contain.
func validPayloadType(pt int) bool {
	return pt >= 0 && pt <= maxPayloadType
}

// parseFMTP parses "<pt> <params...>" and stores the raw params string in
// current.FMTPs. It skips the whole fmtp on any malformation.
func parseFMTP(value string, current *Media) {
	// BINDING TOTALITY RULE (split guard): "a=fmtp:97" with no space
	// after the payload type splits to a single element.
	parts := strings.SplitN(value, " ", 2)
	if len(parts) < 2 {
		return
	}
	pt, err := strconv.Atoi(parts[0])
	if err != nil || !validPayloadType(pt) {
		return
	}
	current.FMTPs[pt] = parts[1]
}
