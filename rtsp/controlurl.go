package rtsp

import (
	"errors"
	"net/url"
	"strings"
)

// ErrInvalidURL means a URL string passed to this package's resolvers could
// not be parsed.
var ErrInvalidURL = errors.New("rtsp: invalid URL")

// ResolveBaseURL computes the connection base URL from the DESCRIBE request
// URL and the response's Content-Base and Content-Location headers, in the
// spec's precedence order: Content-Base, then Content-Location, then the
// request URL. A header value beginning with "/" is resolved as an absolute
// path against the request URL's scheme and host (ignoring any host a
// misbehaving server put after the path); an otherwise-absolute header
// value contributes only its path and query, with scheme and host taken from
// the request URL (firmware bakes wrong LAN or placeholder authorities into
// these headers, and the request-URI must address the host the socket is
// already connected to); an absolute header value carrying no authority (an
// opaque "rtsp:stream" or an empty-authority "rtsp:///path") is malformed as a
// base and is ignored in favour of the request URL; a relative header value is
// joined onto the request URL. The userinfo (credentials) from requestURL is
// re-attached to the
// result, since the base URL later carries aggregate PLAY/TEARDOWN/keepalive
// requests that must still authenticate. It returns ErrInvalidURL when
// requestURL does not parse, and never panics.
func ResolveBaseURL(requestURL, contentBase, contentLocation string) (string, error) {
	reqURL, err := url.Parse(requestURL)
	if err != nil {
		return "", ErrInvalidURL
	}

	source := contentBase
	if source == "" {
		source = contentLocation
	}
	if source == "" {
		source = requestURL
	}

	var resolvedStr string
	switch {
	case strings.HasPrefix(source, "/"):
		// Force the value into the path component by string concatenation
		// rather than url.Parse, so a "//host/path" shape from a misbehaving
		// server cannot be reinterpreted as a network-relative host.
		resolvedStr = reqURL.Scheme + "://" + reqURL.Host + source
	default:
		parsed, perr := url.Parse(source)
		if perr != nil {
			return "", ErrInvalidURL
		}
		switch {
		case parsed.IsAbs() && parsed.Host != "":
			// An absolute Content-Base/Content-Location with an authority
			// contributes only its path and query; scheme and host come from
			// the already-connected request URL, mirroring resolveAbsoluteControl.
			// IsAbs tests only that a scheme is present, so the header authority
			// (and even its scheme) is untrusted: firmware bakes wrong LAN or
			// placeholder addresses into it, and every later aggregate
			// PLAY/TEARDOWN/keepalive travels the established socket regardless,
			// so a header authority must never redirect the request-URI.
			resolvedStr = reqURL.Scheme + "://" + reqURL.Host + parsed.EscapedPath()
			if parsed.RawQuery != "" {
				resolvedStr = resolvedStr + "?" + parsed.RawQuery
			}
		case parsed.IsAbs():
			// Absolute but authority-less: an opaque form ("rtsp:stream", whose
			// segment url.Parse stores in Opaque, not Path) or an empty authority
			// ("rtsp:///path"). It carries no host to distrust and is malformed
			// as an RTSP base, so fall back to the request URL rather than adopt
			// a hostless base or silently drop the opaque segment through
			// EscapedPath.
			resolvedStr = requestURL
		default:
			resolvedStr = reqURL.ResolveReference(parsed).String()
		}
	}

	resolved, err := url.Parse(resolvedStr)
	if err != nil {
		return "", ErrInvalidURL
	}
	resolved.User = reqURL.User
	return resolved.String(), nil
}

// ResolveControlURL resolves one track's a=control value against base
// (ResolveBaseURL's result, or a non-"*" session-level control already
// resolved against it). Rules (RFC 2326 plus observed firmware tolerances):
//   - control == "" or control == "*": base is used unchanged.
//   - absolute control (rtsp:// or rtsps://): only its path and query are
//     kept; scheme, host, and userinfo come from base, because firmware
//     bakes wrong LAN or placeholder addresses into the SDP.
//   - relative control: appended to base's string form. Exactly one "/" is
//     inserted, except no separator is added when control begins with "/"
//     or "?", or when base already ends with "/". A "?"-leading control
//     attaches directly after base (extending its query).
//
// Appending is textual and deliberate, not RFC 3986 reference resolution: a
// control value like "trackID=1" is not a valid relative reference, and
// resolving it as one would drop the base's last path segment entirely.
//
// One consequence is worth knowing. When base already carries a query, the
// appended segment lands INSIDE that query: "rtsp://h/s?token=a" plus
// "trackID=1" gives "rtsp://h/s?token=a/trackID=1", which re-parses with
// RawQuery "token=a/trackID=1" rather than as a new path segment. This is
// deliberate and validated, not a quirk to fix: it mirrors how MediaMTX forms
// its own aggregate base (it advertises Content-Base "rtsp://h/s?token=a/",
// with the separator inside the query) and how ffmpeg/libav resolves the same
// input, and a server that authenticates from the query still recovers the
// token, because it strips the trailing control segment before reading the
// query. Confirmed end to end against MediaMTX with token-in-query
// authentication, and against a camera that carries no query; the behaviour is
// kept and pinned by test.
//
// The userinfo from base is preserved. It returns ErrInvalidURL when base
// does not parse, and never panics.
func ResolveControlURL(base, control string) (string, error) {
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", ErrInvalidURL
	}

	if control == "" || control == "*" {
		return base, nil
	}

	var resolved string
	// The absolute branch is taken only when the control actually carries an
	// authority. An opaque "rtsp:trackID=1" (its segment lands in Opaque, not
	// Path, so EscapedPath is empty and the payload would be dropped) and an
	// empty-authority "rtsp:///x" both have no host, so they fall back to
	// relative resolution instead of a path-stripped URL that addresses no
	// track. This mirrors ResolveBaseURL's parsed.Host != "" guard for the same
	// input class (issue #31); real cameras do not emit an opaque control, so
	// this only keeps the two resolvers from diverging on it.
	if controlURL, cerr := url.Parse(control); cerr == nil && isRTSPScheme(controlURL.Scheme) && controlURL.Host != "" {
		resolved = resolveAbsoluteControl(baseURL, controlURL)
	} else {
		resolved = resolveRelativeControl(base, control)
	}

	// Neither branch has vetted its output. The a=control attribute is
	// remote input, so a value carrying CR or LF would otherwise be handed
	// back as a "resolved" URL and could split a later request line. The
	// absolute branch can also assemble a degenerate string such as "://"
	// when base carries no scheme or host. Re-parsing catches both, exactly
	// as ResolveBaseURL already does for its own result, so every string
	// this package returns as a URL is one that parses.
	if _, err := url.Parse(resolved); err != nil {
		return "", ErrInvalidURL
	}
	return resolved, nil
}

