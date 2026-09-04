package doctor

import (
	"crypto/x509"
	"errors"
	"net"
	"net/url"
	"strconv"
	"syscall"

	"github.com/tphakala/go-audio-stream/rtsp"
)

// failureClass is a classified handshake or open failure: a concise top-line
// result phrase, a plain-language reason for the failed step's detail, and an
// optional actionable hint. Every field is classifier-authored and host-free,
// so none of it can leak PII; the target host, resolved IP, and port are
// deliberately never named (the report is meant to be pasteable publicly).
type failureClass struct {
	result string // top-line "result:" phrase, e.g. "connection refused"
	reason string // failed-step detail, e.g. "nothing is listening on that port"
	hint   string // optional "how to fix" line, "" when there is nothing useful
}

// Result phrases are the controlled vocabulary for a classified failure's
// top-line result field. They are named constants so the classifier and its
// tests share one source of truth (and so repeated phrases do not trip the
// duplicate-string linter). resultAuthFailed reuses the existing
// authFailedPhrase, which mapExit and failStep already agree on.
const (
	resultAuthFailed        = authFailedPhrase
	resultConnRefused       = "connection refused"
	resultHostUnreachable   = "host unreachable"
	resultTransportRejected = "transport rejected"
)

// classifyFailure maps a handshake (RTSP) or open (HTTP) error to a
// failureClass. ok is false when the error is not one this taxonomy recognizes,
// so the caller keeps its generic fallback (the scrubbed raw error). step is
// the stage name (stepDial, stepDescribe, ...) so a generic timeout or a
// dropped connection can name where it happened. opts supplies the flags a hint
// may reference and lets the 401 path tell "no credentials were given" apart
// from "the credentials were rejected".
//
//nolint:gocyclo // flat taxonomy of independent failure cases; splitting by layer would scatter the specific-before-generic ordering.
func classifyFailure(step string, err error, opts Options) (failureClass, bool) {
	if err == nil {
		return failureClass{}, false
	}

	// Authentication first: a 401 is the single most common camera failure and
	// must not be shadowed by the generic non-success-status case below.
	if errors.Is(err, rtsp.ErrUnsupportedAuth) {
		return failureClass{
			result: resultAuthFailed,
			reason: "the server's authentication scheme is not supported",
		}, true
	}
	var unauthorized *rtsp.UnauthorizedError
	if errors.Is(err, rtsp.ErrAuthFailed) || errors.As(err, &unauthorized) {
		if credentialsProvided(opts) {
			return failureClass{
				result: resultAuthFailed,
				reason: "the server rejected the username or password",
				hint:   "check -user and -password (or the credentials in the URL)",
			}, true
		}
		return failureClass{
			result: "authentication required",
			reason: "the stream requires a login but no credentials were given",
			hint:   "pass -user and -password, or put them in the URL",
		}, true
	}

	// TLS trust problems (rtsps/https). x509 errors arrive wrapped in the dial
	// error, so match by type.
	var unknownAuthority x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var certInvalid x509.CertificateInvalidError
	switch {
	case errors.As(err, &unknownAuthority):
		return failureClass{
			result: "TLS certificate not trusted",
			reason: "the server's TLS certificate is signed by an unknown authority",
			hint:   "use -insecure-tls to skip certificate checks (testing only)",
		}, true
	case errors.As(err, &hostnameErr):
		return failureClass{
			result: "TLS certificate mismatch",
			reason: "the server's TLS certificate is not valid for this host",
			hint:   "use -insecure-tls to skip certificate checks (testing only)",
		}, true
	case errors.As(err, &certInvalid):
		return failureClass{
			result: "TLS certificate invalid",
			reason: "the server's TLS certificate is expired or not yet valid",
			hint:   "check the camera's clock, or use -insecure-tls (testing only)",
		}, true
	}

	// DNS resolution.
	if dnsErr, ok := errors.AsType[*net.DNSError](err); ok {
		switch {
		case dnsErr.IsNotFound:
			return failureClass{
				result: "host not found",
				reason: "the hostname could not be resolved (no such host)",
				hint:   "check the hostname, or your DNS settings",
			}, true
		default:
			return failureClass{
				result: "DNS lookup failed",
				reason: "the hostname could not be resolved (lookup failed or timed out)",
				hint:   "check your DNS settings and connectivity",
			}, true
		}
	}

	// Specific connect-time syscall errors, ahead of the generic timeout below.
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return failureClass{
			result: resultConnRefused,
			reason: "nothing is listening on that port",
			hint:   "check the port, or enable RTSP on the camera (some ship with it off)",
		}, true
	case errors.Is(err, syscall.EHOSTUNREACH):
		return failureClass{
			result: resultHostUnreachable,
			reason: "no route to the host",
			hint:   "check the address and that it is on a reachable network",
		}, true
	case errors.Is(err, syscall.ENETUNREACH):
		return failureClass{
			result: "network unreachable",
			reason: "the host's network is unreachable from here",
			hint:   "check your routing and that you are on the right subnet",
		}, true
	case errors.Is(err, syscall.ECONNRESET):
		return failureClass{
			result: "connection reset",
			reason: "the host reset the connection",
		}, true
	}

	// A timeout: either a net timeout or the dial/request context deadline.
	var netErr net.Error
	if errors.Is(err, rtsp.ErrRequestTimeout) ||
		errors.Is(err, syscall.ETIMEDOUT) ||
		(errors.As(err, &netErr) && netErr.Timeout()) {
		if step == stepDial {
			return failureClass{
				result: "connection timed out",
				reason: "timed out connecting: the host is down, unreachable, or firewalled",
				hint:   "increase -timeout, or check the host and firewall",
			}, true
		}
		return failureClass{
			result: "timed out",
			reason: "the server did not respond in time",
			hint:   "increase -timeout, or check the host and firewall",
		}, true
	}

	// RTSP transport rejection (a 461 or the UDP-specific sentinel).
	if errors.Is(err, rtsp.ErrUDPSetupRejected) {
		return failureClass{
			result: resultTransportRejected,
			reason: "the server rejected the requested media transport",
			hint:   "try -transport tcp (or -transport udp)",
		}, true
	}

	// A non-success RTSP status the client did not special-case.
	if respErr, ok := errors.AsType[*rtsp.ResponseError](err); ok {
		return classifyResponseStatus(respErr), true
	}

	// SDP the client could not use.
	if errors.Is(err, rtsp.ErrNotSDP) {
		return failureClass{
			result: "bad stream description",
			reason: "the server's DESCRIBE response was not SDP",
		}, true
	}

	// The control connection dropped mid-handshake.
	if errors.Is(err, rtsp.ErrConnectionClosed) {
		return failureClass{
			result: "connection dropped",
			reason: "the connection closed unexpectedly during " + stepWord(step),
			hint:   "check network stability and that the server accepts this stream",
		}, true
	}

	return failureClass{}, false
}

