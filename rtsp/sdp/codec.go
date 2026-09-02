package sdp

import (
	"encoding/base64"
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

// The four plain G.726 rtpmap encoding names (RFC 3551 section 4.5.4), one per
// bit rate. They pack codewords least-significant-bit-first.
const (
	g726Name16 = "G726-16"
	g726Name24 = "G726-24"
	g726Name32 = "G726-32"
	g726Name40 = "G726-40"
)

// The four AAL2-G726 rtpmap encoding names. They carry the same codewords as the
// plain names above at the same bit rates, packed most-significant-bit-first per
// ITU-T I.366.2 Annex E. RFC 3551 section 4.5.4 names these subtypes and defers
// their payload format to a separate document that was never published, so
// neither RFC 3551 nor RFC 4856 registers them.
const (
	aal2G726Name16 = "AAL2-G726-16"
	aal2G726Name24 = "AAL2-G726-24"
	aal2G726Name32 = "AAL2-G726-32"
	aal2G726Name40 = "AAL2-G726-40"
)

// g726ClockRate is the fixed RTP clock rate for G.726 (RFC 3551/4856): all four
// bit rates run at 8 kHz.
const g726ClockRate = 8000

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
	// Codec is the resolved codec: CodecAAC, CodecMP4ALATM, CodecOpus,
	// CodecG711, CodecG726, CodecL16, CodecFLAC, or CodecUnknown. Never nil.
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
// CodecG711, 2 (G.721, equivalent to G.726 at 32 kbps) to CodecG726, and 10
// (L16 stereo 44100) and 11 (L16 mono 44100) to CodecL16, even when no a=rtpmap
// is present.
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
		case 2:
			// RFC 3551 static payload type 2 is G.721, identical to
			// G.726 at 32 kbps. RFC 3551 section 6 deprecates it and marks it
			// reserved precisely "due to conflicting use for the payload formats
			// G726-32 and AAL2-G726-32", so the packing is genuinely ambiguous
			// here and the plain RFC 3551 order is ASSUMED, not signalled. A
			// sender that means the AAL2 order has to say so with an
			// AAL2-G726-32 rtpmap.
			encoding, clock, channels = g726Name32, g726ClockRate, 1
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

	up := strings.ToUpper(encoding)
	switch up {
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
	case "FLAC":
		// FLAC over RTP carries raw FLAC frames; the STREAMINFO a decoder needs is
		// advertised out of band in the fmtp streaminfo= parameter (base64). A
		// missing or malformed streaminfo leaves StreamInfo nil: the track is still
		// FLAC and still depacketizes, and a decoder can recover geometry from the
		// frame headers, so an unusable fmtp must not demote the track.
		t.Codec = audiostream.CodecFLAC{StreamInfo: parseFLACFmtp(m.FMTPs[pt])}
	case "PCMU":
		t.Codec = audiostream.CodecG711{Law: audiostream.MuLaw}
	case "PCMA":
		t.Codec = audiostream.CodecG711{Law: audiostream.ALaw}
	case g726Name16, g726Name24, g726Name32, g726Name40,
		aal2G726Name16, aal2G726Name24, aal2G726Name32, aal2G726Name40:
		// G.726 is single-channel at an 8 kHz clock (RFC 3551/4856: no channels
		// parameter, 8000 Hz), and the decoder holds one adaptive state, so a
		// multi-channel or non-8 kHz advertisement cannot be decoded or timed
		// correctly. Resolve only the conformant form to CodecG726 and leave any
		// other channel count or clock as CodecUnknown rather than mis-decode it.
		// The AAL2 names resolve to the same bit rates with the AAL2 packing, so
		// the only difference from the plain names is the codeword bit order.
		// They have no registration of their own, so the same conformance rule is
		// applied to them by analogy: same codec, same 8 kHz clock, same single
		// adaptive state.
		if br, pk, ok := resolveG726(up); ok && channels == 1 && clock == g726ClockRate {
			t.Codec = audiostream.CodecG726{BitRate: br, Packing: pk, ClockRate: clock, Channels: channels}
		} else {
			t.Codec = audiostream.CodecUnknown{RTPMap: rtpmapString(encoding, clock, rawChannels, hasRTPMap)}
		}
	case encodingL16:
		t.Codec = audiostream.CodecL16{ClockRate: clock, Channels: channels}
	default:
		t.Codec = audiostream.CodecUnknown{RTPMap: rtpmapString(encoding, clock, rawChannels, hasRTPMap)}
	}

	return t
}

