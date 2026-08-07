package udpsource

import "errors"

// Package-typed causes, matched with errors.Is. UDP is connectionless, so there
// is no orderly end-of-stream: a source ends via Close (audiostream.ErrClosed),
// the read-idle watchdog (audiostream.ErrReadTimeout), a Wait-context
// cancellation (ctx.Err()), or a socket failure (ErrConnectionClosed).
var (
	// ErrInvalidConfig reports a Config that cannot bind or interpret datagrams:
	// an empty or unparseable ListenAddr, an unparseable SourceIP, a ModeRTP
	// source missing its ClockRate, or a ModePCM source missing its sample rate
	// or channel count. It is returned by Open.
	ErrInvalidConfig = errors.New("udpsource: invalid config")
	// ErrUnsupportedCodec reports a Config.Codec this source does not carry over
	// raw RTP. The MVP frames G.711, L16, and Opus, and passes an unrecognized
	// codec through opaquely; AAC and MP3 over raw RTP are not yet supported.
	ErrUnsupportedCodec = errors.New("udpsource: unsupported codec")
	// ErrBind reports that the UDP socket could not be bound to ListenAddr.
	ErrBind = errors.New("udpsource: cannot bind UDP socket")
	// ErrConnectionClosed wraps the underlying cause when the socket read failed
	// for a reason other than the read-idle watchdog or a local Close.
	ErrConnectionClosed = errors.New("udpsource: connection closed")
)