// classifyResponseStatus turns a non-success RTSP status into a failureClass,
// naming the common codes and falling back to the numeric code for the rest.
func classifyResponseStatus(re *rtsp.ResponseError) failureClass {
	switch re.Code {
	case rtsp.StatusNotFound:
		return failureClass{
			result: "stream not found",
			reason: "the stream path was not found (404)",
			hint:   "check the path in the URL",
		}
	case rtsp.StatusForbidden:
		return failureClass{
			result: "access forbidden",
			reason: "the server refused access to the stream (403)",
		}
	case rtsp.StatusUnsupportedTransport:
		return failureClass{
			result: resultTransportRejected,
			reason: "the server rejected the media transport (461)",
			hint:   "try -transport tcp (or -transport udp)",
		}
	default:
		return failureClass{
			result: "server error",
			reason: "the server returned status " + strconv.Itoa(re.Code),
		}
	}
}

// credentialsProvided reports whether the run supplied any credentials, via the
// -user/-password flags or the URL userinfo. It lets the 401 path distinguish
// "you gave a wrong password" from "this stream needs a login you did not
// provide". A URL that does not parse is treated as carrying no userinfo; the
// flags still count.
func credentialsProvided(opts Options) bool {
	if opts.Username != "" || opts.Password != "" {
		return true
	}
	if u, err := url.Parse(opts.URL); err == nil && u.User != nil {
		// A bare "user" (no password) still counts as an attempt to log in.
		return u.User.Username() != ""
	}
	return false
}

// stepWord maps a step constant to a lowercase word for a sentence.
func stepWord(step string) string {
	switch step {
	case stepDial:
		return "connect"
	case stepDescribe:
		return "describe"
	case stepSetup:
		return "setup"
	case stepPlay:
		return "play"
	case stepOpen:
		return "open"
	default:
		return "the handshake"
	}
}
