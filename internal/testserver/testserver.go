package testserver

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/md5" //nolint:gosec // reproduces the client's RFC 7616 Digest math in a test double, not a security primitive.
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tphakala/go-audio-stream/rtsp"
)

// readTimeout bounds a single server-side read, re-armed per read. Long enough
// not to fire on a scripted exchange, short enough that a stalled client fails
// the test rather than hanging the package. A test that deliberately holds a
// session idle longer than this (a keepalive-interval test, say) must raise it,
// or the server will close mid-test and the client will report a connection
// error instead of the behaviour under test.
const readTimeout = 10 * time.Second

// readChunk is the per-read buffer size for the connection accumulation
// buffer. It is a plain constant, not tuned: the wire units are small and
// the M4a parsers enforce their own caps.
const readChunk = 4096

// Server is a scripted in-process RTSP server on a loopback listener. It
// accepts connections and drives each with the handler supplied to New,
// which runs in its own goroutine and owns that connection's exchange. The
// server is for tests only and is never exported from the module.
type Server struct {
	t       *testing.T
	opts    Options
	ln      net.Listener
	port    int
	certPEM []byte

	mu     sync.Mutex
	conns  []net.Conn
	closed bool
	wg     sync.WaitGroup
}

// Options configures a Server.
type Options struct {
	// TLS makes the listener serve rtsps using an in-memory self-signed
	// certificate. CertPEM exposes that certificate so a test can build a
	// verifying tls.Config.
	TLS bool
	// Handle runs once per accepted connection with a ServerConn bound to
	// it. When Handle returns, the connection is closed. Required.
	Handle func(*ServerConn)
}

// New starts a Server and registers cleanup on t. It listens on
// 127.0.0.1:0, spawns an accept loop, and stops on t.Cleanup. It fails the
// test on any listen error.
func New(t *testing.T, opts Options) *Server {
	t.Helper()
	if opts.Handle == nil {
		t.Fatal("testserver: Options.Handle is required")
	}
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("testserver: listen: %v", err)
	}
	s := &Server{t: t, opts: opts}
	tcpAddr, ok := base.Addr().(*net.TCPAddr)
	if !ok {
		_ = base.Close()
		t.Fatalf("testserver: listener address is not TCP: %T", base.Addr())
	}
	s.port = tcpAddr.Port

	ln := base
	if opts.TLS {
		cert, certPEM, cerr := generateSelfSigned()
		if cerr != nil {
			_ = base.Close()
			t.Fatalf("testserver: generate cert: %v", cerr)
		}
		s.certPEM = certPEM
		ln = tls.NewListener(base, &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		})
	}
	s.ln = ln

	s.wg.Add(1)
	go s.acceptLoop()
	t.Cleanup(s.stop)
	return s
}

// URL returns the rtsp:// (or rtsps://) base URL clients dial, including
// the ephemeral port and the given path (leading slash optional).
func (s *Server) URL(path string) string {
	scheme := "rtsp"
	if s.opts.TLS {
		scheme = "rtsps"
	}
	if path == "" || path[0] != '/' {
		path = "/" + path
	}
	return scheme + "://127.0.0.1:" + strconv.Itoa(s.port) + path
}

// CertPEM returns the PEM-encoded server certificate for a TLS Server, so
// a test can construct a RootCAs pool for verified-TLS assertions. It is
// nil for a non-TLS Server.
func (s *Server) CertPEM() []byte {
	if s.certPEM == nil {
		return nil
	}
	out := make([]byte, len(s.certPEM))
	copy(out, s.certPEM)
	return out
}

// acceptLoop accepts connections until the listener is closed, spawning one
// handler goroutine per connection.
func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			// stop() has already snapshotted and closed the connection set, so
			// this conn would never be closed and wg.Wait would block forever
			// on its handler, hanging the whole test binary inside t.Cleanup.
			s.mu.Unlock()
			_ = conn.Close()
			return
		}
		s.conns = append(s.conns, conn)
		s.mu.Unlock()
		s.wg.Add(1)
		go s.handle(conn)
	}
}

