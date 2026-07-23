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

// String returns a lowercase name for the media kind.
func (k MediaKind) String() string {
	switch k {
	case MediaAudio:
		return "audio"
	case MediaVideo:
		return "video"
	case MediaOther:
		return "other"
	case MediaUnknown:
		return "unknown"
	default:
		return "unknown"
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

// String returns a lowercase name for the companding law.
func (l Law) String() string {
	if l == ALaw {
		return "a-law"
	}
	return "mu-law"
}
