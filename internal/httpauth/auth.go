// Package httpauth implements the RFC 7616 (HTTP Digest, with RFC 2069
// legacy) and RFC 7617 (HTTP Basic) challenge parsing and Authorization
// building shared by the rtsp and httpsource packages. Both are peer audio
// sources that must answer a WWW-Authenticate challenge, and neither should
// import the other, so the scheme-agnostic machinery lives here instead of in
// one of them. The functions are pure and total: they never panic, and the
// Digest computation takes its per-request cnonce and nonce count as inputs so
// callers own the session state (a random cnonce, an incrementing nc).
//
// It is an internal package: only code within this module can import it, so
// the surface stays private and the rtsp package can re-export the pieces that
// are part of its public API without this becoming public itself.
package httpauth

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"strings"
)

// AuthScheme identifies an authentication scheme.
type AuthScheme int

const (
	// AuthNone means no authentication has been negotiated. It is the zero
	// value, so a Challenge or SessionInfo that has not been filled in reports
	// "no auth" rather than "a scheme I do not implement".
	AuthNone AuthScheme = iota
	// AuthUnknown is a scheme this client does not implement.
	AuthUnknown
	// AuthBasic is HTTP Basic (RFC 7617).
	AuthBasic
	// AuthDigest is HTTP Digest (RFC 7616, with RFC 2069 legacy support).
	AuthDigest
)

// String returns the scheme name as it appears in a WWW-Authenticate header
// ("Basic", "Digest"), "none" when no scheme has been negotiated, or "unknown"
// for a scheme this client does not implement.
func (s AuthScheme) String() string {
	switch s {
	case AuthNone:
		return "none"
	case AuthBasic:
		return "Basic"
	case AuthDigest:
		return "Digest"
	default:
		// AuthUnknown and any out-of-range value.
		return unknownAuthName
	}
}

// unknownAuthName is the name reported for an unimplemented or out-of-range
// scheme, shared by String and its test.
const unknownAuthName = "unknown"

// Auth-param names, lowercased as ParseChallenges stores them in
// Challenge.Params and as digestAuthorization reads them.
const (
	paramRealm     = "realm"
	paramNonce     = "nonce"
	paramQOP       = "qop"
	paramAlgorithm = "algorithm"
	paramOpaque    = "opaque"
	paramStale     = "stale"
)

// Digest algorithm tokens (RFC 7616) and the single qop-value this client
// answers with.
const (
	algMD5    = "MD5"
	algSHA256 = "SHA-256"
	qopAuth   = "auth"
)

// Challenge is one parsed WWW-Authenticate challenge. Params holds every
// auth-param keyed by lowercased name (realm, nonce, opaque, qop,
// algorithm, domain, stale, ...) with quotes removed. Realm is a copy of
// Params["realm"] for convenience.
type Challenge struct {
	// Scheme is the challenge scheme.
	Scheme AuthScheme
	// Realm is the realm parameter, "" if absent.
	Realm string
	// Params holds all auth-params, lowercased keys, unquoted values.
	Params map[string]string
}

// Stale reports whether the challenge carries stale=true
// (case-insensitive), RFC 7616's signal that the nonce expired but the
// credentials were otherwise valid. The client uses it to permit one extra
// re-auth without counting a failure.
func (c Challenge) Stale() bool {
	return strings.EqualFold(c.Params[paramStale], "true")
}

// ParseChallenges parses WWW-Authenticate header field values into
// challenges. Each element of values is one header line's value; a single
// value may itself carry several comma-separated challenges. Challenges
// that do not parse (or use an unimplemented scheme) are skipped rather
// than failing the whole set. Encounter order is preserved. It never panics.
func ParseChallenges(values []string) []Challenge {
	var challenges []Challenge
	for _, value := range values {
		parseChallengeValue(value, &challenges)
	}
	return challenges
}