// handle runs the user handler for one connection and closes it afterward.
func (s *Server) handle(conn net.Conn) {
	defer s.wg.Done()
	defer func() { _ = conn.Close() }()
	sc := &ServerConn{t: s.t, conn: conn}
	s.opts.Handle(sc)
}

// stop closes the listener and every accepted connection, then waits for
// the accept loop and all handler goroutines to exit. Registered on
// t.Cleanup so a test leaves no server goroutines behind.
func (s *Server) stop() {
	_ = s.ln.Close()
	s.mu.Lock()
	s.closed = true
	conns := s.conns
	s.conns = nil
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
	s.wg.Wait()
}

// generateSelfSigned builds a fresh P-256 self-signed certificate valid for
// 127.0.0.1 and returns both the tls.Certificate the listener serves and
// the PEM encoding a test verifies against.
func generateSelfSigned() (tls.Certificate, []byte, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, certPEM, nil
}

// ServerConn is one accepted connection with request/response and
// injection primitives. All methods are called from the handler goroutine.
type ServerConn struct {
	t    *testing.T
	conn net.Conn

	// buf accumulates unparsed bytes; start is the parse offset into buf.
	buf   []byte
	start int
	// cseq is the monotonic CSeq allocator for server-initiated requests.
	cseq int
	// skipped records interleaved frames ReadRequest and ReadResponse stepped
	// over, so a test can assert on client-sent data that preceded a request.
	skipped []rtsp.InterleavedFrame
}

// fill compacts the consumed prefix of buf and appends one read from the
// socket. It returns the read error only when no new bytes arrived, so a
// short read that carried a full unit is still parsed before the error
// surfaces on the next call.
func (sc *ServerConn) fill() error {
	if sc.start > 0 {
		sc.buf = sc.buf[:copy(sc.buf, sc.buf[sc.start:])]
		sc.start = 0
	}
	// Without a deadline a client that connects and then goes quiet (exactly
	// what a bug in the code under test produces) blocks here forever, so the
	// package dies on the 10-minute timeout with a stack pointing at the
	// harness rather than failing at the assertion.
	_ = sc.conn.SetReadDeadline(time.Now().Add(readTimeout))
	var tmp [readChunk]byte
	n, err := sc.conn.Read(tmp[:])
	if n > 0 {
		sc.buf = append(sc.buf, tmp[:n]...)
		return nil
	}
	return err
}

// readNext reads and classifies the next wire unit: exactly one of req,
// resp, or frame is non-nil on success. Interleaved payloads are copied out
// of buf so they stay valid after the buffer is reused.
func (sc *ServerConn) readNext() (req *rtsp.Request, resp *rtsp.Response, frame *rtsp.InterleavedFrame, err error) {
	for {
		avail := sc.buf[sc.start:]
		switch rtsp.ClassifyStream(avail) {
		case rtsp.FrameNeedMore:
			if e := sc.fill(); e != nil {
				return nil, nil, nil, e
			}
		case rtsp.FrameInterleaved:
			f, n, e := rtsp.ParseInterleaved(avail)
			if errors.Is(e, rtsp.ErrIncomplete) {
				if fe := sc.fill(); fe != nil {
					return nil, nil, nil, fe
				}
				continue
			}
			if e != nil {
				return nil, nil, nil, e
			}
			payload := append([]byte(nil), f.Payload...)
			sc.start += n
			return nil, nil, &rtsp.InterleavedFrame{Channel: f.Channel, Payload: payload}, nil
		case rtsp.FrameRequest:
			r, n, e := rtsp.ParseRequest(avail)
			if errors.Is(e, rtsp.ErrIncomplete) {
				if fe := sc.fill(); fe != nil {
					return nil, nil, nil, fe
				}
				continue
			}
			if e != nil {
				return nil, nil, nil, e
			}
			sc.start += n
			return r, nil, nil, nil
		case rtsp.FrameResponse:
			r, n, e := rtsp.ParseResponse(avail)
			if errors.Is(e, rtsp.ErrIncomplete) {
				if fe := sc.fill(); fe != nil {
					return nil, nil, nil, fe
				}
				continue
			}
			if e != nil {
				return nil, nil, nil, e
			}
			sc.start += n
			return nil, r, nil, nil
		case rtsp.FrameUnknown:
			return nil, nil, nil, errors.New("testserver: unrecognized bytes on connection")
		default:
			return nil, nil, nil, errors.New("testserver: unexpected frame kind")
		}
	}
}