// isRTSPScheme reports whether scheme is "rtsp" or "rtsps", case-insensitively.
func isRTSPScheme(scheme string) bool {
	return strings.EqualFold(scheme, "rtsp") || strings.EqualFold(scheme, "rtsps")
}

// resolveAbsoluteControl builds baseScheme://[baseUser@]baseHost + the
// control URL's path and query, discarding the control URL's own scheme,
// host, and userinfo per the firmware tolerance documented on
// ResolveControlURL.
func resolveAbsoluteControl(baseURL, controlURL *url.URL) string {
	var b strings.Builder
	b.WriteString(baseURL.Scheme)
	b.WriteString("://")
	if baseURL.User != nil {
		b.WriteString(baseURL.User.String())
		b.WriteByte('@')
	}
	b.WriteString(baseURL.Host)
	b.WriteString(controlURL.EscapedPath())
	if controlURL.RawQuery != "" {
		b.WriteByte('?')
		b.WriteString(controlURL.RawQuery)
	}
	return b.String()
}

// resolveRelativeControl appends control to base's string form with exactly
// one "/" separator, per the rule documented on ResolveControlURL. This is
// deliberate string concatenation, not url.ResolveReference: a control value
// like "trackID=1" is not a valid relative reference and must be appended
// verbatim.
func resolveRelativeControl(base, control string) string {
	sep := "/"
	if strings.HasSuffix(base, "/") || strings.HasPrefix(control, "/") || strings.HasPrefix(control, "?") {
		sep = ""
	}
	return base + sep + control
}

// RedactURL returns urlStr with any userinfo replaced by the single token
// "REDACTED" and the password dropped entirely, for safe logging. A URL
// that parses is rebuilt through net/url; one that does not is masked with
// a best-effort scan of a "scheme://userinfo@host" prefix. It never panics
// and never returns a string containing the original password.
func RedactURL(urlStr string) string {
	u, err := url.Parse(urlStr)
	if err != nil {
		return redactFallback(urlStr)
	}
	if u.User != nil {
		u.User = url.User("REDACTED")
		return u.String()
	}
	// A URL with no "//" authority ("rtsp:user:pass@cam/stream") is still
	// valid: net/url parses the whole remainder into Opaque and never
	// populates User, so the branch above cannot fire and the credentials
	// would survive into the log. Mask that shape explicitly.
	if red, ok := redactOpaque(u.Opaque); ok {
		u.Opaque = red
		return u.String()
	}
	return urlStr
}

// isURIScheme reports whether s is a syntactically valid URI scheme per RFC
// 3986: an ALPHA followed by ALPHA / DIGIT / "+" / "-" / ".". It gates the
// redaction fallback so ordinary prose containing a colon is not mistaken
// for a credential-bearing URL.
func isURIScheme(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z':
		case i > 0 && (ch >= '0' && ch <= '9' || ch == '+' || ch == '-' || ch == '.'):
		default:
			return false
		}
	}
	return true
}

// redactOpaque masks the userinfo of an opaque URI remainder shaped like
// "userinfo@host/path", returning the masked remainder and true when that
// shape is present. The authority ends at the first "/", and the userinfo
// at the last "@" within it, so a password containing "@" cannot leave part
// of itself behind.
func redactOpaque(opaque string) (string, bool) {
	authorityEnd := strings.IndexByte(opaque, '/')
	if authorityEnd < 0 {
		authorityEnd = len(opaque)
	}
	at := strings.LastIndex(opaque[:authorityEnd], "@")
	if at < 0 {
		return "", false
	}
	return "REDACTED" + opaque[at:], true
}

// redactFallback masks the userinfo of a string that failed strict URL
// parsing by scanning for a "scheme://userinfo@host" shape, or the
// authority-less "scheme:userinfo@host" shape, and replacing the userinfo
// with "REDACTED". A string without either shape is returned unchanged.
func redactFallback(urlStr string) string {
	var prefixLen int
	switch i := strings.IndexByte(urlStr, ':'); {
	case i < 0 || !isURIScheme(urlStr[:i]):
		// No scheme at all, so this is not a URL and must be left alone
		// rather than have arbitrary "word:word@word" text rewritten.
		return urlStr
	case strings.HasPrefix(urlStr[i:], "://"):
		prefixLen = i + len("://")
	default:
		prefixLen = i + len(":")
	}
	red, ok := redactOpaque(urlStr[prefixLen:])
	if !ok {
		return urlStr
	}
	return urlStr[:prefixLen] + red
}
