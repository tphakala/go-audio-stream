package httpsource

import (
	"context"
	"fmt"
	"net/http"

	"github.com/tphakala/go-audio-stream/internal/httpauth"
)

// doFunc issues an open-phase request and returns the response with a transport
// failure already classified (see classifyOpenErr). authorize and its helpers
// take it so every challenge-response retry shares the first request's open
// timeout and caller-context cancellation rather than running unbounded.
type doFunc func(*http.Request) (*http.Response, error)

// authorize answers the server's WWW-Authenticate challenge on resp401 and
// returns the response to proceed with. It is called only when the first
// request came back 401 and credentials are present.
//
// It selects the strongest usable challenge (SHA-256 Digest > MD5 Digest >
// Basic). A Digest challenge is answered here over plaintext and TLS alike,
// because Digest never puts the password on the wire, so it needs no
// AllowInsecureAuth opt-in. A Basic challenge is not answered here: buildRequest
// already sends preemptive Basic whenever it is permitted (TLS, or
// AllowInsecureAuth over plaintext), so a Basic challenge means either those
// credentials were rejected (surface the 401 unchanged) or the caller declined
// to send Basic over plaintext, in which case answering now would leak the
// password and is refused with ErrInsecureAuth. A challenge this source cannot
// answer leaves resp401 to surface as a *StatusError.
func (c *Client) authorize(ctx context.Context, do doFunc, resp401 *http.Response, cfg *Config, tgt *target) (*http.Response, error) {
	sel, ok := selectChallenge(resp401)
	if !ok {
		return resp401, nil
	}
	switch sel.Scheme {
	case httpauth.AuthDigest:
		return c.answerDigest(ctx, do, resp401, sel, cfg, tgt)
	case httpauth.AuthBasic:
		if !tgt.tls && !cfg.AllowInsecureAuth {
			// The first request went out bare, so answering Basic now is the
			// only way the password would reach the server, and it would be in
			// the clear. Refuse, matching the secure-by-default policy.
			_ = resp401.Body.Close()
			return nil, ErrInsecureAuth
		}
		// Preemptive Basic was already sent and rejected; retrying it is
		// pointless. Surface the 401.
		return resp401, nil
	default:
		return resp401, nil
	}
}

// answerDigest computes a Digest Authorization for challenge and retries the
// GET, then allows exactly one more retry when a second 401 carries stale=true
// (a rotated server nonce, not a credential failure), mirroring the rtsp
// client. A retry that still returns 401 is left to surface as a *StatusError.
func (c *Client) answerDigest(ctx context.Context, do doFunc, resp401 *http.Response, challenge httpauth.Challenge, cfg *Config, tgt *target) (*http.Response, error) {
	resp, err := c.digestRetry(ctx, do, resp401, challenge, cfg, tgt)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	next, ok := selectChallenge(resp)
	if !ok || next.Scheme != httpauth.AuthDigest || !next.Stale() {
		return resp, nil
	}
	return c.digestRetry(ctx, do, resp, next, cfg, tgt)
}

// digestRetry builds a GET carrying a fresh Digest Authorization for challenge
// and issues it. The nonce count is always 1: this source makes exactly one
// authorized request under a given server nonce, and a stale rotation issues a
// new nonce that resets the count. The digest-uri is the request-target
// (origin-form path and query), per RFC 7616.
func (c *Client) digestRetry(ctx context.Context, do doFunc, prev *http.Response, challenge httpauth.Challenge, cfg *Config, tgt *target) (*http.Response, error) {
	// The superseded 401 is finished with, and DisableKeepAlives means the
	// retry dials a fresh connection regardless, so close it on every path.
	defer func() { _ = prev.Body.Close() }()
	cnonce, err := httpauth.NewCNonce()
	if err != nil {
		return nil, fmt.Errorf("%w: preparing the authenticated request: %w", ErrConnectionClosed, err)
	}
	req, err := newRequest(ctx, cfg, tgt)
	if err != nil {
		return nil, err
	}
	value, err := httpauth.Authorize(challenge, httpauth.Credentials{Username: tgt.username, Password: tgt.password}, httpauth.DigestInput{
		Method:     http.MethodGet,
		URI:        req.URL.RequestURI(),
		CNonce:     cnonce,
		NonceCount: 1,
	})
	if err != nil {
		// Unreachable: SelectChallenge only returns answerable challenges. If
		// that invariant ever broke, fail the open rather than loop.
		return nil, fmt.Errorf("%w: preparing the authenticated request: %w", ErrConnectionClosed, err)
	}
	req.Header.Set("Authorization", value)
	return do(req)
}

// selectChallenge parses resp's WWW-Authenticate header and returns the
// strongest usable challenge (SHA-256 Digest > MD5 Digest > Basic), or false
// when none is answerable.
func selectChallenge(resp *http.Response) (httpauth.Challenge, bool) {
	return httpauth.SelectChallenge(httpauth.ParseChallenges(resp.Header.Values("WWW-Authenticate")))
}