// ReadRequest reads and parses the next client RTSP request. Interleaved
// data the client sends (RTCP Receiver Reports) is returned via ReadAny;
// ReadRequest skips leading interleaved frames and returns the next text
// request, recording skipped frames for assertions.
func (sc *ServerConn) ReadRequest() (*rtsp.Request, error) {
	for {
		req, resp, frame, err := sc.readNext()
		switch {
		case err != nil:
			return nil, err
		case frame != nil:
			sc.recordSkipped(*frame)
		case resp != nil:
			return nil, fmt.Errorf("testserver: expected request, got response %d", resp.StatusCode)
		default:
			return req, nil
		}
	}
}

// ReadAny reads the next unit off the wire, classified: a request, or an
// interleaved frame (channel and payload). Used by tests that assert on
// client-sent RTCP or on request ordering relative to injected data.
func (sc *ServerConn) ReadAny() (req *rtsp.Request, frame *rtsp.InterleavedFrame, err error) {
	req, resp, frame, err := sc.readNext()
	if err != nil {
		return nil, nil, err
	}
	if resp != nil {
		return nil, nil, fmt.Errorf("testserver: expected request or frame, got response %d", resp.StatusCode)
	}
	return req, frame, nil
}

// ReadResponse reads and parses the next RTSP response: the client's answer
// to a server-initiated request (see SendServerRequest). Like ReadRequest,
// it skips leading interleaved frames and records them via SkippedFrames,
// and it returns an error when the next text unit is a request rather than a
// response.
func (sc *ServerConn) ReadResponse() (*rtsp.Response, error) {
	for {
		req, resp, frame, err := sc.readNext()
		switch {
		case err != nil:
			return nil, err
		case frame != nil:
			sc.recordSkipped(*frame)
		case req != nil:
			return nil, fmt.Errorf("testserver: expected response, got request %s", req.Method)
		default:
			return resp, nil
		}
	}
}

// SkippedFrames returns a copy of the interleaved frames ReadRequest and
// ReadResponse stepped over so far, in arrival order, for tests that assert on client-sent data.
func (sc *ServerConn) SkippedFrames() []rtsp.InterleavedFrame {
	return slices.Clone(sc.skipped)
}

// recordSkipped stores f with its payload copied out of the read buffer.
// ParseInterleaved returns a Payload aliasing that buffer and fill compacts it
// in place, so keeping the alias would let every later read silently overwrite
// a frame recorded earlier. Cloning the outer slice in SkippedFrames does not
// help: it copies the headers, not the bytes they point at.
func (sc *ServerConn) recordSkipped(f rtsp.InterleavedFrame) {
	f.Payload = slices.Clone(f.Payload)
	sc.skipped = append(sc.skipped, f)
}

// Respond writes a response to req, echoing req.CSeq, with the given
// status, headers (may be nil), and body (may be nil). Content-Length is
// set by MarshalResponse.
func (sc *ServerConn) Respond(req *rtsp.Request, code int, reason string, headers rtsp.Header, body []byte) error {
	if req == nil {
		// A handler that logs a ReadRequest error and forgets to return would
		// otherwise panic here, and a panic on this goroutine aborts the whole
		// test binary with a stack that points nowhere near the mistake.
		return errors.New("testserver: Respond called with a nil request")
	}
	raw, err := rtsp.MarshalResponse(&rtsp.Response{
		StatusCode: code,
		Reason:     reason,
		CSeq:       req.CSeq,
		Header:     headers,
		Body:       body,
	})
	if err != nil {
		return err
	}
	_, err = sc.conn.Write(raw)
	return err
}

// InjectFrame writes one interleaved frame (channel plus payload) to the
// client. Used to push RTP and RTCP after PLAY, and to push early data
// before the PLAY response.
func (sc *ServerConn) InjectFrame(channel int, payload []byte) error {
	raw, err := rtsp.MarshalInterleaved(channel, payload)
	if err != nil {
		return err
	}
	_, err = sc.conn.Write(raw)
	return err
}