// resolveG726 maps an upper-cased G.726 rtpmap encoding name to its bit rate and
// codeword packing. The plain G726-NN names resolve to the RFC 3551 section
// 4.5.4 packing and the AAL2-G726-NN names to the ITU-T I.366.2 Annex E packing
// at the same four bit rates. ok is false for any other name.
func resolveG726(up string) (audiostream.G726BitRate, audiostream.G726Packing, bool) {
	switch up {
	case g726Name16:
		return audiostream.G726Rate16, audiostream.G726PackingRFC3551, true
	case g726Name24:
		return audiostream.G726Rate24, audiostream.G726PackingRFC3551, true
	case g726Name32:
		return audiostream.G726Rate32, audiostream.G726PackingRFC3551, true
	case g726Name40:
		return audiostream.G726Rate40, audiostream.G726PackingRFC3551, true
	case aal2G726Name16:
		return audiostream.G726Rate16, audiostream.G726PackingAAL2, true
	case aal2G726Name24:
		return audiostream.G726Rate24, audiostream.G726PackingAAL2, true
	case aal2G726Name32:
		return audiostream.G726Rate32, audiostream.G726PackingAAL2, true
	case aal2G726Name40:
		return audiostream.G726Rate40, audiostream.G726PackingAAL2, true
	default:
		return 0, 0, false
	}
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

// parseFmtpPairs splits a semicolon-separated fmtp parameter list into
// key/value pairs and invokes fn for each. The key is lowercased for
// case-insensitive matching and the value is trimmed but otherwise verbatim.
//
// BINDING TOTALITY RULE (split guard): a bare flag parameter with no '=' (for
// example "cpresent") splits to a single element and is skipped here, never
// indexed at [1]. Centralizing the guard means every fmtp parser inherits it.
func parseFmtpPairs(params string, fn func(key, value string)) {
	for _, elem := range strings.Split(params, ";") {
		kv := strings.SplitN(elem, "=", 2)
		if len(kv) < 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(kv[0]))
		value := strings.TrimSpace(kv[1])
		fn(key, value)
	}
}

// parseAACFmtp parses a semicolon-separated MPEG4-GENERIC fmtp parameter
// list. Keys are matched case-insensitively; unknown keys are ignored.
// It never fails: a missing or non-hex config yields a nil Config, and
// missing numeric fields stay 0.
func parseAACFmtp(params string) *AACParams {
	p := &AACParams{}
	parseFmtpPairs(params, func(key, value string) {
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
	})
	return p
}

// streamInfoLen is the fixed size of a FLAC STREAMINFO metadata block, in bytes
// (RFC 9639 section 8.2). A base64 streaminfo fmtp value that decodes to any
// other length is rejected as malformed.
const streamInfoLen = 34

// parseFLACFmtp extracts the STREAMINFO block from a FLAC fmtp parameter string,
// returning the base64-decoded bytes only when they form a complete 34-byte
// STREAMINFO, and nil when no streaminfo= parameter is present or its value does
// not decode to exactly that length. It never errors: an absent or malformed
// STREAMINFO leaves a FLAC track playable (a decoder can recover geometry from
// the frame headers), so describeTrack keeps the track as CodecFLAC with a nil
// StreamInfo rather than demoting it. Both padded and unpadded base64 are
// accepted, since senders differ on whether they include the trailing '='.
func parseFLACFmtp(params string) []byte {
	var streamInfo []byte
	parseFmtpPairs(params, func(key, value string) {
		if key != "streaminfo" || value == "" {
			// An absent or empty streaminfo leaves StreamInfo nil, as CodecFLAC
			// documents: base64-decoding "" yields a non-nil zero-length slice,
			// which would contradict that nil and hand a decoder a 0-byte STREAMINFO.
			return
		}
		b, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			b, err = base64.RawStdEncoding.DecodeString(value)
		}
		// Keep the decoded block only when it is a complete STREAMINFO (RFC 9639
		// section 8.2 fixes it at 34 bytes). A value that fails to decode or
		// decodes to any other length is malformed metadata: leave StreamInfo nil
		// so the track stays FLAC and a decoder recovers geometry from the frame
		// headers rather than trusting a truncated or oversized block.
		if err == nil && len(b) == streamInfoLen {
			streamInfo = b
		}
	})
	return streamInfo
}

// parseLATMFmtp parses a semicolon-separated MP4A-LATM fmtp parameter list,
// mirroring parseAACFmtp: keys are matched case-insensitively, unknown keys
// are ignored, and a bare flag with no '=' is skipped rather than indexed out
// of range. It never fails: a missing or non-hex config yields a nil Config,
// a missing or non-numeric object stays 0, and a missing or non-numeric
// cpresent leaves the RFC 3016 default of present (true).
func parseLATMFmtp(params string) *LATMParams {
	p := &LATMParams{Cpresent: true}
	parseFmtpPairs(params, func(key, value string) {
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
	})
	return p
}
