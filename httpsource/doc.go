// Package httpsource is an HTTP(S) progressive audio source for
// go-audio-stream. It opens a single GET against an endpoint that streams
// audio, resolves the audio format from the response, and delivers frames to
// Config.OnFrame on a reader goroutine: little-endian s16le PCM for a PCM
// source, or whole coded frames for a compressed source (MP3), the same frame
// shape the rtsp client delivers. A Client satisfies
// audiostream.Source, so a supervisor can drive its lifecycle and read its
// statistics and identity without importing this package.
//
// Three body formats are supported. A WAV response (audio/wav and its aliases, or
// a body sniffed to begin with a RIFF/WAVE signature) is parsed by a minimal
// streaming RIFF parser that requires 16-bit integer PCM; its fmt chunk is
// authoritative for the rate and channel count, and a bounded data chunk ends
// the stream when its declared bytes are consumed. The fmt chunk may declare
// classic PCM (audioFormat 1) or WAVE_FORMAT_EXTENSIBLE, accepted only when its
// cbSize is at least 22 (carrying the SubFormat GUID), its SubFormat GUID is
// KSDATAFORMAT_SUBTYPE_PCM, and both the container and valid bits per sample
// are 16, so an EXTENSIBLE chunk is admitted only when it is byte-identical
// 16-bit integer PCM; every other EXTENSIBLE subformat is rejected the same
// as any other non-PCM audioFormat, and a chunk whose cbSize overruns the
// chunk size, or that is smaller than 40 bytes, is ErrMalformedWAV. A raw
// response (audio/L16, or an unlabeled application/octet-stream or audio/pcm)
// carries no header, so its shape comes from the Content-Type parameters and
// Config.Format. Raw PCM defaults to little-endian and is delivered verbatim.
// RFC 3551 defines audio/L16 as big-endian, but real HTTP embedded microphones
// (for example esp32-audio-streamer's /stream.pcm) send native little-endian
// while labeling the stream audio/L16, so this source defaults audio/L16 to
// little-endian to match the devices in the field; unlabeled embedded PCM is native
// little-endian for the same reason. Set Config.Format.Endian = EndianBig for a
// spec-strict big-endian audio/L16 source, which is byte-swapped to
// little-endian on delivery. The precedence for rate and channels is WAV header,
// then Content-Type parameters, then Config.Format; an unresolvable shape fails
// Open with ErrFormatUnknown rather than being guessed.
//
// A compressed MP3 response (audio/mpeg or audio/mp3) is framed, not decoded. A
// streaming frame scanner splits the body on MPEG-1/2/2.5 Layer I/II/III frame
// boundaries, skipping a leading ID3v2 tag and resynchronizing past garbage or
// a false sync, and delivers each whole coded frame as one OnFrame call with an
// increasing per-frame PTS. Such a track reports CodecMP3 and KindCompressed,
// with SampleRate and Channels 0 per the AudioFormat contract, since the true
// geometry is the consumer's decoder's to determine. The request omits the
// Icy-MetaData header, so an Icecast or SHOUTcast server streams the audio
// without interleaving ICY metadata blocks; there is no ICY metadata parsing.
//
// Other compressed and container formats remain out of scope by design. A
// Content-Type this source does not carry (audio/aac, audio/ogg and the rest)
// fails Open with ErrUnsupportedFormat rather than delivering bytes it cannot
// frame, and the 64-bit RIFF variants RF64 and BW64 are rejected the same way.
// This source never decodes: it moves PCM or a framed compressed bitstream,
// nothing else.
//
// Open performs the whole handshake and returns an already-delivering source.
// Its ctx bounds only the open phase and is divorced from the streaming request
// (context.WithoutCancel), so cancelling ctx after Open returns does not end the
// stream; Close does. A Client holds a socket and a goroutine and must be
// released with Close; Wait reports the terminal cause. Close, Stats, Info and
// Format are safe from any goroutine, including from inside OnFrame; Wait is safe
// from other goroutines but must not be called from inside OnFrame, which would
// deadlock the reader it waits on. A read-idle watchdog (Config.ReadIdle) ends
// the stream with audiostream.ErrReadTimeout when the peer goes quiet.
//
// Errors are matched with errors.Is against the package sentinels (ErrInvalidURL,
// ErrConnectionClosed, ErrRequestTimeout, ErrStreamEnded, ErrBadStatus,
// ErrUnsupportedFormat, ErrFormatUnknown and ErrMalformedWAV) and the root
// package's audiostream.ErrClosed, audiostream.ErrReadTimeout and
// audiostream.ErrRedirect. Two typed errors match a sentinel through an Is
// method and carry recoverable fields: *StatusError (matching ErrBadStatus) for
// a non-success status code, and the root package's *audiostream.RedirectError
// (matching audiostream.ErrRedirect) for a 3xx Location. Recover either with
// errors.As.
package httpsource