// WriteRaw writes bytes straight to the connection, bypassing every framing
// helper, so a test can inject a byte run that desynchronizes the client's
// framing loop ahead of a real unit. It is the escape hatch for reader-framing
// tests; use InjectFrame, Respond, or SendServerRequest for everything else.
func (sc *ServerConn) WriteRaw(b []byte) error {
	_, err := sc.conn.Write(b)
	return err
}

// SendServerRequest writes a server-initiated request (for example a
// mid-session OPTIONS or a TEARDOWN) with a fresh server CSeq and returns
// that CSeq so the test can await the client's 200 OK.
func (sc *ServerConn) SendServerRequest(method, url string, headers rtsp.Header) (cseq int, err error) {
	sc.cseq++
	raw, err := rtsp.MarshalRequest(&rtsp.Request{
		Method: method,
		URL:    url,
		CSeq:   sc.cseq,
		Header: headers,
	})
	if err != nil {
		return 0, err
	}
	if _, err := sc.conn.Write(raw); err != nil {
		return 0, err
	}
	return sc.cseq, nil
}

// Close drops the underlying TCP connection immediately, with no TEARDOWN
// handshake, simulating an abrupt disconnect.
func (sc *ServerConn) Close() error {
	return sc.conn.Close()
}

// HandshakeConfig parameterizes the standard exchange Handshake performs.
type HandshakeConfig struct {
	SDP             string    // DESCRIBE response body (application/sdp)
	ContentType     string    // "" defaults to "application/sdp"
	ContentBase     string    // "" omits the Content-Base header
	ContentLocation string    // "" omits the Content-Location header
	SessionID       string    // Session id echoed from SETUP on
	SessionTimeout  int       // seconds; 0 omits the ;timeout= parameter
	PublicMethods   []string  // OPTIONS Public header; nil omits it
	InterleavedBase int       // first RTP channel the server assigns (renumber quirk); RTCP is +1, next track +2
	Auth            *AuthSpec // nil = no auth required
	TrackControls   []string  // reserved; declared for the SETUP URI assertion Handshake does not yet make
	RangeEcho       bool      // when true, echo the PLAY Range header back
	RTPInfo         string    // "" omits RTP-Info on the PLAY response
}

// AuthSpec makes Handshake demand authentication before DESCRIBE succeeds.
type AuthSpec struct {
	Scheme    string // "Basic" or "Digest"
	Realm     string
	Nonce     string
	Algorithm string // "", "MD5", or "SHA-256" (Digest)
	QOP       string // "" (legacy RFC 2069) or "auth"
	Username  string
	Password  string
	Stale     bool // when set, the first post-auth 401 carries stale=true once
	// StaleNonce is the nonce the stale=true challenge rotates to. Empty reuses
	// Nonce, which makes the retry indistinguishable from a plain resend and
	// leaves a client that ignores the rotation undetectable.
	StaleNonce string
}

// ChannelPair is the RTP/RTCP interleaved channel pair the server assigned
// to one track during Handshake.
type ChannelPair struct{ RTP, RTCP int }

// Handshake runs OPTIONS, DESCRIBE, SETUP (one per m= section in cfg.SDP),
// and PLAY, applying cfg, and returns once PLAY has been answered 200, with
// the connection left in the playing state and the negotiated interleaved
// channels reported. The test then injects frames.
//
// It fails the test (via the stored *testing.T) when a request arrives with an
// unexpected method, and when cfg.Auth is set and the Authorization header
// does not match. That is the whole of what it asserts: it does NOT check the
// request URI, the Transport header, the Session echo, or CSeq monotonicity,
// so a passing Handshake means the method sequence was right, not that the
// client spoke correct RTSP. A test that cares about those must assert on the
// requests itself.
//
//nolint:gocritic // hugeParam: cfg is a value because the scripted-handshake API takes a config literal; Handshake runs once per connection, not on a hot path.
func (sc *ServerConn) Handshake(cfg HandshakeConfig) (channels []ChannelPair, err error) {
	sc.t.Helper()
	if err := sc.handshakeOptions(&cfg); err != nil {
		return nil, err
	}
	if err := sc.handshakeDescribe(&cfg); err != nil {
		return nil, err
	}
	pairs, err := sc.handshakeSetup(&cfg)
	if err != nil {
		return nil, err
	}
	if err := sc.handshakePlay(&cfg); err != nil {
		return nil, err
	}
	return pairs, nil
}