// parseChallengeValue tokenizes one header line value and appends the
// challenges it carries to challenges. current tracks the challenge that
// trailing auth-param segments attach to; an unknown scheme clears it so its
// params are dropped. current is re-taken after every append, so it is never
// stale when a param segment mutates it.
func parseChallengeValue(value string, challenges *[]Challenge) {
	var current *Challenge
	for _, segment := range splitChallengeSegments(value) {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}
		head, rest := splitToken(segment)
		switch {
		case strings.EqualFold(head, "Basic"):
			*challenges = append(*challenges, Challenge{Scheme: AuthBasic, Params: map[string]string{}})
			current = &(*challenges)[len(*challenges)-1]
			if rest != "" {
				parseAuthParam(current, rest)
			}
		case strings.EqualFold(head, "Digest"):
			*challenges = append(*challenges, Challenge{Scheme: AuthDigest, Params: map[string]string{}})
			current = &(*challenges)[len(*challenges)-1]
			if rest != "" {
				parseAuthParam(current, rest)
			}
		case !strings.Contains(head, "="):
			// The segment opens with a bare token. RFC 7235 allows
			// whitespace around an auth-param's "=", so "nonce = \"n\"" also
			// lands here; it is a param exactly when the remainder starts
			// with "=". Otherwise the token names an unimplemented scheme
			// (Negotiate, NTLM) or is garbage, and current is cleared so any
			// following params are dropped with it. Testing the remainder
			// rather than the whole segment matters because a scheme's
			// opaque token often carries base64 "=" padding, which would
			// otherwise misfile the token as a param of the previous,
			// unrelated challenge.
			if strings.HasPrefix(rest, "=") {
				parseAuthParam(current, segment)
			} else {
				current = nil
			}
		default:
			// name=value auth-param for the current challenge.
			parseAuthParam(current, segment)
		}
	}
}

// splitChallengeSegments splits a header value on commas that are not inside
// a double-quoted string, honoring backslash escapes within quotes. Quotes
// and escapes are left intact in the returned segments; unquoting happens in
// the auth-param value parse. It never panics on unterminated quotes.
func splitChallengeSegments(value string) []string {
	var segments []string
	var b strings.Builder
	inQuotes := false
	escaped := false
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if escaped {
			b.WriteByte(ch)
			escaped = false
			continue
		}
		switch {
		case ch == '\\' && inQuotes:
			b.WriteByte(ch)
			escaped = true
		case ch == '"':
			inQuotes = !inQuotes
			b.WriteByte(ch)
		case ch == ',' && !inQuotes:
			segments = append(segments, b.String())
			b.Reset()
		default:
			b.WriteByte(ch)
		}
	}
	segments = append(segments, b.String())
	return segments
}

// splitToken splits s into its leading token (up to the first run of spaces
// or tabs) and the remainder with that whitespace trimmed off.
func splitToken(s string) (head, rest string) {
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimLeft(s[i:], " \t")
}

// parseAuthParam parses a single name=value auth-param segment into current.
// It splits on the first '=', requiring a non-empty name; the value is
// unquoted when double-quoted. A nil current (segment following an unknown
// scheme) drops the param.
func parseAuthParam(current *Challenge, segment string) {
	if current == nil {
		return
	}
	before, after, ok := strings.Cut(segment, "=")
	if !ok {
		return
	}
	name := strings.ToLower(strings.TrimSpace(before))
	if name == "" {
		return
	}
	value := parseAuthParamValue(after)
	if current.Params == nil {
		current.Params = map[string]string{}
	}
	current.Params[name] = value
	if name == paramRealm {
		current.Realm = value
	}
}

// parseAuthParamValue trims and, when the value is double-quoted, returns the
// unescaped contents up to the closing quote. An unquoted value is returned
// as-is. An unterminated quote yields what was accumulated (total, no panic).
func parseAuthParamValue(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw[0] != '"' {
		return raw
	}
	var b strings.Builder
	escaped := false
	for i := 1; i < len(raw); i++ {
		ch := raw[i]
		if escaped {
			b.WriteByte(ch)
			escaped = false
			continue
		}
		switch ch {
		case '\\':
			escaped = true
		case '"':
			return b.String()
		default:
			b.WriteByte(ch)
		}
	}
	return b.String()
}

// SelectChallenge picks the strongest usable challenge: a SHA-256 Digest
// first, then any other Digest, then Basic (matching the research report's
// SHA-256 Digest > MD5 Digest > Basic ordering). It returns the challenge
// and true, or a zero Challenge and false when none are usable.
//
// "Usable" means Authorize can actually answer it, so a Digest challenge
// this client cannot compute a response for (no nonce, an unsupported
// algorithm such as a -sess variant, or a qop that does not offer "auth")
// is skipped rather than selected. Without that filter a server offering an
// exotic Digest alongside Basic would strand the client on the Digest and
// fail the session, even though the Basic challenge was answerable.
func SelectChallenge(challenges []Challenge) (Challenge, bool) {
	var digest, basic *Challenge
	for i := range challenges {
		c := &challenges[i]
		switch c.Scheme {
		case AuthDigest:
			if !digestAnswerable(c) {
				continue
			}
			if strings.EqualFold(c.Params[paramAlgorithm], algSHA256) {
				return *c, true
			}
			if digest == nil {
				digest = c
			}
		case AuthBasic:
			if basic == nil {
				basic = c
			}
		default:
			// AuthNone and AuthUnknown: not usable, skip.
		}
	}
	if digest != nil {
		return *digest, true
	}
	if basic != nil {
		return *basic, true
	}
	return Challenge{}, false
}

