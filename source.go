package audiostream

import "context"

// SourceInfo is a source-neutral snapshot of a capture source's identity,
// for supervisor diagnostics. It is deliberately minimal: protocol detail
// (RTSP session, auth and channel state; HTTP response metadata) stays on
// the concrete client's own snapshot type, reached via type assertion.
type SourceInfo struct {
	// URL is the target the source was opened against, with any
	// credentials stripped. It is stable for the life of the source.
	URL string
	// Server is the peer's self-identification (the RTSP or HTTP Server
	// response header), "" when the peer did not send one.
	Server string
}

// Source is the contract shared by every running capture source: one
// delivering session, already constructed and negotiated by its own
// package (rtsp.Dial plus the RTSP handshake today; an HTTP progressive
// source later). Frame delivery is not part of the contract because it is
// configured at construction via each source's Config.OnFrame, so
// delivery is race-free no matter how early the peer starts sending.
//
// Terminal causes are typed. Wait returns ErrClosed after Close (unless
// an earlier cause won, since the first cause wins), the Wait context's
// ctx.Err() when it cancels first, and an error matching ErrReadTimeout
// when the source's configured read-idle watchdog expired, so a
// supervisor gets the same "peer went quiet" signal from every source.
// Protocol-specific causes (for example rtsp.ErrServerTeardown) surface
// as themselves; the contract is that they are typed and matchable with
// errors.Is and errors.As, not that the list is closed.
//
// Close, Stats and Info are safe from any goroutine, including from
// inside the OnFrame callback. Wait must NOT be called from inside
// OnFrame: OnFrame runs on the reader goroutine and Wait blocks until
// that goroutine has stopped delivering, so calling Wait from the
// callback deadlocks permanently.
type Source interface {
	// Wait blocks until the session ends and returns the terminal
	// cause. After Wait returns, OnFrame will not be called again. Do
	// not call Wait from inside OnFrame; it deadlocks (see the type doc).
	Wait(ctx context.Context) error
	// Close ends the session. It is idempotent and safe from any
	// goroutine, including from inside OnFrame.
	Close() error
	// Stats returns a snapshot of per-track receive counters in freshly
	// allocated memory, keyed by track ID. Counters a source has no
	// equivalent for stay zero.
	Stats() Stats
	// Info returns a source-neutral identity snapshot.
	Info() SourceInfo
}