// handshakeOptions reads OPTIONS and answers 200, advertising cfg's Public
// methods when any are configured.
func (sc *ServerConn) handshakeOptions(cfg *HandshakeConfig) error {
	req, err := sc.ReadRequest()
	if err != nil {
		return err
	}
	sc.expectMethod(req, "OPTIONS")
	h := rtsp.Header{}
	if len(cfg.PublicMethods) > 0 {
		h.Set("Public", strings.Join(cfg.PublicMethods, ", "))
	}
	return sc.Respond(req, rtsp.StatusOK, "OK", h, nil)
}

// handshakeDescribe reads DESCRIBE (challenging first when cfg.Auth is set)
// and answers 200 with the SDP body and content headers.
func (sc *ServerConn) handshakeDescribe(cfg *HandshakeConfig) error {
	req, err := sc.readDescribeWithAuth(cfg)
	if err != nil {
		return err
	}
	contentType := cfg.ContentType
	if contentType == "" {
		contentType = "application/sdp"
	}
	h := rtsp.Header{}
	h.Set("Content-Type", contentType)
	if cfg.ContentBase != "" {
		h.Set("Content-Base", cfg.ContentBase)
	}
	if cfg.ContentLocation != "" {
		h.Set("Content-Location", cfg.ContentLocation)
	}
	return sc.Respond(req, rtsp.StatusOK, "OK", h, []byte(cfg.SDP))
}

// readDescribeWithAuth returns the DESCRIBE request that should be answered
// with the SDP. With cfg.Auth set it first challenges an unauthenticated
// DESCRIBE with 401, then optionally issues one stale re-challenge, and
// validates the final Authorization header.
func (sc *ServerConn) readDescribeWithAuth(cfg *HandshakeConfig) (*rtsp.Request, error) {
	req, err := sc.ReadRequest()
	if err != nil {
		return nil, err
	}
	sc.expectMethod(req, "DESCRIBE")
	if cfg.Auth == nil {
		return req, nil
	}

	if req.Header.Get("Authorization") == "" {
		if err := sc.sendChallenge(req, cfg.Auth, false); err != nil {
			return nil, err
		}
		req, err = sc.readDescribeAgain()
		if err != nil {
			return nil, err
		}
	}
	spec := cfg.Auth
	if spec.Stale {
		if err := sc.sendChallenge(req, spec, true); err != nil {
			return nil, err
		}
		req, err = sc.readDescribeAgain()
		if err != nil {
			return nil, err
		}
		if spec.StaleNonce != "" {
			// Validate the retry against the ROTATED nonce. Checking it against
			// the original would accept a client that ignored the rotation
			// entirely, which is the whole behaviour the stale path exists for.
			rotated := *spec
			rotated.Nonce = spec.StaleNonce
			spec = &rotated
		}
	}
	sc.validateAuthorization(req, spec)
	return req, nil
}

// readDescribeAgain reads the retried DESCRIBE and asserts its method.
func (sc *ServerConn) readDescribeAgain() (*rtsp.Request, error) {
	req, err := sc.ReadRequest()
	if err != nil {
		return nil, err
	}
	sc.expectMethod(req, "DESCRIBE")
	return req, nil
}

// sendChallenge answers req with 401 and a WWW-Authenticate header built
// from spec; stale adds stale=true for a Digest challenge.
func (sc *ServerConn) sendChallenge(req *rtsp.Request, spec *AuthSpec, stale bool) error {
	h := rtsp.Header{}
	h.Set("WWW-Authenticate", buildChallenge(spec, stale))
	return sc.Respond(req, rtsp.StatusUnauthorized, "Unauthorized", h, nil)
}

