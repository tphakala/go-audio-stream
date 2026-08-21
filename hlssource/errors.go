package hlssource

import (
	"errors"
	"strconv"
)

// Package-typed open-phase and terminal causes, matched with errors.Is against
// these sentinels. The taxonomy mirrors httpsource where the meaning is the
// same, so a consumer that already handles the HTTP source recognizes the
// shared cases. Terminal read-idle and local-close causes are the shared
// audiostream.ErrReadTimeout and audiostream.ErrClosed, not redefined here.
var (
	// ErrInvalidURL reports a Config.URL that is empty, unparseable, carries a
	// scheme other than http or https, is missing a host, has an out-of-range
	// port, or embeds credentials containing CR, LF or NUL. Open-phase.
	ErrInvalidURL = errors.New("hlssource: invalid URL")
	// ErrConnectionClosed wraps the underlying cause when a playlist or segment
	// download was lost unexpectedly (a reset or a truncated body), or when a
	// request failed for a reason that is not a timeout, a caller cancellation,
	// or a bad status.
	ErrConnectionClosed = errors.New("hlssource: connection closed")
	// ErrRequestTimeout ends the open phase when a request did not complete
	// within Config.Timeout. It is the open-phase counterpart to
	// audiostream.ErrReadTimeout, which covers the read-idle watchdog.
	ErrRequestTimeout = errors.New("hlssource: request timeout")
	// ErrStreamEnded is the terminal cause for an orderly end of stream: a VOD
	// playlist (EXT-X-ENDLIST) whose segments have all been delivered.
	ErrStreamEnded = errors.New("hlssource: stream ended")
	// ErrBadStatus is the sentinel a *StatusError matches under errors.Is, so a
	// caller can test for a non-success response without recovering the code. It
	// covers a non-200 response that is not a 3xx redirect the client followed.
	ErrBadStatus = errors.New("hlssource: non-success response status")
	// ErrMalformedPlaylist reports a body that is not a valid m3u8 playlist: a
	// missing #EXTM3U header, a media playlist with no target duration, an
	// unparseable EXTINF, a segment URI with no preceding EXTINF, or a body that
	// is neither a media nor a master playlist.
	ErrMalformedPlaylist = errors.New("hlssource: malformed playlist")
	// ErrUnsupportedPlaylist reports a valid playlist this source will not play:
	// encrypted content (EXT-X-KEY with a method other than NONE), an fMP4
	// initialization segment (EXT-X-MAP), a byte-range segment (EXT-X-BYTERANGE),
	// or a master playlist with no audio-bearing option.
	ErrUnsupportedPlaylist = errors.New("hlssource: unsupported playlist")
	// ErrMalformedSegment reports a segment whose MPEG-TS structure could not be
	// parsed: no 0x47 sync, no PAT or PMT, no audio PID, or no ADTS access unit
	// in the elementary stream.
	ErrMalformedSegment = errors.New("hlssource: malformed segment")
	// ErrUnsupportedCodec reports a segment whose audio elementary stream is not
	// AAC in ADTS (the only audio this source demuxes today): an MP3, LATM, or
	// other stream_type in the PMT.
	ErrUnsupportedCodec = errors.New("hlssource: unsupported audio codec")
	// ErrPlaylistTooLarge reports a playlist body exceeding Config.MaxPlaylistBytes.
	ErrPlaylistTooLarge = errors.New("hlssource: playlist exceeds size limit")
	// ErrSegmentTooLarge reports a segment body exceeding Config.MaxSegmentBytes.
	ErrSegmentTooLarge = errors.New("hlssource: segment exceeds size limit")
)

// StatusError reports a non-success HTTP status. It matches errors.Is against
// ErrBadStatus; use errors.As to recover the Code and Status.
type StatusError struct {
	// Code is the numeric HTTP status (for example 404).
	Code int
	// Status is the server's full status line ("404 Not Found"), "" when the
	// transport did not provide one.
	Status string
}

// Error satisfies error.
func (e *StatusError) Error() string {
	if e.Status == "" {
		return "hlssource: unexpected status " + strconv.Itoa(e.Code)
	}
	return "hlssource: unexpected status " + e.Status
}

// Is reports whether target is ErrBadStatus, so any *StatusError matches
// errors.Is(err, ErrBadStatus).
func (e *StatusError) Is(target error) bool {
	return target == ErrBadStatus
}
