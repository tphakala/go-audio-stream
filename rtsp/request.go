package rtsp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// roundTrip sends req (assigning a fresh CSeq and adding User-Agent and, when
// set, Session), registers a pending response channel, and waits for the
// matching response, bounded by Config.Timeout merged with ctx. On a write
// error, timeout, or ctx cancellation it funnels into shutdown and returns the
// cause. It does not interpret the status; callers apply ClassifyStatus.
//
// It preauthenticates the request once authentication is active, but it does
// not answer a challenge itself: that is do's job. Dial's OPTIONS probe calls
// this directly, so an OPTIONS answered 401 is tolerated rather than retried.
func (c *Client) roundTrip(ctx context.Context, req *Request) (*Response, error) {
	cseq := int(c.cseq.Add(1))
	req.CSeq = cseq
	if req.Header == nil {
		req.Header = Header{}
	}
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	c.mu.Lock()
	sess := c.sessionID
	c.mu.Unlock()
	if sess != "" {
		req.Header.Set("Session", sess)
	}
	ch := make(chan *Response, 1)
	c.pendMu.Lock()
	c.pending[cseq] = ch
	c.pendMu.Unlock()
	defer func() {
		c.pendMu.Lock()
		delete(c.pending, cseq)
		c.pendMu.Unlock()
	}()

	// Allocate the nonce count, compute the Authorization, marshal, and write,
	// all under one writeMu (writeAuthorizedRequest), so the nonce-count order
	// and the wire order stay the same order (RFC 7616 section 3.4.3, issue
	// #17). Every non-nil error funnels shutdown here, but for different
	// reasons: a write or deadline error also reaches the reader through the
	// broken socket, whereas a marshal failure (for example a request URI over
	// MaxRequestURILen) never touched the wire, so this is the one funnel point
	// nothing else would reach. The deferred cleanup above removes the pending
	// entry regardless, so callers (Dial's cleanup, Wait) unblock instead of
	// parking on a bare receive forever. A marshal failure returns
	// ErrInvalidRequest unwrapped; a write or deadline error wraps
	// ErrConnectionClosed.
	if werr := c.writeAuthorizedRequest(req); werr != nil {
		cause := werr
		if !errors.Is(werr, ErrInvalidRequest) {
			cause = fmt.Errorf("%w: %w", ErrConnectionClosed, werr)
		}
		c.initiateShutdown(cause)
		return nil, cause
	}

	timer := time.NewTimer(c.cfg.Timeout)
	defer timer.Stop()
	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		if resp, ok := drain(ch); ok {
			return resp, nil
		}
		c.initiateShutdown(ctx.Err())
		return nil, ctx.Err()
	case <-timer.C:
		if resp, ok := drain(ch); ok {
			return resp, nil
		}
		terr := fmt.Errorf("%w after %v", ErrRequestTimeout, c.cfg.Timeout)
		c.initiateShutdown(terr)
		return nil, terr
	case <-c.done:
		if resp, ok := drain(ch); ok {
			return resp, nil
		}
		if te := c.termError(); te != nil {
			return nil, te
		}
		return nil, ErrConnectionClosed
	}
}

// writeAuthorizedRequest allocates the nonce count and computes the
// Authorization header (attachAuthorization), marshals req, and writes it, all
// under one writeMu critical section, so the nonce-count order and the wire
// order are the same order (RFC 7616 section 3.4.3, issue #17).
// attachAuthorization increments nc under mu; folding it and the write into a
// single writeMu hold stops a second sender from allocating a higher nc and
// reaching the socket first.
//
// It returns MarshalRequest's error unwrapped, so a caller can match
// ErrInvalidRequest, and the deadline or socket write error otherwise.
// attachAuthorization takes mu while this holds writeMu; mu is only ever taken
// while writeMu is held, never the reverse, so the writeMu->mu order is uniform
// and cannot deadlock.
func (c *Client) writeAuthorizedRequest(req *Request) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.attachAuthorization(req)
	raw, err := MarshalRequest(req)
	if err != nil {
		return err
	}
	if err := c.armWriteDeadline(c.cfg.Timeout); err != nil {
		return err
	}
	_, err = c.conn.Write(raw)
	return err
}