// buildChallenge renders a WWW-Authenticate value for spec.
func buildChallenge(spec *AuthSpec, stale bool) string {
	if strings.EqualFold(spec.Scheme, "Basic") {
		return `Basic realm="` + spec.Realm + `"`
	}
	nonce := spec.Nonce
	if stale && spec.StaleNonce != "" {
		nonce = spec.StaleNonce
	}
	parts := make([]string, 0, 5)
	parts = append(parts, `realm="`+spec.Realm+`"`, `nonce="`+nonce+`"`)
	if spec.Algorithm != "" {
		parts = append(parts, "algorithm="+spec.Algorithm)
	}
	if spec.QOP != "" {
		parts = append(parts, `qop="`+spec.QOP+`"`)
	}
	if stale {
		parts = append(parts, "stale=true")
	}
	return "Digest " + strings.Join(parts, ", ")
}

// validateAuthorization checks the retried DESCRIBE carries an Authorization
// header consistent with spec. Basic is verified exactly; Digest is checked
// for scheme and the echoed challenge nonce (the digest response math is
// covered by the rtsp package's own tests).
func (sc *ServerConn) validateAuthorization(req *rtsp.Request, spec *AuthSpec) {
	sc.t.Helper()
	got := req.Header.Get("Authorization")
	if got == "" {
		sc.t.Errorf("testserver: DESCRIBE missing Authorization header")
		return
	}
	switch {
	case strings.EqualFold(spec.Scheme, "Basic"):
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte(spec.Username+":"+spec.Password))
		if got != want {
			sc.t.Errorf("testserver: Basic authorization mismatch")
		}
	case strings.EqualFold(spec.Scheme, "Digest"):
		if !strings.HasPrefix(got, "Digest ") {
			sc.t.Errorf("testserver: expected Digest authorization")
			return
		}
		sc.verifyDigest(req, spec)
	default:
		sc.t.Errorf("testserver: unsupported auth scheme %q", spec.Scheme)
	}
}

// verifyDigest independently recomputes the RFC 7616 (or RFC 2069 legacy)
// Digest response from the client's Authorization header and spec's
// credentials, failing the test on any mismatch. This is what makes the auth
// integration tests validate the client's digest math end to end rather than
// only checking that some header was sent.
func (sc *ServerConn) verifyDigest(req *rtsp.Request, spec *AuthSpec) {
	sc.t.Helper()
	params := parseDigestParams(req.Header.Get("Authorization"))
	if params["nonce"] != spec.Nonce {
		sc.t.Errorf("testserver: Digest nonce = %q, want %q", params["nonce"], spec.Nonce)
		return
	}
	// The digest URI is checked against the request line rather than merely
	// fed into the hash. Deriving HA2 from whatever the client claimed would
	// verify the client against itself: a client that digests over the session
	// base URL instead of the track control URL is internally consistent and
	// would pass here while a real camera answers it 401.
	if params["uri"] != req.URL {
		sc.t.Errorf("testserver: Digest uri = %q, want the request URI %q", params["uri"], req.URL)
		return
	}
	// Likewise the qop: taking the client's word for it would accept a legacy
	// response to a qop challenge and a qop response to a legacy one, which are
	// the two ways the RFC 2069 and RFC 7616 formulas get confused.
	if (spec.QOP == "") != (params["qop"] == "") {
		sc.t.Errorf("testserver: Digest qop = %q, want %q", params["qop"], spec.QOP)
		return
	}
	if spec.QOP != "" && (params["nc"] == "" || params["cnonce"] == "") {
		sc.t.Errorf("testserver: Digest with qop must carry nc and cnonce, got nc=%q cnonce=%q",
			params["nc"], params["cnonce"])
		return
	}
	h := digestHasher(spec.Algorithm)
	ha1 := h(spec.Username + ":" + spec.Realm + ":" + spec.Password)
	ha2 := h(req.Method + ":" + req.URL)
	var want string
	if qop := params["qop"]; qop != "" {
		want = h(strings.Join([]string{ha1, spec.Nonce, params["nc"], params["cnonce"], qop, ha2}, ":"))
	} else {
		want = h(strings.Join([]string{ha1, spec.Nonce, ha2}, ":"))
	}
	if params["response"] != want {
		sc.t.Errorf("testserver: Digest response mismatch: got %q, want %q", params["response"], want)
	}
}

