package httpsource

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Shared Digest fixtures, factored out to satisfy goconst.
const (
	testAlgMD5    = "MD5"
	testAlgSHA256 = "SHA-256"
	testQOPAuth   = "auth"
	testRealmCam  = "cam"
)

// --- digest test server ----------------------------------------------------

// parseAuthz parses a Digest Authorization value ("Digest k=v, k="v", ...")
// into a param map, stripping surrounding quotes. Test-only: the values used
// here (hex tokens, a slash path, a comma-free realm) never contain a comma, so
// a plain split on commas is sufficient.
func parseAuthz(v string) map[string]string {
	m := map[string]string{}
	v = strings.TrimSpace(strings.TrimPrefix(v, "Digest "))
	for part := range strings.SplitSeq(v, ",") {
		name, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		m[strings.TrimSpace(name)] = strings.Trim(strings.TrimSpace(val), `"`)
	}
	return m
}

// digestResponseValid recomputes the RFC 7616 (or RFC 2069 legacy) response the
// way a server would and reports whether the client's matches. realm and
// algorithm are the SERVER's authoritative values, not the client's echoed
// copies, so a client that answered under the wrong realm or hashed with the
// wrong algorithm fails here. It is an independent implementation, so a passing
// test proves the client's Digest is wire-correct, not merely that some
// Authorization header was sent.
func digestResponseValid(authz, method, realm, algorithm, user, pass string) bool {
	if !strings.HasPrefix(authz, "Digest ") {
		return false
	}
	p := parseAuthz(authz)
	newHash := md5.New
	if strings.EqualFold(algorithm, testAlgSHA256) {
		newHash = sha256.New
	}
	hx := func(s string) string {
		h := newHash()
		_, _ = h.Write([]byte(s))
		return hex.EncodeToString(h.Sum(nil))
	}
	ha1 := hx(user + ":" + realm + ":" + pass)
	ha2 := hx(method + ":" + p["uri"])
	var want string
	if p["qop"] != "" {
		want = hx(strings.Join([]string{ha1, p["nonce"], p["nc"], p["cnonce"], p["qop"], ha2}, ":"))
	} else {
		want = hx(strings.Join([]string{ha1, p["nonce"], ha2}, ":"))
	}
	return p["response"] == want
}

// digestChallenge builds a WWW-Authenticate Digest header value.
func digestChallenge(realm, nonce, algorithm, qop string, stale bool) string {
	parts := []string{`realm="` + realm + `"`, `nonce="` + nonce + `"`}
	if qop != "" {
		parts = append(parts, `qop="`+qop+`"`)
	}
	if algorithm != "" {
		parts = append(parts, "algorithm="+algorithm)
	}
	if stale {
		parts = append(parts, "stale=true")
	}
	return "Digest " + strings.Join(parts, ", ")
}

// digestConfig configures a serveDigest handler.
type digestConfig struct {
	realm, algorithm, qop string
	user, pass            string
	body                  []byte
	// alsoOfferBasic adds a Basic challenge alongside the Digest one, so a test
	// can assert the client prefers Digest.
	alsoOfferBasic bool
	// nonce is the server nonce advertised in the initial challenge.
	nonce string
	// staleNonce, when set, makes the server answer the first valid response
	// under nonce with a single stale=true challenge advertising staleNonce, and
	// then accept a valid response under staleNonce, exercising the one-shot
	// stale retry.
	staleNonce string
}

// digestState records how many valid authorized attempts a serveDigest handler
// verified, so a test can assert the round-trip count.
type digestState struct {
	mu       sync.Mutex
	attempts int
}

func (d *digestState) attemptCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.attempts
}

