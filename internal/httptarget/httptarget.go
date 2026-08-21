// Package httptarget resolves an http/https URL plus fallback credentials into
// the request destination the HTTP-family sources (httpsource, hlssource) need,
// with one copy of the scheme, port, and credential validation, including the
// security-relevant CR/LF/NUL rejection, so a fix to any of them lands in every
// source at once rather than in one copy only.
package httptarget

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

// Parse resolves rawURL together with the caller's fallback credentials
// (cfgUser, cfgPass, typically from Config) into a Target. It validates the
// scheme (http or https) and the port range, extracts credentials (a non-empty
// URL userinfo overrides the fallback; a wholly empty userinfo is treated as
// absent), and rejects CR, LF and NUL in the resolved credentials. On malformed
// input it returns a bare cause error describing the fault; callers wrap it in
// their own ErrInvalidURL sentinel.
func Parse(rawURL, cfgUser, cfgPass string) (Target, error) {
	raw := strings.TrimSpace(rawURL)
	if raw == "" {
		return Target{}, errors.New("empty URL")
	}
	u, err := url.Parse(raw)
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

	username, password := cfgUser, cfgPass
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
			username, password = urlUser, urlPass
		}
	}
	// Userinfo is percent-decoded, so url.Parse's rejection of raw control
	// characters does not cover "%0D%0A". These values flow into an Authorization
	// header via SetBasicAuth; rejecting them at the boundary that extracts them
	// from an untrusted URL is where it belongs.
	if strings.ContainsAny(username, "\r\n\x00") || strings.ContainsAny(password, "\r\n\x00") {
		return Target{}, errors.New("CR, LF or NUL in credentials")
	}

	reqURL := *u
	reqURL.User = nil
	// A fragment is a client-side construct with no meaning on the wire; leaving
	// it on would send it to the server verbatim and expose it through Info.
	reqURL.Fragment = ""
	reqURL.RawFragment = ""
	return Target{
		TLS:        tlsOn,
		RequestURL: reqURL.String(),
		Hostname:   host,
		Host:       u.Host,
		Username:   username,
		Password:   password,
	}, nil
}
