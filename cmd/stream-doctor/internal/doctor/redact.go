package doctor

import (
	"errors"
	"net/url"
	"regexp"
	"strings"

	"github.com/tphakala/go-audio-stream/rtsp"
)

// redactedToken is the placeholder every scrubbed PII value collapses to.
const redactedToken = "[redacted]"

// ipLiteralPattern matches an IPv4 dotted-quad or a bracketed IPv6 literal. It
// scrubs a resolved address that a dial error may report even when the target
// URL used a hostname: Go embeds the resolved literal, e.g. dial tcp
// [2001:db8::5]:554, on a connect failure, and neither the host tokens (the
// hostname, not the resolved IP) nor an IPv4-only pattern would catch it. It
// over-matches a bare dotted-quad or a bracketed hex token that is not an
// address (rare in an error string), which is acceptable: over-redaction never
// leaks.
var ipLiteralPattern = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b|\[[0-9a-fA-F:]+\]`)

// redactTarget returns rawURL reduced to its scheme and path, with all PII
// removed: userinfo (credentials), host (hostname or IP), port, and query are
// dropped, so the target stays identifiable (scheme and stream path) without
// revealing where it is, how to reach it, or how to authenticate. A URL that
// does not parse, or carries no host, collapses to the bare token so a raw
// string that may embed credentials never leaks. The report and walkthrough
// are meant to be pasted publicly, so none of that PII may appear.
func redactTarget(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return redactedToken
	}
	return u.Scheme + "://" + redactedToken + u.EscapedPath()
}

// piiScrubber removes PII from an arbitrary string, such as an RTSP error
// bound for the walkthrough, that would otherwise expose the target host or a
// resolved IP.
type piiScrubber struct {
	replacer *strings.Replacer
}

// newPIIScrubber builds a scrubber for the host and IP tokens derived from
// rawURL. Credentials are handled separately (see scrubError): the only error
// path that echoes them is a malformed URL, which is reported by category
// rather than scrubbed, because the standard library also echoes the offending
// credential fragment, which no token list can catch.
func newPIIScrubber(rawURL string) piiScrubber {
	var pairs []string
	if u, err := url.Parse(rawURL); err == nil {
		// u.Host is "host:port" (or "host"); u.Hostname() is the bare host.
		// Replace the longer form first so "host:port" is not left as
		// "[redacted]:port".
		if u.Host != "" {
			pairs = append(pairs, u.Host, redactedToken)
		}
		if h := u.Hostname(); h != "" && h != u.Host {
			pairs = append(pairs, h, redactedToken)
		}
	}
	return piiScrubber{replacer: strings.NewReplacer(pairs...)}
}

// scrubError renders err for display with all PII removed. A malformed-URL
// error from the RTSP stack echoes the raw URL and the offending credential
// fragment, so it is reduced to a category rather than scrubbed token by
// token. Every other error can carry the target host or a resolved IP but not
// credentials (the RTSP client strips userinfo from the request URL and dial
// errors carry only host:port), so the host tokens are replaced and any IP
// literal (IPv4 or bracketed IPv6) is masked as a backstop for a resolved
// address.
func (s piiScrubber) scrubError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, rtsp.ErrInvalidURL) {
		return "invalid URL"
	}
	return s.scrub(err.Error())
}

// scrubString removes PII (the target host and any resolved IP) from an
// arbitrary stream-derived display string and makes it safe for the single-line
// fenced report. It is the non-error sibling of scrubError, used for the RTSP
// Server header, the raw fmtp, and an unknown codec's rtpmap, values a camera
// controls. An empty input stays empty so callers can omit the line.
func (s piiScrubber) scrubString(in string) string {
	if in == "" {
		return ""
	}
	return s.scrub(in)
}

// scrub is the shared core of scrubError and scrubString: it replaces the target
// host and any resolved IP literal with the redaction token, then makes the
// result single-line and fence-safe. Redaction runs on the raw string first, so
// the PII patterns always match before sanitizeLine's character swaps.
func (s piiScrubber) scrub(in string) string {
	out := s.replacer.Replace(in)
	out = ipLiteralPattern.ReplaceAllString(out, redactedToken)
	return sanitizeLine(out)
}

// lineSanitizer collapses the characters that would corrupt the single-line,
// code-fenced report layout.
var lineSanitizer = strings.NewReplacer("\r", " ", "\n", " ", "`", "'")

// sanitizeLine makes s safe to place on one line inside the report's code
// fence: CR and LF (which would inject extra lines or forge list items) become
// spaces, and backticks (which could break out of the surrounding fence)
// become single quotes. Every untrusted stream-derived string that reaches the
// output passes through it, so a hostile camera cannot escape the fence or forge
// report structure.
func sanitizeLine(s string) string {
	return lineSanitizer.Replace(s)
}