// serveDigest returns a handler that requires Digest auth per cfg and serves
// cfg.body once the client presents a valid response, plus the state it records.
func serveDigest(cfg *digestConfig) (*digestState, http.HandlerFunc) {
	st := &digestState{}
	challenge := func(w http.ResponseWriter, nonce string, stale bool) {
		w.Header().Add("WWW-Authenticate", digestChallenge(cfg.realm, nonce, cfg.algorithm, cfg.qop, stale))
		if cfg.alsoOfferBasic {
			w.Header().Add("WWW-Authenticate", `Basic realm="`+cfg.realm+`"`)
		}
		w.WriteHeader(http.StatusUnauthorized)
	}
	return st, func(w http.ResponseWriter, r *http.Request) {
		authz := r.Header.Get("Authorization")
		p := parseAuthz(authz)
		// A faithful server: verify the response against the SERVER's own
		// realm/nonce/algorithm (not the client's echoed copies), require the
		// echoed realm/algorithm to be the ones issued, and require the
		// digest-uri to be the actual request-target (RFC 7616), which is what
		// req.URL.RequestURI() produces.
		nonceIssued := p["nonce"] == cfg.nonce || (cfg.staleNonce != "" && p["nonce"] == cfg.staleNonce)
		if strings.HasPrefix(authz, "Digest ") &&
			p["realm"] == cfg.realm &&
			nonceIssued &&
			strings.EqualFold(p["algorithm"], cfg.algorithm) &&
			strings.EqualFold(p["qop"], cfg.qop) &&
			p["uri"] == r.URL.RequestURI() &&
			digestResponseValid(authz, r.Method, cfg.realm, cfg.algorithm, cfg.user, cfg.pass) {
			st.mu.Lock()
			st.attempts++
			st.mu.Unlock()
			if cfg.staleNonce != "" && p["nonce"] == cfg.nonce {
				challenge(w, cfg.staleNonce, true)
				return
			}
			w.Header().Set("Content-Type", "audio/wav")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(cfg.body)
			return
		}
		challenge(w, cfg.nonce, false)
	}
}

func digestWAVBody(pcm []byte) []byte {
	return append(stdWAVHeader(wavFormatPCM, 1, 8000, 16, wavUnbounded, wavUnbounded), pcm...)
}

// --- tests -----------------------------------------------------------------

// A Digest challenge is answered over plaintext http without AllowInsecureAuth,
// for both algorithms this client implements. Digest never puts the password on
// the wire, so it needs no insecure opt-in, unlike Basic.
func TestDigestAuthOverPlaintext(t *testing.T) {
	pcm := pcmMono(500)
	body := digestWAVBody(pcm)
	for _, alg := range []string{testAlgMD5, testAlgSHA256} {
		t.Run(alg, func(t *testing.T) {
			st, h := serveDigest(&digestConfig{
				realm: "camera", algorithm: alg, qop: testQOPAuth,
				user: testUser, pass: testPass, body: body,
				nonce: "srv-nonce-" + alg,
			})
			srv := httptest.NewServer(h)
			defer srv.Close()

			var col collector
			c := openOK(t, srv, Config{Username: testUser, Password: testPass, OnFrame: col.onFrame})
			if err := waitResult(t, c, 5*time.Second); !errors.Is(err, ErrStreamEnded) {
				t.Fatalf("Wait = %v, want ErrStreamEnded", err)
			}
			if !bytes.Equal(col.bytes(), pcm) {
				t.Fatal("delivered PCM diverged from source")
			}
			if got := st.attemptCount(); got != 1 {
				t.Fatalf("authorized attempts = %d, want 1", got)
			}
		})
	}
}

