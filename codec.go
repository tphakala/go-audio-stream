package audiostream

// Codec identifies a track's payload format. It is a sealed sum type:
// the variants below are the only implementations.
type Codec interface {
	isCodec()
}

// CodecAAC is MPEG-4 AAC audio. AudioSpecificConfig carries the raw ASC
// bytes from the SDP fmtp config attribute, ready to hand to a decoder
// such as github.com/tphakala/go-aac.
type CodecAAC struct {
	AudioSpecificConfig []byte
}

func (CodecAAC) isCodec() {}

// CodecOpus is Opus audio (RFC 7587: always signaled as opus/48000/2).
type CodecOpus struct{}

func (CodecOpus) isCodec() {}

// CodecG711 is G.711 audio; Law distinguishes PCMU from PCMA.
type CodecG711 struct {
	Law Law
}

func (CodecG711) isCodec() {}

// CodecUnknown is a track whose rtpmap was not recognized. It can still
// be set up; its frames carry raw RTP payloads without depacketization.
type CodecUnknown struct {
	RTPMap string
}

func (CodecUnknown) isCodec() {}
