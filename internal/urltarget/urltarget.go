// Package urltarget resolves a URL plus fallback credentials into the request
// destination the network sources (httpsource, hlssource, rtsp) need. It holds
// one copy of the security-relevant pieces shared across schemes: leak-safe URL
// parsing (ParseURL), credential resolution with CR/LF/NUL rejection
// (ResolveCredentials), and userinfo/fragment stripping (StripUserinfoFragment),
// so a fix to any of them lands in every source at once rather than in one copy
// only. ParseHTTP composes them for the http/https sources; the rtsp source,
// which validates a different scheme set and supplies default ports, calls the
// helpers directly.
package urltarget

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Target is the resolved request destination parsed from a raw URL. Each caller
// picks the fields it needs: httpsource uses TLS and Hostname (the TLS server
// name), hlssource uses Host (host:port, for same-origin credential gating) and
// reads https per request from the URL scheme.
type Target struct {
	// TLS reports whether the scheme is https.
	TLS bool
	// RequestURL is the request URL with userinfo and fragment stripped, so
	// neither reaches the server or a source's Info.
	RequestURL string
	// Hostname is the host without its port (the TLS server name).
	Hostname string
	// Host is host:port as written in the URL (same-origin credential gating).
	Host string
	// Username and Password are the resolved Basic credentials.
	Username string
	Password string
}

// ParseURL trims raw, rejects an empty value, and parses it. Its error never
// contains the URL text: url.Parse returns a *url.Error whose Error() prints the
// whole input verbatim, userinfo included, so a URL carrying credentials would
// leak them through a wrapped error into caller logs; only the underlying cause
// is surfaced, never the URL text or a decoded credential. (A malformed
// percent-escape in the userinfo can surface the offending escape token, for
// example "%zz", via url.EscapeError; parsing fails before decode, so no usable
// secret escapes.) It does NOT validate the scheme, host, or port; callers apply
// their own scheme set and port policy.
func ParseURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("empty URL")
	}
	u, err := url.Parse(raw)
	if err != nil {
		if uerr, ok := errors.AsType[*url.Error](err); ok {
			return nil, fmt.Errorf("malformed URL: %w", uerr.Err)
		}
		return nil, errors.New("malformed URL")
	}
	return u, nil
}

// ResolveCredentials overrides cfgUser and cfgPass with u's userinfo (treating a
// wholly empty userinfo as absent), and rejects CR, LF and NUL in the result.
func ResolveCredentials(u *url.URL, cfgUser, cfgPass string) (user, pass string, err error) {
	user, pass = cfgUser, cfgPass
	// A wholly empty userinfo ("http://@host" or "http://:@host", what a URL
	// template produces when its substitution variables are unset) is treated as
	// absent rather than as an override, so it cannot silently discard the
	// credentials the caller supplied. url.User is non-nil for both of those, so
	// neither a nil check nor an empty-string check on User.String() catches
	// them. Gating on the username alone would be too broad:
	// "http://:secret@host" carries a real password-only credential.
	if u.User != nil {
		urlUser := u.User.Username()
		urlPass, _ := u.User.Password()
		if urlUser != "" || urlPass != "" {
			user, pass = urlUser, urlPass
		}
	}
	// Userinfo is percent-decoded, so url.Parse's rejection of raw control
	// characters does not cover "%0D%0A". These values flow into an Authorization
	// header; rejecting them at the boundary that extracts them from an untrusted
	// URL is where it belongs.
	if strings.ContainsAny(user, "\r\n\x00") || strings.ContainsAny(pass, "\r\n\x00") {
		return "", "", errors.New("CR, LF or NUL in credentials")
	}
	return user, pass, nil
}

// StripUserinfoFragment returns u's string form with the userinfo and fragment
// removed, so neither the credentials nor a client-side fragment reaches the
// server or a source's Info. It copies u and does not mutate it.
func StripUserinfoFragment(u *url.URL) string {
	reqURL := *u
	reqURL.User = nil
	// A fragment is a client-side construct with no meaning on the wire; leaving
	// it on would send it to the server verbatim and expose it through Info.
	reqURL.Fragment = ""
	reqURL.RawFragment = ""
	return reqURL.String()
}

// ParseHTTP resolves rawURL together with the caller's fallback credentials
// (cfgUser, cfgPass, typically from Config) into a Target for an http/https
// source. It validates the scheme (http or https) and the port range, resolves
// credentials, and strips the userinfo and fragment from the request URL. On
// malformed input it returns a bare cause error describing the fault; callers
// wrap it in their own ErrInvalidURL sentinel. It is http/https only; the rtsp
// source composes ParseURL, ResolveCredentials, and StripUserinfoFragment
// itself for its own scheme set and default-port policy.
func ParseHTTP(rawURL, cfgUser, cfgPass string) (Target, error) {
	u, err := ParseURL(rawURL)
	if err != nil {
		return Target{}, err
	}

	var tlsOn bool
	switch strings.ToLower(u.Scheme) {
	case "http":
		tlsOn = false
	case "https":
		tlsOn = true
	default:
		return Target{}, fmt.Errorf("unsupported scheme %q", u.Scheme)
	}

	host := u.Hostname()
	if host == "" {
		return Target{}, errors.New("missing host")
	}
	if port := u.Port(); port != "" {
		// url.Parse only guarantees the port is digits, so an out-of-range value
		// would otherwise reach the dialer and surface as a connection error
		// rather than the documented ErrInvalidURL.
		n, perr := strconv.Atoi(port)
		if perr != nil || n < 1 || n > 65535 {
			return Target{}, fmt.Errorf("port %q out of range", port)
		}
	}

	username, password, err := ResolveCredentials(u, cfgUser, cfgPass)
	if err != nil {
		return Target{}, err
	}

	return Target{
		TLS:        tlsOn,
		RequestURL: StripUserinfoFragment(u),
		Hostname:   host,
		Host:       u.Host,
		Username:   username,
		Password:   password,
	}, nil
}