// digestAnswerable reports whether digestAuthorization can build a response
// for this Digest challenge. It mirrors that function's preconditions
// exactly, so SelectChallenge never hands back a challenge Authorize would
// reject.
func digestAnswerable(c *Challenge) bool {
	if c.Params[paramNonce] == "" {
		return false
	}
	if _, err := digestHash(c.Params[paramAlgorithm]); err != nil {
		return false
	}
	qop := strings.TrimSpace(c.Params[paramQOP])
	return qop == "" || qopOffersAuth(qop)
}

// qopOffersAuth reports whether a qop parameter value offers the "auth"
// qop-value this client answers with. The value is a comma-separated list,
// for example `auth,auth-int`.
func qopOffersAuth(qop string) bool {
	for v := range strings.SplitSeq(qop, ",") {
		if strings.EqualFold(strings.TrimSpace(v), qopAuth) {
			return true
		}
	}
	return false
}

// Sentinel errors from the Authorization builders.
var (
	// ErrUnsupportedAuth means the scheme is not implemented or the Digest
	// algorithm is neither MD5, SHA-256, nor absent (for example a -sess
	// variant).
	ErrUnsupportedAuth = errors.New("httpauth: unsupported authentication scheme or algorithm")
	// ErrMissingNonce means a Digest challenge carried no nonce.
	ErrMissingNonce = errors.New("httpauth: digest challenge missing nonce")
	// ErrMissingCNonce means a qop-bearing Digest challenge was answered with
	// an empty client nonce.
	ErrMissingCNonce = errors.New("httpauth: digest qop requires a client nonce")
)

// Credentials are the username and password used to answer a challenge.
type Credentials struct {
	// Username is the account name.
	Username string
	// Password is the secret. It is never logged by this package.
	Password string
}

// DigestInput carries the per-request inputs to a Digest computation that
// vary by request. Keeping cnonce and nc as inputs makes the function pure
// and deterministic; the caller generates a random cnonce (crypto/rand) and
// tracks the nonce count.
type DigestInput struct {
	// Method is the request method, for example "DESCRIBE" or "GET".
	Method string
	// URI is the request-URI exactly as it appears on the request line.
	URI string
	// CNonce is the client nonce. Required when the challenge carries qop;
	// ignored for a no-qop (RFC 2069) challenge.
	CNonce string
	// NonceCount is the nc value. The caller increments it per request made
	// under a given server nonce. Used only when the challenge carries qop.
	NonceCount uint32
}

// Authorize builds the Authorization header value answering challenge for
// creds and the request described by in. For AuthBasic it returns a Basic
// credential (in is ignored). For AuthDigest it computes the RFC 7616
// response using the challenge's algorithm and qop:
//   - qop=auth present: the full form with cnonce and nc from in.
//   - qop absent (legacy RFC 2069): H(H(A1):nonce:H(A2)).
//
// It returns ErrUnsupportedAuth for an unknown scheme, an unsupported
// algorithm (anything other than MD5, SHA-256, or absent), or a qop that
// does not offer "auth"; ErrMissingNonce when a Digest challenge has no
// nonce; and ErrMissingCNonce when qop is present but in.CNonce is empty.
// SelectChallenge screens for exactly these conditions, so a challenge it
// returns is always answerable. It never panics.
func Authorize(challenge Challenge, creds Credentials, in DigestInput) (string, error) {
	switch challenge.Scheme {
	case AuthBasic:
		return basicAuthorization(creds), nil
	case AuthDigest:
		return digestAuthorization(challenge, creds, in)
	default:
		return "", ErrUnsupportedAuth
	}
}

// basicAuthorization returns the Basic credential value
// "Basic base64(username:password)" (RFC 7617). It is unexported: tests
// compile into package httpauth and reach it directly, and callers use
// Authorize, so it need not be part of the public API surface.
func basicAuthorization(creds Credentials) string {
	token := base64.StdEncoding.EncodeToString([]byte(creds.Username + ":" + creds.Password))
	return "Basic " + token
}

