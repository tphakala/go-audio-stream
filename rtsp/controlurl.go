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
// value is used as-is; a relative header value is joined onto the request
// URL. The userinfo (credentials) from requestURL is re-attached to the
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
		if parsed.IsAbs() {
			resolvedStr = source
		} else {
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

	if controlURL, cerr := url.Parse(control); cerr == nil && isRTSPScheme(controlURL.Scheme) {
		return resolveAbsoluteControl(baseURL, controlURL), nil
	}

	return resolveRelativeControl(base, control), nil
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
	if u.User == nil {
		return urlStr
	}
	u.User = url.User("REDACTED")
	return u.String()
}

// redactFallback masks the userinfo of a string that failed strict URL
// parsing by scanning for a "scheme://userinfo@host" shape: the substring
// between the first "://" and the following "@" is replaced with
// "REDACTED". A string without that shape is returned unchanged.
func redactFallback(urlStr string) string {
	schemeEnd := strings.Index(urlStr, "://")
	if schemeEnd < 0 {
		return urlStr
	}
	rest := urlStr[schemeEnd+len("://"):]
	at := strings.Index(rest, "@")
	if at < 0 {
		return urlStr
	}
	return urlStr[:schemeEnd+len("://")] + "REDACTED" + rest[at:]
}