// The RFC 2069 legacy no-qop form (Hikvision-style challenge) is answered too.
func TestDigestAuthLegacyNoQOP(t *testing.T) {
	pcm := pcmMono(200)
	st, h := serveDigest(&digestConfig{
		realm: "IP Camera(42)", algorithm: "", qop: "",
		user: testUser, pass: testPass, body: digestWAVBody(pcm),
		nonce: "4e4f4e43452d31323334",
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	var col collector
	c := openOK(t, srv, Config{Username: testUser, Password: testPass, OnFrame: col.onFrame})
	if err := waitResult(t, c, 5*time.Second); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	if !bytes.Equal(col.bytes(), pcm) {
		t.Fatal("delivered PCM diverged from source")
	}
	if got := st.attemptCount(); got != 1 {
		t.Fatalf("authorized attempts = %d, want 1", got)
	}
}

// Offered both Digest and Basic, the client picks Digest, so even over plaintext
// without the opt-in the password never travels in the clear.
func TestDigestPreferredOverBasic(t *testing.T) {
	pcm := pcmMono(200)
	st, h := serveDigest(&digestConfig{
		realm: testRealmCam, algorithm: testAlgMD5, qop: testQOPAuth,
		user: testUser, pass: testPass, body: digestWAVBody(pcm),
		nonce: "n1", alsoOfferBasic: true,
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	var col collector
	// A non-root request target exercises the digest-uri = request-target rule
	// (RFC 7616) against a real path and query, which the server pins.
	c, err := Open(context.Background(), Config{URL: srv.URL + "/live?ch=1", Username: testUser, Password: testPass, OnFrame: col.onFrame})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := waitResult(t, c, 5*time.Second); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	if !bytes.Equal(col.bytes(), pcm) {
		t.Fatal("delivered PCM diverged from source")
	}
	// The handler only serves on a valid Digest response, so a delivery proves
	// Digest was chosen over the Basic offer.
	if got := st.attemptCount(); got != 1 {
		t.Fatalf("authorized attempts = %d, want 1", got)
	}
}

// A second 401 carrying stale=true earns exactly one more retry under the
// rotated nonce, then succeeds.
func TestDigestStaleRetry(t *testing.T) {
	pcm := pcmMono(200)
	st, h := serveDigest(&digestConfig{
		realm: testRealmCam, algorithm: testAlgSHA256, qop: testQOPAuth,
		user: testUser, pass: testPass, body: digestWAVBody(pcm),
		nonce: "n1", staleNonce: "n2",
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	var col collector
	c := openOK(t, srv, Config{Username: testUser, Password: testPass, OnFrame: col.onFrame})
	if err := waitResult(t, c, 5*time.Second); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	if !bytes.Equal(col.bytes(), pcm) {
		t.Fatal("delivered PCM diverged from source")
	}
	// One attempt under n1 (answered stale), one under n2 (accepted).
	if got := st.attemptCount(); got != 2 {
		t.Fatalf("authorized attempts = %d, want 2", got)
	}
}

// Wrong credentials are rejected: the server re-challenges (not stale), the
// client gives up, and the 401 surfaces as a *StatusError.
func TestDigestBadCredentials(t *testing.T) {
	_, h := serveDigest(&digestConfig{
		realm: testRealmCam, algorithm: testAlgMD5, qop: testQOPAuth,
		user: testUser, pass: testPass, body: digestWAVBody(pcmMono(10)),
		nonce: "n1",
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	_, err := Open(context.Background(), Config{URL: srv.URL, Username: testUser, Password: "wrong"})
	if !errors.Is(err, ErrBadStatus) {
		t.Fatalf("Open = %v, want ErrBadStatus", err)
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusUnauthorized {
		t.Fatalf("StatusError = %+v, want Code 401", se)
	}
}

// Over https the client sends preemptive Basic, but a Digest-only server ignores
// it and challenges; the client then answers Digest and streams.
func TestDigestOverHTTPSAfterPreemptiveBasic(t *testing.T) {
	pcm := pcmMono(200)
	st, h := serveDigest(&digestConfig{
		realm: testRealmCam, algorithm: testAlgMD5, qop: testQOPAuth,
		user: testUser, pass: testPass, body: digestWAVBody(pcm),
		nonce: "n1",
	})
	srv := httptest.NewTLSServer(h)
	defer srv.Close()

	var col collector
	c := openOK(t, srv, Config{Username: testUser, Password: testPass, InsecureTLS: true, OnFrame: col.onFrame})
	if err := waitResult(t, c, 5*time.Second); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	if !bytes.Equal(col.bytes(), pcm) {
		t.Fatal("delivered PCM diverged from source")
	}
	if got := st.attemptCount(); got != 1 {
		t.Fatalf("authorized attempts = %d, want 1", got)
	}
}

// A 401 with no credentials configured is never answered; it surfaces as a
// *StatusError exactly as before, so the challenge-response path is inert
// without credentials.
func TestUnauthorizedWithoutCredentials(t *testing.T) {
	_, h := serveDigest(&digestConfig{
		realm: testRealmCam, algorithm: testAlgMD5, qop: testQOPAuth,
		user: testUser, pass: testPass, body: digestWAVBody(pcmMono(10)),
		nonce: "n1",
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	_, err := Open(context.Background(), Config{URL: srv.URL})
	if !errors.Is(err, ErrBadStatus) {
		t.Fatalf("Open = %v, want ErrBadStatus", err)
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusUnauthorized {
		t.Fatalf("StatusError = %+v, want Code 401", se)
	}
}

// An unanswerable Digest challenge (a -sess algorithm variant this client does
// not implement), with no Basic fallback, surfaces as the 401 *StatusError
// rather than a broken authorized request or a hang.
func TestDigestUnanswerableChallengeSurfacesStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Digest realm="cam", nonce="n1", algorithm=MD5-sess`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := Open(context.Background(), Config{URL: srv.URL, Username: testUser, Password: testPass})
	if !errors.Is(err, ErrBadStatus) {
		t.Fatalf("Open = %v, want ErrBadStatus", err)
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusUnauthorized {
		t.Fatalf("StatusError = %+v, want Code 401", se)
	}
}

// Over plaintext http without the opt-in the first request is sent bare. A
// server that does not challenge streams normally, and that first request must
// have carried no credentials: the password never reaches the wire before a
// challenge. This is the behavior change from the old up-front refusal.
func TestPlaintextCredsNoChallengeStreamsBare(t *testing.T) {
	pcm := pcmMono(100)
	var auth authCapture
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		auth.set(u, p, ok)
		w.Header().Set("Content-Type", "audio/wav")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(digestWAVBody(pcm))
	}))
	defer srv.Close()

	var col collector
	c := openOK(t, srv, Config{Username: testUser, Password: testPass, OnFrame: col.onFrame})
	if err := waitResult(t, c, 5*time.Second); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	if !bytes.Equal(col.bytes(), pcm) {
		t.Fatal("delivered PCM diverged from source")
	}
	if _, _, ok := auth.get(); ok {
		t.Fatal("the bare first request carried Basic credentials over plaintext without opt-in")
	}
}

// syncBuffer is a race-safe io.Writer for capturing slog output written on the
// Open goroutine while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// Sending preemptive Basic over plaintext (with the opt-in) logs a warning; over
// TLS, where Basic is not sent in the clear, it does not.
func TestPlaintextBasicWarns(t *testing.T) {
	body := digestWAVBody(pcmMono(50))

	t.Run("plaintext opt-in warns", func(t *testing.T) {
		sb := &syncBuffer{}
		srv := httptest.NewServer(serveStatic("audio/wav", body))
		defer srv.Close()
		var col collector
		c := openOK(t, srv, Config{
			Username: testUser, Password: testPass, AllowInsecureAuth: true,
			Logger: slog.New(slog.NewTextHandler(sb, nil)), OnFrame: col.onFrame,
		})
		_ = waitResult(t, c, 5*time.Second)
		if !strings.Contains(sb.String(), "plaintext") {
			t.Fatalf("expected a plaintext-credentials warning, got %q", sb.String())
		}
	})

	t.Run("tls does not warn", func(t *testing.T) {
		sb := &syncBuffer{}
		srv := httptest.NewTLSServer(serveStatic("audio/wav", body))
		defer srv.Close()
		var col collector
		c := openOK(t, srv, Config{
			Username: testUser, Password: testPass, InsecureTLS: true,
			Logger: slog.New(slog.NewTextHandler(sb, nil)), OnFrame: col.onFrame,
		})
		_ = waitResult(t, c, 5*time.Second)
		if strings.Contains(sb.String(), "plaintext") {
			t.Fatalf("unexpected plaintext warning over TLS: %q", sb.String())
		}
	})
}

// A server that answers every authorized attempt with stale=true must still
// terminate: the client takes its single permitted stale retry and then
// surfaces the 401 as a *StatusError rather than looping forever. The attempt
// count pins the bound at exactly two authorized requests.
func TestDigestAlwaysStaleTerminates(t *testing.T) {
	st := &digestState{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Authorization"), "Digest ") {
			st.mu.Lock()
			st.attempts++
			st.mu.Unlock()
			w.Header().Add("WWW-Authenticate", digestChallenge(testRealmCam, "n-stale", testAlgMD5, testQOPAuth, true))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Add("WWW-Authenticate", digestChallenge(testRealmCam, "n-init", testAlgMD5, testQOPAuth, false))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := Open(context.Background(), Config{URL: srv.URL, Username: testUser, Password: testPass})
	if !errors.Is(err, ErrBadStatus) {
		t.Fatalf("Open = %v, want ErrBadStatus", err)
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusUnauthorized {
		t.Fatalf("StatusError = %+v, want Code 401", se)
	}
	// One attempt under the initial nonce, one under the single permitted stale
	// retry, then it gives up: the retry is bounded, not a loop.
	if got := st.attemptCount(); got != 2 {
		t.Fatalf("authorized attempts = %d, want 2", got)
	}
}