// parseDigestParams splits a Digest Authorization value into its auth-params,
// lowercasing names and stripping surrounding quotes. It is sufficient for the
// test payloads, whose values carry no embedded commas.
func parseDigestParams(header string) map[string]string {
	params := map[string]string{}
	for _, part := range strings.Split(strings.TrimPrefix(header, "Digest "), ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		params[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	return params
}

// digestHasher returns a hex-hashing function for the challenge algorithm:
// SHA-256 when named, otherwise MD5, which RFC 7616 section 3.3 makes the
// default when the algorithm directive is absent (it also registers SHA-256 and
// discourages MD5, which is why both are supported here). It reproduces the
// client's arithmetic and is never used as a security primitive.
func digestHasher(algorithm string) func(string) string {
	newHash := md5.New
	if strings.EqualFold(algorithm, "SHA-256") {
		newHash = sha256.New
	}
	return func(s string) string {
		hh := newHash()
		_, _ = hh.Write([]byte(s))
		return hex.EncodeToString(hh.Sum(nil))
	}
}

// handshakeSetup reads one SETUP per m= section, assigning interleaved
// channels from cfg.InterleavedBase, and answers each 200 with a Transport
// and Session header. It returns the assigned channel pairs.
func (sc *ServerConn) handshakeSetup(cfg *HandshakeConfig) ([]ChannelPair, error) {
	n := countMediaSections(cfg.SDP)
	pairs := make([]ChannelPair, 0, n)
	for i := 0; i < n; i++ {
		req, err := sc.ReadRequest()
		if err != nil {
			return nil, err
		}
		sc.expectMethod(req, "SETUP")
		rtpCh := cfg.InterleavedBase + 2*i
		rtcpCh := rtpCh + 1
		// An interleaved channel is a single byte on the wire, so an
		// InterleavedBase high enough to push a pair past 255 produces a
		// Transport header the client accepts and frames it can never send:
		// MarshalInterleaved rejects the channel later, far from the
		// misconfiguration that caused it.
		if cfg.InterleavedBase < 0 || rtcpCh > 255 {
			return nil, fmt.Errorf("testserver: InterleavedBase %d puts track %d channels (%d-%d) outside 0..255",
				cfg.InterleavedBase, i, rtpCh, rtcpCh)
		}
		h := rtsp.Header{}
		h.Set("Transport", rtsp.BuildTransport(rtpCh, rtcpCh))
		h.Set("Session", sessionHeader(cfg))
		if err := sc.Respond(req, rtsp.StatusOK, "OK", h, nil); err != nil {
			return nil, err
		}
		pairs = append(pairs, ChannelPair{RTP: rtpCh, RTCP: rtcpCh})
	}
	return pairs, nil
}

// handshakePlay reads PLAY and answers 200, optionally echoing Range and
// attaching RTP-Info.
func (sc *ServerConn) handshakePlay(cfg *HandshakeConfig) error {
	req, err := sc.ReadRequest()
	if err != nil {
		return err
	}
	sc.expectMethod(req, "PLAY")
	h := rtsp.Header{}
	h.Set("Session", sessionHeader(cfg))
	if cfg.RangeEcho {
		if r := req.Header.Get("Range"); r != "" {
			h.Set("Range", r)
		}
	}
	if cfg.RTPInfo != "" {
		h.Set("RTP-Info", cfg.RTPInfo)
	}
	return sc.Respond(req, rtsp.StatusOK, "OK", h, nil)
}

// expectMethod fails the test when req.Method is not want.
func (sc *ServerConn) expectMethod(req *rtsp.Request, want string) {
	sc.t.Helper()
	if req.Method != want {
		sc.t.Errorf("testserver: expected %s, got %s", want, req.Method)
	}
}

// sessionHeader builds the Session header value from cfg, appending
// ";timeout=" only when a positive timeout is configured.
func sessionHeader(cfg *HandshakeConfig) string {
	if cfg.SessionTimeout > 0 {
		return cfg.SessionID + ";timeout=" + strconv.Itoa(cfg.SessionTimeout)
	}
	return cfg.SessionID
}

// countMediaSections counts the m= lines in an SDP body, tolerating LF or
// CRLF line endings.
func countMediaSections(sdp string) int {
	count := 0
	for _, line := range strings.Split(sdp, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "m=") {
			count++
		}
	}
	return count
}
