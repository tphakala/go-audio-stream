package audiostream

// MediaKind identifies the media type of a stream track.
type MediaKind uint8

const (
	// MediaUnknown marks a track whose media type was not recognized.
	MediaUnknown MediaKind = iota
	// MediaAudio marks an audio track.
	MediaAudio
	// MediaVideo marks a video track.
	MediaVideo
	// MediaOther marks a declared but non-audio, non-video track
	// (for example application or text media in SDP).
	MediaOther
)

// unknownName labels a value that falls outside the defined set of an
// enum, so an out-of-range value is never reported as a real one.
const unknownName = "unknown"

// String returns a lowercase name for the media kind. MediaUnknown and
// any value outside the defined kinds report "unknown".
func (k MediaKind) String() string {
	switch k {
	case MediaAudio:
		return "audio"
	case MediaVideo:
		return "video"
	case MediaOther:
		return "other"
	default:
		return unknownName
	}
}

// Law selects a G.711 companding law.
type Law uint8

const (
	// MuLaw is G.711 mu-law companding (RTP payload type PCMU).
	MuLaw Law = iota
	// ALaw is G.711 A-law companding (RTP payload type PCMA).
	ALaw
)

// String returns a lowercase name for the companding law. A value
// outside the defined laws reports "unknown" rather than defaulting to
// a real law, so a malformed value is never mislabeled as audio data
// the caller can trust.
func (l Law) String() string {
	switch l {
	case MuLaw:
		return "mu-law"
	case ALaw:
		return "a-law"
	default:
		return unknownName
	}
}

// G726BitRate selects one of the four ITU-T G.726 ADPCM bit rates. The rate
// fixes the codeword width on the wire (2, 3, 4, or 5 bits per sample for 16,
// 24, 32, and 40 kbps), which a depacketizer needs to unpack the RTP payload.
// It lives in this package (like Law) so CodecG726 can carry it without the
// root package importing depacket/g726.
type G726BitRate uint8

const (
	// G726Rate16 is 16 kbps G.726 (2 bits per codeword).
	G726Rate16 G726BitRate = iota
	// G726Rate24 is 24 kbps G.726 (3 bits per codeword).
	G726Rate24
	// G726Rate32 is 32 kbps G.726 (4 bits per codeword). RFC 3551 static
	// payload type 2 (G.721) is this rate.
	G726Rate32
	// G726Rate40 is 40 kbps G.726 (5 bits per codeword).
	G726Rate40
)

// String returns a short label like "32 kbps". A value outside the defined
// rates reports "unknown" rather than a plausible rate, so a malformed value is
// never mislabeled.
func (r G726BitRate) String() string {
	switch r {
	case G726Rate16:
		return "16 kbps"
	case G726Rate24:
		return "24 kbps"
	case G726Rate32:
		return "32 kbps"
	case G726Rate40:
		return "40 kbps"
	default:
		return unknownName
	}
}
