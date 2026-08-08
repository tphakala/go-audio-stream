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

// CodecMP3 is MPEG-1/2/2.5 Audio Layer III (and the Layer I/II frames that
// share the same framing). httpsource frames a raw MP3 byte stream (an Icecast
// or SHOUTcast radio endpoint, or a progressive MP3 response) into individual
// coded frames; the library never decodes them, so a CodecMP3 track is
// delivered as KindCompressed for the consumer to decode. It carries no
// parameters: an MP3 frame is self-describing (its 4-byte header names the
// sample rate, channel mode, and bitrate), unlike AAC, whose decoder needs the
// out-of-band AudioSpecificConfig.
type CodecMP3 struct{}

func (CodecMP3) isCodec() {}

// CodecG711 is G.711 audio; Law distinguishes PCMU from PCMA.
type CodecG711 struct {
	Law Law
}

func (CodecG711) isCodec() {}

// CodecL16 is uncompressed 16-bit linear PCM (RFC 3551 "L16"). Frames are
// delivered as little-endian s16le PCM: the library byte-swaps the big-endian
// (network byte order) RTP payload before delivery, so an L16 track and a
// G.711 track hand the consumer PCM in the same byte order. ClockRate is the
// RTP clock in Hz and Channels the channel count, both from the rtpmap (or the
// RFC 3551 static payload-type default), enough to interpret the samples.
type CodecL16 struct {
	ClockRate int
	Channels  int
}

func (CodecL16) isCodec() {}

// CodecUnknown is a track whose rtpmap was not recognized. It can still
// be set up; its frames carry raw RTP payloads without depacketization.
type CodecUnknown struct {
	RTPMap string
}

func (CodecUnknown) isCodec() {}

// CodecMP4ALATM is MPEG-4 AAC carried in MP4A-LATM (RFC 3016).
// StreamMuxConfig holds the raw StreamMuxConfig bytes from the SDP config=
// (which for LATM is a StreamMuxConfig, not a bare AudioSpecificConfig), and
// MuxConfigPresent mirrors the fmtp cpresent parameter. A nil StreamMuxConfig
// with MuxConfigPresent true means the config is carried in-band.
//
// AudioSpecificConfig is the AAC ASC bytes a decoder needs. For an
// out-of-band config it is filled in at Describe time (extracted from
// StreamMuxConfig); for an in-band config it is nil on the Track returned by
// Describe and is delivered later through Config.OnCodecUpdate, whose codec
// value carries the resolved ASC. This mirrors CodecAAC.AudioSpecificConfig,
// so a consumer handles both codecs the same way once the field is populated.
type CodecMP4ALATM struct {
	StreamMuxConfig     []byte
	MuxConfigPresent    bool
	AudioSpecificConfig []byte
}

func (CodecMP4ALATM) isCodec() {}
