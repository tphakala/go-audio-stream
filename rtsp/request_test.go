package rtsp_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/go-audio-stream/internal/testserver"
	"github.com/tphakala/go-audio-stream/rtsp"
)

// Shared credentials and challenge fields for the auth scenarios.
const (
	authUser  = "admin"
	authPass  = "s3cr3t"
	authRealm = "IP Camera"
	authNonce = "0cc175b9c0f1b6a831c399e269772661"

	// Auth scheme names as SessionInfo reports them and AuthSpec expects them.
	schemeDigest = "Digest"
	schemeBasic  = "Basic"
)

// digestSpec builds a Digest AuthSpec with the given algorithm and qop. An
// empty qop selects the legacy RFC 2069 form.
func digestSpec(algorithm, qop string) *testserver.AuthSpec {
	return &testserver.AuthSpec{
		Scheme:    schemeDigest,
		Realm:     authRealm,
		Nonce:     authNonce,
		Algorithm: algorithm,
		QOP:       qop,
		Username:  authUser,
		Password:  authPass,
	}
}

// runAuthedPlayback dials with the spec's credentials, then drives
// Describe/Setup/Play against a server whose Handshake demands authentication
// via spec (the server recomputes and validates the Digest response). It
// returns the client left in the playing state.
func runAuthedPlayback(t *testing.T, spec *testserver.AuthSpec) *rtsp.Client {
	t.Helper()
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		if _, err := sc.Handshake(testserver.HandshakeConfig{
			SDP:            opusSDP,
			SessionID:      testSessionID,
			SessionTimeout: testTimeoutS,
			Auth:           spec,
		}); err != nil {
			return
		}
		drainRequests(sc)
	}})
	c, err := rtsp.Dial(context.Background(), rtsp.Config{
		URL:      s.URL("/stream"),
		Timeout:  testTimeout,
		Username: spec.Username,
		Password: spec.Password,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	describeSetupPlay(t, c, nil)
	return c
}

func TestDigestAuthRetrySHA256(t *testing.T) {
	c := runAuthedPlayback(t, digestSpec("SHA-256", "auth"))
	defer closeAndWait(t, c)
	if got := c.SessionInfo().AuthScheme; got != rtsp.AuthDigest {
		t.Errorf("AuthScheme = %q, want Digest", got)
	}
}

func TestDigestAuthRetryMD5(t *testing.T) {
	c := runAuthedPlayback(t, digestSpec("MD5", "auth"))
	defer closeAndWait(t, c)
	if got := c.SessionInfo().AuthScheme; got != rtsp.AuthDigest {
		t.Errorf("AuthScheme = %q, want Digest", got)
	}
}

func TestLegacyRFC2069Auth(t *testing.T) {
	// A no-qop Digest challenge (legacy RFC 2069, Hikvision style): the client
	// answers in legacy form (no qop/nc/cnonce).
	c := runAuthedPlayback(t, digestSpec("", ""))
	defer closeAndWait(t, c)
	if got := c.SessionInfo().AuthScheme; got != rtsp.AuthDigest {
		t.Errorf("AuthScheme = %q, want Digest", got)
	}
}

func TestBasicAuth(t *testing.T) {
	c := runAuthedPlayback(t, &testserver.AuthSpec{
		Scheme:   schemeBasic,
		Realm:    authRealm,
		Username: authUser,
		Password: authPass,
	})
	defer closeAndWait(t, c)
	if got := c.SessionInfo().AuthScheme; got != rtsp.AuthBasic {
		t.Errorf("AuthScheme = %q, want Basic", got)
	}
}

func TestStaleNonceReauth(t *testing.T) {
	spec := digestSpec("MD5", "auth")
	spec.Stale = true
	// A DIFFERENT nonce, so the retry is only accepted if the client actually
	// adopted the rotation. Reusing the original would make a client that
	// ignored it byte-identical to one that honoured it.
	spec.StaleNonce = "9f2c1e4b7a8d0356c1f9e2b4a7d0356c"
	// The server issues one stale=true 401 after the first authenticated
	// DESCRIBE; the client re-auths once more with the refreshed nonce and the
	// request succeeds, so no failure surfaces.
	c := runAuthedPlayback(t, spec)
	defer closeAndWait(t, c)
	if got := c.SessionInfo().AuthScheme; got != rtsp.AuthDigest {
		t.Errorf("AuthScheme = %q, want Digest", got)
	}
}

// digestChallenge builds a WWW-Authenticate header carrying a Digest
// challenge, optionally marked stale.
func digestChallenge(stale bool) rtsp.Header {
	v := `Digest realm="` + authRealm + `", nonce="` + authNonce + `", algorithm=MD5, qop="auth"`
	if stale {
		v += ", stale=true"
	}
	h := rtsp.Header{}
	h.Set("WWW-Authenticate", v)
	return h
}

func TestAuthFailureExhausted(t *testing.T) {
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		req, err := sc.ReadRequest() // Dial OPTIONS.
		if err != nil {
			return
		}
		_ = sc.Respond(req, 200, "OK", nil, nil)
		// DESCRIBE with no credentials: challenge.
		req, err = sc.ReadRequest()
		if err != nil {
			return
		}
		_ = sc.Respond(req, 401, "Unauthorized", digestChallenge(false), nil)
		// The authenticated retry is rejected again, non-stale: the client must
		// give up rather than loop.
		req, err = sc.ReadRequest()
		if err != nil {
			return
		}
		_ = sc.Respond(req, 401, "Unauthorized", digestChallenge(false), nil)
		drainRequests(sc)
	}})
	c, err := rtsp.Dial(context.Background(), rtsp.Config{
		URL:      s.URL("/stream"),
		Timeout:  testTimeout,
		Username: authUser,
		Password: authPass,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer closeAndWait(t, c)

	_, err = c.Describe(context.Background())
	if !errors.Is(err, rtsp.ErrAuthFailed) {
		t.Fatalf("Describe = %v, want ErrAuthFailed", err)
	}
	// The auth path itself does not tear down the session: the caller decides.
	// The deferred closeAndWait asserting ErrClosed is the proof; had the auth
	// path funneled a shutdown, that first cause would win over Close's
	// ErrClosed and the assertion would fail.
}

// authReq records one authenticated request the preauth server observed.
type authReq struct {
	method string
	nc     string
	err    error
}

// checkAuthorized validates that req is the expected method carrying a Digest
// Authorization header, extracting its nc for the incrementing-count assertion.
func checkAuthorized(req *rtsp.Request, wantMethod string) authReq {
	if req.Method != wantMethod {
		return authReq{method: req.Method, err: fmt.Errorf("got %s, want %s", req.Method, wantMethod)}
	}
	authz := req.Header.Get("Authorization")
	if !strings.HasPrefix(authz, "Digest ") {
		return authReq{method: wantMethod, err: fmt.Errorf("missing Digest Authorization: %q", authz)}
	}
	return authReq{method: wantMethod, nc: digestParam(authz, "nc")}
}

// digestParam extracts one auth-param value from a Digest header value,
// stripping any surrounding quotes.
func digestParam(header, name string) string {
	for _, part := range strings.Split(strings.TrimPrefix(header, "Digest "), ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && strings.EqualFold(strings.TrimSpace(k), name) {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return ""
}

func TestAuthPreauthSubsequentRequests(t *testing.T) {
	results := make(chan authReq, 4)
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		req, err := sc.ReadRequest() // Dial OPTIONS.
		if err != nil {
			return
		}
		_ = sc.Respond(req, 200, "OK", nil, nil)
		// DESCRIBE with no credentials: challenge once.
		req, err = sc.ReadRequest()
		if err != nil {
			return
		}
		_ = sc.Respond(req, 401, "Unauthorized", digestChallenge(false), nil)
		// The authenticated DESCRIBE, SETUP, and PLAY must all preauthenticate
		// with an incrementing nc and never be re-challenged.
		describe, err := sc.ReadRequest()
		if err != nil {
			return
		}
		results <- checkAuthorized(describe, methodDescribe)
		_ = sc.Respond(describe, 200, "OK", sdpHeaders(""), []byte(opusSDP))
		setup, err := sc.ReadRequest()
		if err != nil {
			return
		}
		results <- checkAuthorized(setup, methodSetup)
		_ = sc.Respond(setup, 200, "OK", setupHeaders(0, 1, testSessionID, testTimeoutS), nil)
		play, err := sc.ReadRequest()
		if err != nil {
			return
		}
		results <- checkAuthorized(play, methodPlay)
		h := rtsp.Header{}
		h.Set("Session", sessionValue(testSessionID, testTimeoutS))
		_ = sc.Respond(play, 200, "OK", h, nil)
		drainRequests(sc)
	}})
	c, err := rtsp.Dial(context.Background(), rtsp.Config{
		URL:      s.URL("/stream"),
		Timeout:  testTimeout,
		Username: authUser,
		Password: authPass,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer closeAndWait(t, c)

	describeSetupPlay(t, c, nil)

	wantNC := []string{"00000001", "00000002", "00000003"}
	for i := range wantNC {
		select {
		case got := <-results:
			if got.err != nil {
				t.Errorf("%s: %v", got.method, got.err)
			}
			if got.nc != wantNC[i] {
				t.Errorf("%s nc = %q, want %q (incrementing per request)", got.method, got.nc, wantNC[i])
			}
		case <-time.After(testTimeout):
			t.Fatal("timed out awaiting an authorized request")
		}
	}
	if got := c.SessionInfo().AuthScheme; got != rtsp.AuthDigest {
		t.Errorf("AuthScheme = %q, want Digest", got)
	}
}

// Once authentication is active, requests the timer and teardown paths send
// outside roundTrip must carry credentials too. A camera that challenges every
// request answers 401 to an unauthenticated keepalive, and the fire-and-forget
// reply is dropped as an unknown CSeq, so the session would expire at the
// server's own timeout with no error anywhere.
func TestKeepaliveAndTeardownCarryAuthorization(t *testing.T) {
	spec := digestSpec("MD5", "auth")
	observed := make(chan authReq, 8)
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		if _, err := sc.Handshake(testserver.HandshakeConfig{
			SDP:            opusSDP,
			SessionID:      testSessionID,
			SessionTimeout: 2, // keepalive interval = sessionTimeout/2 = 1s
			Auth:           spec,
		}); err != nil {
			return
		}
		for {
			req, err := sc.ReadRequest()
			if err != nil {
				return
			}
			observed <- checkAuthorized(req, req.Method)
			_ = sc.Respond(req, 200, "OK", nil, nil)
		}
	}})
	c, err := rtsp.Dial(context.Background(), rtsp.Config{
		URL:      s.URL("/stream"),
		Timeout:  testTimeout,
		Username: spec.Username,
		Password: spec.Password,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	describeSetupPlay(t, c, nil)

	// The first post-PLAY request is a keepalive.
	select {
	case got := <-observed:
		if got.err != nil {
			t.Errorf("keepalive: %v", got.err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("no keepalive within 4s")
	}

	closeAndWait(t, c)

	// The best-effort TEARDOWN follows on the reader's terminal path.
	for {
		select {
		case got := <-observed:
			if got.method != methodTeardown {
				continue // another keepalive fired first.
			}
			if got.err != nil {
				t.Errorf("teardown: %v", got.err)
			}
			return
		case <-time.After(4 * time.Second):
			t.Fatal("no authenticated TEARDOWN observed")
		}
	}
}

// A challenge that changes scheme mid-session is refused rather than honoured,
// even when it claims stale=true. "stale" is an RFC 7616 Digest parameter, but
// nothing stops a server attaching it to a Basic challenge, and honouring that
// would silently downgrade an established Digest session into sending the
// password base64-encoded over a plaintext control channel.
func TestStaleChallengeCannotDowngradeTheScheme(t *testing.T) {
	sawBasic := make(chan struct{}, 1)
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		req, err := sc.ReadRequest() // Dial OPTIONS.
		if err != nil {
			return
		}
		_ = sc.Respond(req, 200, "OK", nil, nil)

		// Challenge Digest, so the client commits to Digest.
		req, err = sc.ReadRequest()
		if err != nil {
			return
		}
		_ = sc.Respond(req, 401, "Unauthorized", digestChallenge(false), nil)

		// Then answer the authenticated retry with a stale=true BASIC challenge.
		req, err = sc.ReadRequest()
		if err != nil {
			return
		}
		h := rtsp.Header{}
		h.Set("WWW-Authenticate", `Basic realm="`+authRealm+`", stale=true`)
		_ = sc.Respond(req, 401, "Unauthorized", h, nil)

		// Anything further must not carry a Basic credential.
		for {
			req, err = sc.ReadRequest()
			if err != nil {
				return
			}
			if strings.HasPrefix(req.Header.Get("Authorization"), "Basic ") {
				select {
				case sawBasic <- struct{}{}:
				default:
				}
			}
			_ = sc.Respond(req, 200, "OK", nil, nil)
		}
	}})
	c, err := rtsp.Dial(context.Background(), rtsp.Config{
		URL:      s.URL("/stream"),
		Timeout:  testTimeout,
		Username: authUser,
		Password: authPass,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer closeAndWait(t, c)

	_, err = c.Describe(context.Background())
	if !errors.Is(err, rtsp.ErrAuthFailed) {
		t.Fatalf("Describe = %v, want ErrAuthFailed rather than a scheme downgrade", err)
	}
	// The scheme the client committed to must still be the one it reports.
	if got := c.SessionInfo().AuthScheme; got != rtsp.AuthDigest {
		t.Errorf("AuthScheme = %q, want Digest", got)
	}
	select {
	case <-sawBasic:
		t.Error("client sent a Basic credential after committing to Digest")
	default:
	}
}