// digestAuthorization computes an RFC 7616 (or RFC 2069 legacy) Digest
// Authorization header value from an already-parsed Digest challenge. It is
// the AuthDigest branch of Authorize. Same errors as Authorize's Digest
// branch. Unexported for the same reason as basicAuthorization; the RFC 7616
// test vectors assert against it directly from the in-package test file.
func digestAuthorization(challenge Challenge, creds Credentials, in DigestInput) (string, error) {
	nonce := challenge.Params[paramNonce]
	if nonce == "" {
		return "", ErrMissingNonce
	}
	algorithm := challenge.Params[paramAlgorithm]
	newHash, err := digestHash(algorithm)
	if err != nil {
		return "", err
	}
	hashHex := func(s string) string {
		h := newHash()
		_, _ = h.Write([]byte(s))
		return hex.EncodeToString(h.Sum(nil))
	}
	ha1 := hashHex(creds.Username + ":" + challenge.Realm + ":" + creds.Password)
	ha2 := hashHex(in.Method + ":" + in.URI)

	// Common fields, in the fixed order the tests string-compare against.
	parts := []string{
		kvQuoted("username", creds.Username),
		kvQuoted(paramRealm, challenge.Realm),
		kvQuoted(paramNonce, nonce),
		kvQuoted("uri", in.URI),
	}

	// An explicitly empty qop ("qop=\"\"") is malformed rather than a real
	// offer, so it is treated as absent and answered with the RFC 2069
	// legacy form. That is strictly more likely to authenticate than
	// demanding a cnonce for a qop the server never actually named.
	if qop := strings.TrimSpace(challenge.Params[paramQOP]); qop != "" {
		// This client only implements qop=auth. Answering "auth" to a
		// challenge that offers only auth-int would produce a response the
		// server is bound to reject, so refuse it here instead.
		if !qopOffersAuth(qop) {
			return "", ErrUnsupportedAuth
		}
		if in.CNonce == "" {
			return "", ErrMissingCNonce
		}
		nc := fmt.Sprintf("%08x", in.NonceCount)
		response := hashHex(strings.Join([]string{ha1, nonce, nc, in.CNonce, qopAuth, ha2}, ":"))
		echoAlg := algorithm
		if echoAlg == "" {
			echoAlg = algMD5
		}
		parts = append(parts,
			kvUnquoted(paramAlgorithm, echoAlg),
			kvUnquoted(paramQOP, qopAuth),
			kvUnquoted("nc", nc),
			kvQuoted("cnonce", in.CNonce),
			kvQuoted("response", response),
		)
	} else {
		// RFC 2069 legacy: no qop, nc, or cnonce; echo algorithm only when
		// the challenge specified it.
		response := hashHex(strings.Join([]string{ha1, nonce, ha2}, ":"))
		if algorithm != "" {
			parts = append(parts, kvUnquoted(paramAlgorithm, algorithm))
		}
		parts = append(parts, kvQuoted("response", response))
	}
	if opaque, ok := challenge.Params[paramOpaque]; ok {
		parts = append(parts, kvQuoted(paramOpaque, opaque))
	}
	return "Digest " + strings.Join(parts, ", "), nil
}

// digestHash selects the hash constructor for a Digest algorithm token. An
// empty token or MD5 selects md5, SHA-256 selects sha256, and anything else
// (including -sess variants) is unsupported.
func digestHash(algorithm string) (func() hash.Hash, error) {
	switch {
	case algorithm == "" || strings.EqualFold(algorithm, algMD5):
		return md5.New, nil
	case strings.EqualFold(algorithm, algSHA256):
		return sha256.New, nil
	default:
		return nil, ErrUnsupportedAuth
	}
}

// kvQuoted renders a quoted-string auth-param, name="value". The value is
// escaped per the RFC 7616 quoted-string grammar: backslash first, then
// double-quote, so a credential containing either character cannot break
// out of the quotes or forge extra auth-params. CR and LF are dropped
// outright: a malformed WWW-Authenticate can carry a bare CR into a
// challenge param, and escaping does not neutralize it the way it does a
// quote. The request marshalers reject the resulting header anyway, so this
// is defense in depth, but it keeps a header-splitting byte from ever being
// assembled into an Authorization value.
func kvQuoted(name, value string) string {
	value = stripCRLF(value)
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return name + `="` + value + `"`
}

// stripCRLF removes carriage returns and line feeds from s.
func stripCRLF(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return s
	}
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, s)
}

// kvUnquoted renders a bare-token auth-param, name=value.
func kvUnquoted(name, value string) string {
	return name + "=" + value
}

// NewCNonce returns a fresh client nonce for a Digest exchange: 16 random bytes
// hex-encoded. The caller passes it to Authorize via DigestInput.CNonce and
// owns the nonce-count, so the Digest computation itself stays pure. It is the
// one non-pure helper here, shared by both peer sources rather than duplicated.
func NewCNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
