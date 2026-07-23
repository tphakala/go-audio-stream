package audiostream

import "errors"

// ErrClosed is returned by session wait/receive operations after the
// session was closed locally via Close.
var ErrClosed = errors.New("audiostream: session closed")

// ErrReadTimeout is returned when no stream data arrived within the
// configured read-idle window; the stream is considered dead and the
// consumer should reconnect.
var ErrReadTimeout = errors.New("audiostream: read timeout")

// RedirectError reports a server redirect. Protocol clients surface it
// and never follow redirects themselves; the consumer decides.
type RedirectError struct {
	// Location is the redirect target as sent by the server.
	Location string
}

func (e *RedirectError) Error() string {
	return "audiostream: server redirected to " + e.Location
}
