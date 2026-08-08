package sdp

import (
	"encoding/hex"
	"strconv"
	"strings"

	audiostream "github.com/tphakala/go-audio-stream"
)

// encodingL16 is the RFC 3551 rtpmap encoding name for 16-bit linear PCM. It is
// a constant because the literal occurs three times (the encoding switch plus
// the two static payload types 10 and 11), which the goconst linter flags,
// whereas PCMU/PCMA occur twice each and stay inline.
const encodingL16 = "L16"

// DescribedTrack is one media section resolved to a codec identity with
// its clock rate, channel count, and control URL. It is what the RTSP
// client turns into a Track.
type DescribedTrack struct {
	// Media is the media kind from the m= line.
	Media audiostream.MediaKind
	// PayloadType is the primary payload type this track was resolved
	// from (the first entry of the m= format list), or -1 if the section
	// listed no formats.
	PayloadType int
	// Codec is the resolved codec: CodecAAC, CodecOpus, CodecG711, CodecL16,
	// or CodecUnknown. Never nil.
	Codec audiostream.Codec
	// ClockRate is the RTP clock rate in Hz, 0 if unknown.
	ClockRate int
	// Channels is the channel count, defaulting to 1 when the rtpmap
	// omitted it and 0 only when no codec information was available.
	Channels int
	// Control is the media-level a=control value, "" if absent.
	Control string
	// FMTP is the raw a=fmtp parameter string for PayloadType (everything after
	// the payload type), "" when the section carried no fmtp for it. Retained
	// verbatim for diagnostics; the codec resolution uses the parsed forms.
	FMTP string
	// AAC holds the parsed MPEG4-GENERIC fmtp parameters when Codec is a
	// CodecAAC, and is nil for every other codec.
	AAC *AACParams
	// LATM holds the parsed MP4A-LATM fmtp parameters when Codec is a
	// CodecMP4ALATM, and is nil for every other codec.
	LATM *LATMParams
}

// AACParams holds the RFC 3640 MPEG4-GENERIC fmtp parameters the AAC
// depacketizer (M3) needs. Numeric fields are 0 when the fmtp omitted
// them; Config is nil when config= was absent or not valid hex.
type AACParams struct {
	// SizeLength is the AU-header sizelength in bits.
	SizeLength int
	// IndexLength is the AU-header indexlength in bits.
	IndexLength int
	// IndexDeltaLength is the AU-header indexdeltalength in bits.
	IndexDeltaLength int
	// Mode is the raw fmtp mode value as received, for example "AAC-hbr".
	// The depacketizer validates it case-insensitively; the parser only
	// captures it.
	Mode string
	// Config is the decoded AudioSpecificConfig from the config= hex, the
	// same bytes stored in CodecAAC.AudioSpecificConfig.
	Config []byte
}

// LATMParams holds the RFC 3016 MP4A-LATM fmtp parameters. Config is the
// decoded StreamMuxConfig from the config= hex (nil when absent or not valid
// hex); Cpresent mirrors the cpresent parameter (default true when absent);
// Object is the audio object type from object= (0 when absent).
type LATMParams struct {
	// Config is the decoded StreamMuxConfig from the config= hex, the same
	// bytes stored in CodecMP4ALATM.StreamMuxConfig.
	Config []byte
	// Cpresent mirrors the fmtp cpresent parameter: true means each
	// AudioMuxElement carries its StreamMuxConfig in-band, false means it is
	// carried out-of-band in Config. Absent defaults to true (RFC 3016).
	Cpresent bool
	// Object is the audio object type from the fmtp object= parameter, 0
	// when absent.
	Object int
}

// Codecs resolves every media section to a DescribedTrack. It never
// fails: an unrecognized encoding maps to CodecUnknown, and a media
// section with no formats yields a CodecUnknown track with PayloadType
// -1. RFC 3551 static payload types 0 (PCMU) and 8 (PCMA) resolve to
// CodecG711, and 10 (L16 stereo 44100) and 11 (L16 mono 44100) resolve to
// CodecL16, even when no a=rtpmap is present.
func (s *Session) Codecs() []DescribedTrack {
	tracks := make([]DescribedTrack, 0, len(s.Media))
	for i := range s.Media {
		tracks = append(tracks, describeTrack(&s.Media[i]))
	}
	return tracks
}

