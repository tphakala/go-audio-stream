package rtsp

import (
	"github.com/tphakala/go-audio-stream/internal/httpauth"
)

// The RFC 7616 Digest (with RFC 2069 legacy) and RFC 7617 Basic machinery
// used to live here in full. It now lives in internal/httpauth so the
// httpsource package can answer a WWW-Authenticate challenge with the same
// code without importing rtsp (which would couple the two peer sources and
// pull the whole RTSP client into a binary that only wants the HTTP source).
//
// This file re-exports the pieces that are part of rtsp's public API, so
// callers and rtsp.SessionInfo are unchanged. The types are aliases, so a
// value built as rtsp.Challenge is identical to httpauth.Challenge, and the
// sentinel errors are the same values, so errors.Is keeps matching. The
// functions are thin wrappers rather than value aliases so their signatures
// still render in godoc.

// AuthScheme identifies an authentication scheme.
type AuthScheme = httpauth.AuthScheme

const (
	// AuthNone means no authentication has been negotiated. It is the zero
	// value, so a Challenge or SessionInfo that has not been filled in reports
	// "no auth" rather than "a scheme I do not implement".
	AuthNone = httpauth.AuthNone
	// AuthUnknown is a scheme this client does not implement.
	AuthUnknown = httpauth.AuthUnknown
	// AuthBasic is HTTP Basic (RFC 7617).
	AuthBasic = httpauth.AuthBasic
	// AuthDigest is HTTP Digest (RFC 7616, with RFC 2069 legacy support).
	AuthDigest = httpauth.AuthDigest
)

// Challenge is one parsed WWW-Authenticate challenge. Params holds every
// auth-param keyed by lowercased name (realm, nonce, opaque, qop,
// algorithm, domain, stale, ...) with quotes removed. Realm is a copy of
// Params["realm"] for convenience.
type Challenge = httpauth.Challenge

// Credentials are the username and password used to answer a challenge.
type Credentials = httpauth.Credentials

// DigestInput carries the per-request inputs to a Digest computation that
// vary by request. Keeping cnonce and nc as inputs makes the function pure
// and deterministic; the client generates a random cnonce (crypto/rand) and
// tracks the nonce count.
type DigestInput = httpauth.DigestInput

// Sentinel errors from the Authorization builders. They are the same values
// as their internal/httpauth counterparts, so errors.Is matches either; only
// the .Error() text differs, now carrying the "httpauth:" prefix rather than
// "rtsp:". These surface only through Authorize's error return, which the rtsp
// client logs rather than propagates.
var (
	// ErrUnsupportedAuth means the scheme is not implemented or the Digest
	// algorithm is neither MD5, SHA-256, nor absent (for example a -sess
	// variant).
	ErrUnsupportedAuth = httpauth.ErrUnsupportedAuth
	// ErrMissingNonce means a Digest challenge carried no nonce.
	ErrMissingNonce = httpauth.ErrMissingNonce
	// ErrMissingCNonce means a qop-bearing Digest challenge was answered with
	// an empty client nonce.
	ErrMissingCNonce = httpauth.ErrMissingCNonce
)

// ParseChallenges parses WWW-Authenticate header field values into
// challenges. Each element of values is one header line's value; a single
// value may itself carry several comma-separated challenges. Challenges
// that do not parse (or use an unimplemented scheme) are skipped rather
// than failing the whole set. Encounter order is preserved. It never panics.
func ParseChallenges(values []string) []Challenge {
	return httpauth.ParseChallenges(values)
}

// SelectChallenge picks the strongest usable challenge: a SHA-256 Digest
// first, then any other Digest, then Basic. It returns the challenge and
// true, or a zero Challenge and false when none are usable. "Usable" means
// Authorize can actually answer it, so a Digest challenge this client cannot
// compute a response for is skipped rather than selected.
func SelectChallenge(challenges []Challenge) (Challenge, bool) {
	return httpauth.SelectChallenge(challenges)
}

// Authorize builds the Authorization header value answering challenge for
// creds and the request described by in. For AuthBasic it returns a Basic
// credential (in is ignored). For AuthDigest it computes the RFC 7616
// response using the challenge's algorithm and qop. It returns
// ErrUnsupportedAuth, ErrMissingNonce, or ErrMissingCNonce for a challenge it
// cannot answer, and never panics.
func Authorize(challenge Challenge, creds Credentials, in DigestInput) (string, error) {
	return httpauth.Authorize(challenge, creds, in)
}
