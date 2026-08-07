package audiostream

// AudioFormat is the source-agnostic description of a track's audio format. Both
// the rtsp and httpsource clients report it so a consumer can decide how to
// consume Frame.Data without knowing which protocol produced it.
//
// The library depacketizes and reports. It never decodes a complex compressed
// codec (such as AAC or Opus) and never resamples, so those arrive as
// KindCompressed for the consumer to decode. It does expand companded G.711 and
// byte-swap L16 into little-endian s16le PCM as part of depacketization, which
// is why those arrive as KindPCMS16LE. Decoding compressed codecs and
// resampling are always the consumer's responsibility.
//
// SampleRate and Channels are meaningful ONLY when Kind is KindPCMS16LE, where
// they describe the PCM the library delivers and are trustworthy because the
// library produced that PCM. For every other kind they are 0: the transport's
// advertised rate and channel count cannot be trusted for compressed audio (an
// AAC RTP clock is frequently 90000 due to video multiplexing or camera
// firmware, and Opus is fixed at 48000/2 in SDP while the true output geometry
// comes from the bitstream), so a consumer must obtain the real geometry from
// its decoder. Codec carries what the decoder needs for that; for AAC that is
// the AudioSpecificConfig bytes in CodecAAC.
//
// AudioFormat is deliberately not comparable with ==. Codec may hold a CodecAAC
// whose AudioSpecificConfig is a []byte, so == would panic at runtime for an AAC
// track; the zero-width field below turns that mistake into a compile error.
// Compare the scalar fields and the Codec's dynamic type instead.
type AudioFormat struct {
	_ [0]func()
	// Codec is the track's payload codec (a sealed sum type).
	Codec Codec
	// Kind is the form Frame.Data takes for this track.
	Kind PayloadKind
	// SampleRate is the PCM sample rate in Hz, nonzero only when Kind is
	// KindPCMS16LE.
	SampleRate int
	// Channels is the PCM channel count, nonzero only when Kind is KindPCMS16LE.
	Channels int
}

// PayloadKind is the form the bytes in a delivered Frame.Data take. Its zero
// value is KindUnknown, so an uninitialized AudioFormat is never mistaken for a
// decodable one.
type PayloadKind uint8

const (
	// KindUnknown is the zero value: the payload form is not determined. It is
	// what an uninitialized AudioFormat reports and what PayloadKindFor returns
	// for a nil or unclassified codec. A consumer must not treat it as decodable.
	KindUnknown PayloadKind = iota
	// KindCompressed means Frame.Data is a decodable compressed bitstream for
	// the track's Codec: one AAC access unit, one Opus packet, and so on. The
	// consumer must decode it.
	KindCompressed
	// KindPCMS16LE means Frame.Data is interleaved little-endian signed 16-bit
	// PCM, ready to use without decoding. AudioFormat.SampleRate and Channels
	// describe it.
	KindPCMS16LE
	// KindOpaque means Frame.Data is a raw RTP payload the library did not
	// depacketize, because it did not recognize the codec (CodecUnknown). It is
	// not a clean decodable bitstream: it still carries RTP framing and the
	// library cannot say what it is.
	KindOpaque
)

// String returns a short lowercase label for logs and diagnostics. A value
// outside the defined kinds reports "unknown".
func (k PayloadKind) String() string {
	switch k {
	case KindUnknown:
		return "unknown"
	case KindCompressed:
		return "compressed"
	case KindPCMS16LE:
		return "pcm-s16le"
	case KindOpaque:
		return "opaque"
	default:
		return unknownName
	}
}

// PayloadKindFor maps a codec to the form the library delivers it in. It is the
// single source of truth for that mapping, used to populate AudioFormat.Kind.
//
// A nil codec, or a future codec not yet classified here, returns KindUnknown:
// the honest "cannot classify this" answer, and a safe one, since a consumer
// must not decode an unknown kind. CodecUnknown is distinct: the library does
// recognize the track but not its codec and delivers the raw RTP payload, so it
// maps to KindOpaque.
func PayloadKindFor(c Codec) PayloadKind {
	switch c.(type) {
	case CodecG711, CodecL16:
		return KindPCMS16LE
	case CodecAAC, CodecOpus:
		return KindCompressed
	case CodecUnknown:
		return KindOpaque
	default: // nil, and any codec not yet classified.
		return KindUnknown
	}
}