// marshalBareRequest builds and marshals a request that deliberately bypasses
// roundTrip: a fresh CSeq, the standard headers, and credentials when auth is
// active, with no pending entry registered and no reply awaited. Its two callers
// are the keepalive timer and the best-effort TEARDOWN.
//
// It exists so that the header set stays in one place. Each of those callers
// used to assemble it inline, and the copies drifted: the TEARDOWN went out
// unauthenticated for as long as it had its own copy, which on a camera that
// challenges every request leaves the session allocated until the server's own
// timeout. Note it applies no lifecycle-state branch of its own, so routing a
// caller through it cannot change what that caller sends.
//
// Callers must hold writeMu across this call and the socket write that follows,
// so the nonce count attachAuthorization allocates reaches the wire in
// allocation order relative to a concurrent sender's (issue #17).
func (c *Client) marshalBareRequest(method, url string) ([]byte, error) {
	c.mu.Lock()
	sess := c.sessionID
	c.mu.Unlock()

	req := &Request{Method: method, URL: url, CSeq: int(c.cseq.Add(1)), Header: Header{}}
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	if sess != "" {
		req.Header.Set("Session", sess)
	}
	c.attachAuthorization(req)
	return MarshalRequest(req)
}

// drain reports whether a response was already delivered to ch, without
// blocking. Every terminal branch of roundTrip's select must consult it first.
//
// select picks uniformly at random among ready cases, and dispatchResponse
// hands the response to a buffered channel without yielding, so the reader can
// close done (or the deadline can elapse) before the waiter is scheduled. Both
// cases are then ready and the waiter takes the terminal branch half the time,
// discarding a fully parsed, valid response. Reproduced at roughly 1 in 10000
// dials against a server that answers OPTIONS and ends the session in the same
// segment, which is what any server closing after a response produces.
func drain(ch <-chan *Response) (*Response, bool) {
	select {
	case resp := <-ch:
		return resp, true
	default:
		return nil, false
	}
}

// authState is the client's active authentication state, guarded by Client.mu.
// It is populated when a 401 is first answered and consulted by roundTrip to
// preauthenticate every subsequent request. The credentials and the header
// value are never logged.
type authState struct {
	active    bool
	challenge Challenge
	creds     Credentials
	cnonce    string
	nc        uint32
}

// do sends req via roundTrip and, on a 401, authenticates using the selected
// challenge and the session credentials, then returns the final response. It
// applies ClassifyStatus to translate the status into nil (success), a
// *audiostream.RedirectError, or a *ResponseError; a 401 this client cannot
// answer, or one that survives the permitted retries, becomes an error matching
// ErrAuthFailed and wrapping the last *UnauthorizedError. A transport failure
// (already funneled into shutdown by roundTrip) is returned with a nil
// response.
func (c *Client) do(ctx context.Context, req *Request) (*Response, error) {
	resp, err := c.roundTrip(ctx, req)
	if err != nil {
		return nil, err
	}
	cerr := ClassifyStatus(resp)
	var ue *UnauthorizedError
	if !errors.As(cerr, &ue) {
		return resp, cerr
	}
	return c.authenticate(ctx, req, ue)
}

// authenticate answers a 401 and retries req. It selects the strongest usable
// challenge, activates the client's auth state (so roundTrip preauthenticates
// this and every later request), and resends once. A second 401 earns exactly
// one more retry when it carries stale=true (a rotated nonce, not a failure);
// otherwise it returns ErrAuthFailed. The session is never torn down here: the
// caller decides.
func (c *Client) authenticate(ctx context.Context, req *Request, ue *UnauthorizedError) (*Response, error) {
	challenge, ok := SelectChallenge(ParseChallenges(ue.Challenges))
	if !ok {
		return nil, authFailed(ue, "no usable challenge offered")
	}
	if !c.activateAuth(challenge) {
		return nil, authFailed(ue, "challenge scheme cannot be answered")
	}

	resp, err := c.roundTrip(ctx, req)
	if err != nil {
		return nil, err
	}
	cerr := ClassifyStatus(resp)
	var second *UnauthorizedError
	if !errors.As(cerr, &second) {
		return resp, cerr // success, or a redirect/response error to surface.
	}

	next, ok := SelectChallenge(ParseChallenges(second.Challenges))
	if !ok || !next.Stale() {
		return nil, authFailed(second, "credentials rejected")
	}
	if !c.refreshNonce(next) {
		return nil, authFailed(second, "challenge changed scheme mid-session")
	}

	resp, err = c.roundTrip(ctx, req)
	if err != nil {
		return nil, err
	}
	cerr = ClassifyStatus(resp)
	var third *UnauthorizedError
	if errors.As(cerr, &third) {
		return nil, authFailed(third, "credentials rejected after a nonce rotation")
	}
	return resp, cerr
}

