package httpsource

import (
	"errors"
	"strconv"
)

// Package-typed terminal and open-phase causes. They are matched with
// errors.Is against these sentinels; the taxonomy is deliberately flat and
// self-contained, so, unlike the WAV truncation case, ErrStreamEnded does not
// alias io.EOF.
var (
	// ErrInvalidURL reports a Config.URL that is empty, unparseable, carries a
	// scheme other than http or https, is missing a host, has an out-of-range
	// port, or embeds credentials containing CR, LF or NUL. It is an open-phase
	// error returned by Open.
	ErrInvalidURL = errors.New("httpsource: invalid URL")
	// ErrConnectionClosed wraps the underlying cause when the response body was
	// lost unexpectedly (a reset, a truncated chunked stream, or a mid-stream
	// server abort), or when the open-phase request failed for a reason that is
	// not a timeout, a caller cancellation, a redirect, or a bad status.
	ErrConnectionClosed = errors.New("httpsource: connection closed")
	// ErrRequestTimeout ends the open phase when the request did not complete
	// within Config.Timeout. It is the open-phase counterpart to
	// audiostream.ErrReadTimeout, which covers the read-idle watchdog: both mean
	// the peer went quiet.
	ErrRequestTimeout = errors.New("httpsource: request timeout")
	// ErrStreamEnded is the terminal cause for an orderly end of stream: a clean
	// EOF, or a WAV data chunk whose declared size has been fully consumed.
	ErrStreamEnded = errors.New("httpsource: stream ended")
	// ErrBadStatus is the sentinel a *StatusError matches under errors.Is, so a
	// caller can test for a non-success response without recovering the code. It
	// covers a non-200 response that is not a 3xx redirect; a redirect surfaces
	// as *audiostream.RedirectError instead, so ErrBadStatus is not a catch-all
	// for every failed request.
	ErrBadStatus = errors.New("httpsource: non-success response status")
	// ErrUnsupportedFormat reports a response this source will not decode: a
	// Content-Type it does not carry (audio/mpeg, audio/aac, audio/ogg and the
	// rest), a WAV audio format other than 16-bit integer PCM (including a
	// WAVE_FORMAT_EXTENSIBLE chunk whose cbSize is smaller than 22, whose
	// SubFormat is not PCM, or whose valid or container bits per sample is not
	// 16), or an RF64/BW64 container. It fails Open fast rather than delivering
	// garbage.
	ErrUnsupportedFormat = errors.New("httpsource: unsupported media format")
	// ErrFormatUnknown reports raw audio whose sample rate and channel count
	// could not be resolved from the Content-Type parameters or Config.Format,
	// or a resolved shape outside the supported bounds. The format is never
	// guessed, so an unresolvable shape is an open-phase error.
	ErrFormatUnknown = errors.New("httpsource: audio format could not be determined")
	// ErrMalformedWAV reports a WAV stream whose RIFF structure could not be
	// parsed: a missing RIFF/WAVE signature, a data chunk before fmt, a fmt
	// chunk that is too small or whose WAVE_FORMAT_EXTENSIBLE cbSize overruns
	// the chunk, a header that exceeds the pre-data budget, or a truncation
	// (which wraps io.ErrUnexpectedEOF).
	ErrMalformedWAV = errors.New("httpsource: malformed WAV stream")
	// ErrInsecureAuth reports that Open refused to send Basic credentials over a
	// plaintext http connection. Credentials on a plaintext connection travel in
	// the clear, so this source refuses by default; set Config.AllowInsecureAuth
	// to permit it (for a trusted local network), or use https.
	ErrInsecureAuth = errors.New("httpsource: refusing to send credentials over a plaintext http connection; set Config.AllowInsecureAuth to allow")
)

// StatusError reports a non-success HTTP status that is not a 3xx redirect. It
// matches errors.Is against ErrBadStatus; use errors.As to recover the Code
// and Status.
type StatusError struct {
	// Code is the numeric HTTP status (for example 401 or 404).
	Code int
	// Status is the server's full status line ("404 Not Found"), "" when the
	// transport did not provide one.
	Status string
}

// Error satisfies error.
func (e *StatusError) Error() string {
	if e.Status == "" {
		return "httpsource: unexpected status " + strconv.Itoa(e.Code)
	}
	return "httpsource: unexpected status " + e.Status
}

// Is reports whether target is ErrBadStatus, so any *StatusError matches
// errors.Is(err, ErrBadStatus).
func (e *StatusError) Is(target error) bool {
	return target == ErrBadStatus
}
