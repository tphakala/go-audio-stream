// Package httpsource is an HTTP(S) progressive audio source for
// go-audio-stream. It opens a single GET against an endpoint that streams
// linear PCM, resolves the audio format from the response, and delivers
// little-endian s16le PCM frames to Config.OnFrame on a reader goroutine, the
// same frame shape the rtsp client delivers. A Client satisfies
// audiostream.Source, so a supervisor can drive its lifecycle and read its
// statistics and identity without importing this package.
//
// Two body formats are supported. A WAV response (audio/wav and its aliases, or
// a body sniffed to begin with a RIFF/WAVE signature) is parsed by a minimal
// streaming RIFF parser that requires 16-bit integer PCM; its fmt chunk is
// authoritative for the rate and channel count, and a bounded data chunk ends
// the stream when its declared bytes are consumed. A raw response (audio/L16,
// or an unlabeled application/octet-stream or audio/pcm) carries no header, so
// its shape comes from the Content-Type parameters and Config.Format: audio/L16
// is big-endian per RFC 3551 and byte-swapped to little-endian on delivery,
// while unlabeled embedded PCM is taken as native little-endian. An explicit
// Config.Format.Endian overrides the implied byte order. The precedence for
// rate and channels is WAV header, then Content-Type parameters, then
// Config.Format; an unresolvable shape fails Open with ErrFormatUnknown rather
// than being guessed.
//
// Compressed and container formats are out of scope by design. A Content-Type
// this source does not carry (audio/mpeg, audio/aac, audio/ogg and the rest)
// fails Open with ErrUnsupportedFormat rather than delivering bytes it cannot
// turn into PCM, and the 64-bit RIFF variants RF64 and BW64 are rejected the
// same way. There is no Icecast or SHOUTcast metadata handling and no MP3
// decoding; this source moves PCM, nothing else.
//
// Open performs the whole handshake and returns an already-delivering source.
// Its ctx bounds only the open phase and is divorced from the streaming request
// (context.WithoutCancel), so cancelling ctx after Open returns does not end the
// stream; Close does. A Client holds a socket and a goroutine and must be
// released with Close; Wait reports the terminal cause. Close, Stats, Info and
// Codec are safe from any goroutine, including from inside OnFrame; Wait is safe
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