// authFailed builds the error every give-up path returns: it matches
// ErrAuthFailed under errors.Is, says which way the exchange ended, and wraps
// the challenge so a caller can still read the realm and prompt for
// credentials. The bare sentinel told a caller only that authentication had
// failed, and some of these paths return before anything was ever sent, so
// "rejected" would have been wrong for them.
func authFailed(ue *UnauthorizedError, why string) error {
	return fmt.Errorf("%w: %s: %w", ErrAuthFailed, why, ue)
}

// attachAuthorization adds an Authorization header to req when authentication
// is active, computing it for req's method and URI and incrementing the nonce
// count under the same server nonce (RFC 7616). The value is never logged.
//
// A compute error removes the header rather than leaving req as it found it.
// authenticate retries the SAME request, so a bare return would resend the
// PREVIOUS attempt's Authorization under a nonce count the server has already
// seen; deleting it sends the request unauthenticated instead, which is what
// re-enters the auth path.
//
// The nonce count is allocated here under mu. The senders that can run at the
// same time (roundTrip via writeAuthorizedRequest, the keepalive timer, and the
// best-effort TEARDOWN) each perform this call and the socket write inside a
// single writeMu hold, so the nc order and the wire order are the same order
// (issue #17): an RFC 7616 section 3.4.3 server tracking the highest nc seen
// under a nonce cannot observe a later count reach the socket before an earlier
// one. This takes mu while the caller holds writeMu; mu is only ever taken
// while writeMu is held, never the reverse, so the ordering cannot deadlock.
func (c *Client) attachAuthorization(req *Request) {
	c.mu.Lock()
	if !c.auth.active {
		c.mu.Unlock()
		return
	}
	c.auth.nc++
	st := c.auth
	c.mu.Unlock()

	value, err := Authorize(st.challenge, st.creds, DigestInput{
		Method:     req.Method,
		URI:        req.URL,
		CNonce:     st.cnonce,
		NonceCount: st.nc,
	})
	if err != nil {
		req.Header.Del("Authorization")
		logWarn(c.cfg.Logger, "could not compute an Authorization header; sending the request unauthenticated",
			"method", req.Method, "error", err)
		return
	}
	req.Header.Set("Authorization", value)
}

// activateAuth records the selected challenge and the session credentials as
// the active auth state, generates a fresh client nonce, and resets the nonce
// count so the next attach computes nc=1. It also records the scheme
// SessionInfo reports.
//
// It returns false for a scheme this client cannot answer. SelectChallenge
// already rejects those, so the guard is unreachable through authenticate; it
// sits here rather than there because this is the point where the scheme is
// committed to the client, and a future selector that returned a new scheme
// would otherwise activate an auth state Authorize cannot compute, turning
// every later request into a silently unauthenticated one.
func (c *Client) activateAuth(challenge Challenge) bool {
	if challenge.Scheme != AuthBasic && challenge.Scheme != AuthDigest {
		return false
	}
	cnonce, err := newCNonce()
	if err != nil {
		return false
	}
	c.mu.Lock()
	c.auth = authState{
		active:    true,
		challenge: challenge,
		creds:     Credentials{Username: c.username, Password: c.password},
		cnonce:    cnonce,
	}
	c.mu.Unlock()
	return true
}

// refreshNonce updates the active auth state with a rotated (stale) challenge,
// resetting the nonce count so the retry computes nc=1 under the new nonce. The
// credentials and client nonce are unchanged. It reports false, changing
// nothing, when the challenge names a different scheme.
//
// A stale nonce is by definition a rotation within one scheme, so a scheme
// change here is not a rotation at all. Accepting one would be a downgrade
// channel: "stale" is an RFC 7616 Digest parameter, but nothing stops a server
// attaching it to a Basic challenge, and this function is the one writer of the
// active challenge that does not go through activateAuth's scheme check. A
// Digest session would then start sending the password base64-encoded, and
// SessionInfo would still report Digest.
func (c *Client) refreshNonce(challenge Challenge) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if challenge.Scheme != c.auth.challenge.Scheme {
		return false
	}
	c.auth.challenge = challenge
	c.auth.nc = 0
	return true
}

// newCNonce returns a fresh client nonce: 16 random bytes hex-encoded.
func newCNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