// describeTrack resolves one media section to a DescribedTrack.
func describeTrack(m *Media) DescribedTrack {
	t := DescribedTrack{
		Media:   m.Kind,
		Control: m.Control,
	}

	if len(m.Formats) == 0 {
		t.PayloadType = -1
		t.Codec = audiostream.CodecUnknown{}
		return t
	}

	pt := m.Formats[0]
	t.PayloadType = pt
	t.FMTP = m.FMTPs[pt]

	rm, hasRTPMap := m.RTPMaps[pt]
	encoding := rm.EncodingName
	clock := rm.ClockRate
	channels := rm.Channels
	if !hasRTPMap {
		switch pt {
		case 0:
			encoding, clock, channels = "PCMU", 8000, 1
		case 8:
			encoding, clock, channels = "PCMA", 8000, 1
		case 10:
			encoding, clock, channels = encodingL16, 44100, 2
		case 11:
			encoding, clock, channels = encodingL16, 44100, 1
		default:
			encoding, clock, channels = "", 0, 0
		}
	}

	// rawChannels reflects exactly what the rtpmap (or static default)
	// reported, before the "default to 1" normalization below. CodecUnknown.RTPMap
	// reconstructs the rtpmap string as received, so it must omit the
	// channel segment when the source rtpmap omitted it.
	rawChannels := channels

	// Default a missing (0) or nonsensical (negative) channel count to 1 for any
	// recognized encoding. The rtpmap channel segment is a plain Atoi (parse.go),
	// so a value like L16/44100/-2 parses to a negative; left unclamped it would
	// surface on the exported CodecL16.Channels/Track.Channels and feed the L16
	// frame-size math.
	if channels <= 0 && encoding != "" {
		channels = 1
	}

	t.ClockRate = clock
	t.Channels = channels

	switch strings.ToUpper(encoding) {
	case "MPEG4-GENERIC":
		params := parseAACFmtp(m.FMTPs[pt])
		t.Codec = audiostream.CodecAAC{AudioSpecificConfig: params.Config}
		t.AAC = params
	case "MP4A-LATM":
		params := parseLATMFmtp(m.FMTPs[pt])
		t.Codec = audiostream.CodecMP4ALATM{StreamMuxConfig: params.Config, MuxConfigPresent: params.Cpresent}
		t.LATM = params
	case "OPUS":
		t.Codec = audiostream.CodecOpus{}
	case "PCMU":
		t.Codec = audiostream.CodecG711{Law: audiostream.MuLaw}
	case "PCMA":
		t.Codec = audiostream.CodecG711{Law: audiostream.ALaw}
	case encodingL16:
		t.Codec = audiostream.CodecL16{ClockRate: clock, Channels: channels}
	default:
		t.Codec = audiostream.CodecUnknown{RTPMap: rtpmapString(encoding, clock, rawChannels, hasRTPMap)}
	}

	return t
}

// rtpmapString reconstructs the "<encoding>/<clock>[/<channels>]" form for
// CodecUnknown.RTPMap. It returns "" when no rtpmap was present (static
// defaults with an unrecognized payload type carry no encoding name).
func rtpmapString(encoding string, clock, channels int, hasRTPMap bool) string {
	if !hasRTPMap {
		return ""
	}
	s := encoding + "/" + strconv.Itoa(clock)
	if channels != 0 {
		s += "/" + strconv.Itoa(channels)
	}
	return s
}

// parseAACFmtp parses a semicolon-separated MPEG4-GENERIC fmtp parameter
// list. Keys are matched case-insensitively; unknown keys are ignored.
// It never fails: a missing or non-hex config yields a nil Config, and
// missing numeric fields stay 0.
func parseAACFmtp(params string) *AACParams {
	p := &AACParams{}
	for _, elem := range strings.Split(params, ";") {
		// BINDING TOTALITY RULE (split guard): a bare flag parameter with
		// no '=' (for example "cpresent") splits to a single element and
		// must be skipped, never indexed at [1].
		kv := strings.SplitN(elem, "=", 2)
		if len(kv) < 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(kv[0]))
		value := strings.TrimSpace(kv[1])

		switch key {
		case "sizelength":
			if n, err := strconv.Atoi(value); err == nil {
				p.SizeLength = n
			}
		case "indexlength":
			if n, err := strconv.Atoi(value); err == nil {
				p.IndexLength = n
			}
		case "indexdeltalength":
			if n, err := strconv.Atoi(value); err == nil {
				p.IndexDeltaLength = n
			}
		case "mode":
			p.Mode = value
		case "config":
			if b, err := hex.DecodeString(value); err == nil {
				p.Config = b
			}
		}
	}
	return p
}

// parseLATMFmtp parses a semicolon-separated MP4A-LATM fmtp parameter list,
// mirroring parseAACFmtp: keys are matched case-insensitively, unknown keys
// are ignored, and a bare flag with no '=' is skipped rather than indexed out
// of range. It never fails: a missing or non-hex config yields a nil Config,
// a missing or non-numeric object stays 0, and a missing or non-numeric
// cpresent leaves the RFC 3016 default of present (true).
func parseLATMFmtp(params string) *LATMParams {
	p := &LATMParams{Cpresent: true}
	for _, elem := range strings.Split(params, ";") {
		// BINDING TOTALITY RULE (split guard): a bare flag parameter with no
		// '=' splits to a single element and must be skipped, never indexed
		// at [1].
		kv := strings.SplitN(elem, "=", 2)
		if len(kv) < 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(kv[0]))
		value := strings.TrimSpace(kv[1])

		switch key {
		case "cpresent":
			if n, err := strconv.Atoi(value); err == nil {
				p.Cpresent = n != 0
			}
		case "object":
			if n, err := strconv.Atoi(value); err == nil {
				p.Object = n
			}
		case "config":
			if b, err := hex.DecodeString(value); err == nil {
				p.Config = b
			}
		}
	}
	return p
}
