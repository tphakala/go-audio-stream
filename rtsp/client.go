package rtsp

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
)

// Internal tuning constants.
const (
	// teardownDeadline bounds the best-effort TEARDOWN write and its response
	// read during shutdown.
	teardownDeadline = 2 * time.Second
	// maxReadBuffer is a backstop ceiling on the reader accumulation buffer.
	// MaxHeaderBytes and MaxBodySize cap every well-formed unit below it, so it
	// is reachable only on a desynchronized or hostile stream.
	maxReadBuffer = MaxHeaderBytes + MaxBodySize
	// maxResyncBytes bounds byte-at-a-time resynchronization before the
	// reader declares a fatal framing error.
	maxResyncBytes = 4096
)

// Exported errors, in addition to the ones reused from the audiostream root
// package and the wire layer.
var (
	// ErrServerTeardown ends Wait when the server sent a TEARDOWN.
	ErrServerTeardown = errors.New("rtsp: server sent TEARDOWN")
	// ErrConnectionClosed wraps the underlying cause when the control
	// connection was lost unexpectedly.
	ErrConnectionClosed = errors.New("rtsp: connection closed")
	// ErrAuthFailed ends a request whose 401 this client could not answer: no
	// usable challenge was offered, or the credentials were still refused after
	// the permitted retries. The returned error wraps the *UnauthorizedError, so
	// errors.As recovers the challenge (its realm, for prompting) even though
	// the retry was automatic.
	ErrAuthFailed = errors.New("rtsp: authentication failed")
	// ErrRequestTimeout ends a request when the server did not answer within
	// Config.Timeout. It is the request-path counterpart to
	// audiostream.ErrReadTimeout, which covers the read-idle watchdog: both
	// mean the peer went quiet, so a caller retrying on one usually wants to
	// retry on the other.
	ErrRequestTimeout = errors.New("rtsp: request timeout")
)

// ChannelPair is the RTP and RTCP interleaved channel numbers the server
// assigned to one track during Setup.
type ChannelPair struct {
	// TrackID is the track the channels carry.
	TrackID int
	// RTP is the interleaved channel carrying RTP.
	RTP int
	// RTCP is the interleaved channel carrying RTCP.
	RTCP int
}

// SessionInfo is a read-only snapshot of the negotiated session, for
// diagnostics such as a handshake walkthrough tool. It carries no credentials
// and no counters. Dial sets KeepaliveMethod and Server. Setup sets Channels,
// and sets SessionID and SessionTimeout from the first SETUP response that
// carries a Session header, so they stay empty against a server that omits it.
// AuthScheme stays AuthNone until a 401 has been answered, so it reports
// AuthNone against a server that never challenges.
type SessionInfo struct {
	// SessionID is the negotiated RTSP session identifier, "" before Setup.
	SessionID string
	// SessionTimeout is the advertised (or defaulted) session timeout.
	SessionTimeout time.Duration
	// AuthScheme is the authentication scheme in use, AuthNone before any
	// challenge has been answered. It is the same type the wire layer's
	// ParseChallenges and SelectChallenge use, so a caller can compare the two
	// directly.
	AuthScheme AuthScheme
	// KeepaliveMethod is the negotiated keepalive method ("OPTIONS" or
	// "GET_PARAMETER"), "" before Dial's OPTIONS completes.
	KeepaliveMethod string
	// Server is the raw RTSP Server response header (product/firmware string),
	// captured from Dial's OPTIONS response, "" when the server omitted it. It
	// is a diagnostic aid for identifying a camera's RTSP stack; it is not
	// interpreted and may carry arbitrary vendor text.
	Server string
	// Channels lists the assigned interleaved channel pairs, one per set-up
	// track, in Setup order. Freshly allocated; never internal state.
	Channels []ChannelPair
}

// Client is a single RTSP session. Close, Wait, Stats and SessionInfo are safe
// from any goroutine; of those, only Close, Stats and SessionInfo may be
// called from inside OnFrame, because Wait blocks until the reader goroutine
// has finished and would deadlock it. The lifecycle calls (Describe, Setup,
// Play) are not safe for concurrent use and must be made in order from one
// goroutine.
type Client struct {
	cfg  Config
	conn net.Conn

	// writeMu serializes every socket write.
	writeMu sync.Mutex

	// pendMu guards pending, the CSeq rendezvous table.
	pendMu  sync.Mutex
	pending map[int]chan *Response
	cseq    atomic.Uint32

	// lifecycleMu serializes one lifecycle call end to end, across its network
	// round trip. Client documents that Describe, Setup and Play come from a
	// single goroutine, and mu alone cannot enforce that: each verb validates
	// under mu, releases it for the round trip, then re-acquires it to commit,
	// so two concurrent Setups would both pass the already-set-up check and
	// both propose the same channel pair, and the second would overwrite the
	// first's binding in the routing table with no error. Holding this for the
	// whole call makes the documented contract structural. Lock order is
	// lifecycleMu then mu, never the reverse.
	lifecycleMu sync.Mutex

	// mu guards the state machine and negotiated session fields below.
	mu              sync.Mutex
	state           state
	sessionID       string
	sessionTimeout  time.Duration
	keepaliveMethod string
	serverHeader    string
	baseURL         string
	channelPairs    []ChannelPair
	termErr         error
	// auth is the active authentication state. Once a 401 has been answered,
	// every outgoing request carries an Authorization header computed from it,
	// with the nonce count incremented per request under the same server nonce.
	// It is also what SessionInfo reports the scheme from, so the scheme is
	// stored once rather than kept in a second field that has to be updated in
	// step. The password and the header value are never logged.
	auth authState

	// username and password are the resolved credentials (URL userinfo wins
	// over Config) used to answer an authentication challenge. Set once by
	// newClient and never logged.
	username string
	password string
	// reporterSSRC is this client's RTCP reporter SSRC, a random 32-bit value
	// chosen at Dial and read by the keepalive timer when it builds Receiver
	// Reports. Immutable after newClient, so it needs no lock.
	reporterSSRC uint32

	// described retains Describe's per-track descriptors, indexed by track ID,
	// so Setup can build the pipeline without re-parsing the SDP. Written by
	// Describe under mu, read by Setup under mu.
	described []describedTrack
	// tracks holds the constructed per-track pipelines in Setup order, appended
	// by Setup under mu and read under mu by Stats, Play's RTP-Info seeding,
	// and the keepalive timer's Receiver Reports.
	tracks []*track

	// channels is the immutable channel-to-track routing table. Setup publishes
	// a new table by copy-on-write and an atomic store. It sits outside mu so
	// that the reader can load it lock-free on every interleaved frame, rather
	// than blocking on the lock the lifecycle calls hold.
	channels atomic.Pointer[channelTable]

	// lastFrameAt is the watchdog clock (UnixNano), written by the reader on
	// every frame and read when arming the read deadline, so it is atomic.
	// Play stamps it before it sets playing, because armReadDeadline derives
	// the deadline from it: flipping playing while this is still zero would
	// yield a 1970 deadline that has already expired and would kill a healthy
	// stream on its first read.
	lastFrameAt atomic.Int64
	// playing gates the read-idle watchdog; false until Play.
	playing atomic.Bool

	// deadlineMu serializes read-deadline arming against the shutdown
	// interrupt so the reader can never re-arm past a shutdown.
	deadlineMu   sync.Mutex
	shuttingDown bool

	closeOnce sync.Once
	closing   chan struct{}
	done      chan struct{}
	wg        sync.WaitGroup

	// Reader-owned accumulation buffer, parse offset, and read scratch. The
	// scratch is a field rather than a local in fill() because conn is an
	// interface, so escape analysis cannot see the callee and heap-allocates a
	// local array on every single socket read.
	rbuf     []byte
	start    int
	resync   int
	rscratch [readChunk]byte
}

// Dial connects to cfg.URL, starts the reader goroutine, and sends an OPTIONS
// probe to learn the keepalive method from the Public header. For rtsps it
// performs the TLS handshake first. It returns a Client in the idle state
// ready for Describe, or an error (with the connection torn down) on failure.
// A non-2xx OPTIONS response is tolerated (the Public header is simply
// unknown); a connection-level failure is fatal.
//
// A server may answer the probe and end the session in the same segment. The
// response is real, so Dial succeeds, but the returned Client is already
// closed and Describe will return a *StateError. Check Wait or SessionInfo
// before proceeding if that case matters to the caller.
//
//nolint:gocritic // Config is the documented public Dial signature; hugeParam does not apply to a per-session entry point.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	cfg.applyDefaults()
	tgt, err := parseTarget(&cfg)
	if err != nil {
		return nil, err
	}
	conn, err := dialConn(ctx, &cfg, &tgt)
	if err != nil {
		return nil, err
	}
	c := newClient(&cfg, conn, &tgt)
	go c.reader()
	if err := c.options(ctx, tgt.requestURL); err != nil {
		<-c.done // the funnel already fired; wait for the reader to finish.
		return nil, err
	}
	return c, nil
}

// newClient builds a Client around an established connection.
func newClient(cfg *Config, conn net.Conn, tgt *target) *Client {
	return &Client{
		cfg:          *cfg,
		conn:         conn,
		pending:      make(map[int]chan *Response),
		baseURL:      tgt.requestURL,
		username:     tgt.username,
		password:     tgt.password,
		reporterSSRC: randomSSRC(),
		closing:      make(chan struct{}),
		done:         make(chan struct{}),
		state:        stateIdle,
	}
}

// randomSSRC returns a random 32-bit RTCP reporter SSRC.
//
// crypto/rand.Read has been documented never to return an error since Go 1.24:
// it crashes the program irrecoverably if the system source fails. There is
// therefore no error branch to write, and any fallback constant here would be
// unreachable code pretending to be a degraded mode.
func randomSSRC() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:]) // cannot fail; see above.
	return binary.BigEndian.Uint32(b[:])
}

// dialConn performs the TCP connect and, for rtsps, the TLS handshake, both
// bounded by cfg.Timeout and ctx.
func dialConn(ctx context.Context, cfg *Config, tgt *target) (net.Conn, error) {
	d := net.Dialer{Timeout: cfg.Timeout}
	conn, err := d.DialContext(ctx, "tcp", tgt.address)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrConnectionClosed, err)
	}
	if !tgt.tls {
		return conn, nil
	}
	tconn := tls.Client(conn, tlsConfigFor(cfg, tgt))
	if err := tconn.SetDeadline(time.Now().Add(cfg.Timeout)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: %w", ErrConnectionClosed, err)
	}
	if err := tconn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: %w", ErrConnectionClosed, err)
	}
	if err := tconn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: %w", ErrConnectionClosed, err)
	}
	return tconn, nil
}

// tlsConfigFor returns the tls.Config for an rtsps dial: the caller's
// TLSConfig (cloned, with the server name filled in when empty), or a
// verified default keyed on the URL host.
func tlsConfigFor(cfg *Config, tgt *target) *tls.Config {
	if cfg.TLSConfig != nil {
		tc := cfg.TLSConfig.Clone()
		if tc.ServerName == "" {
			tc.ServerName = tgt.serverName
		}
		// The clone is already normalized (ServerName above), so leaving the
		// floor unset here would silently give a caller who passed only
		// RootCAs whatever minimum the toolchain currently defaults to.
		if tc.MinVersion == 0 {
			tc.MinVersion = tls.VersionTLS12
		}
		return tc
	}
	return &tls.Config{
		ServerName:         tgt.serverName,
		InsecureSkipVerify: cfg.InsecureTLS,
		MinVersion:         tls.VersionTLS12,
	}
}

// options sends the OPTIONS probe and records the keepalive method from the
// Public header. A non-2xx response is tolerated: the keepalive method
// defaults to OPTIONS. A connection-level failure is returned (already
// funneled into shutdown by roundTrip).
func (c *Client) options(ctx context.Context, reqURL string) error {
	resp, err := c.roundTrip(ctx, &Request{Method: methodOptions, URL: reqURL})
	if err != nil {
		return err
	}
	km := KeepaliveMethod(ParsePublic(resp.Header.Get("Public")))
	server := resp.Header.Get("Server")
	c.mu.Lock()
	c.keepaliveMethod = km
	// Record the Server header when the camera sent one. options runs once,
	// from Dial, so the non-empty guard is future-proofing: a later probe
	// reporting no Server must not blank a value already recorded.
	if server != "" {
		c.serverHeader = server
	}
	c.mu.Unlock()
	return nil
}

// initiateShutdown funnels every terminal trigger through one place, exactly
// once. The first cause wins. It signals the timer goroutine, records the
// shutdown intent under deadlineMu, and interrupts a blocked read or write
// with an immediate deadline on both directions.
func (c *Client) initiateShutdown(cause error) {
	c.closeOnce.Do(func() {
		c.setTermErr(cause)
		close(c.closing)
		c.deadlineMu.Lock()
		c.shuttingDown = true
		_ = c.conn.SetDeadline(time.Now())
		c.deadlineMu.Unlock()
	})
}

// setTermErr records the terminal cause (first writer wins) and moves the
// state to closed, both under mu.
func (c *Client) setTermErr(cause error) {
	c.mu.Lock()
	if c.termErr == nil {
		c.termErr = cause
	}
	c.state = stateClosed
	c.mu.Unlock()
}

// termError returns the recorded terminal cause, or nil if none yet.
func (c *Client) termError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.termErr
}

// Close ends the session. It is idempotent and safe from any goroutine,
// including from inside OnFrame. It signals the reader loop, which sends a
// best-effort TEARDOWN when a session was established and then closes the
// connection. Wait afterwards returns audiostream.ErrClosed, unless an earlier
// cause had already ended the session, since the first cause wins. Close
// returns nil.
func (c *Client) Close() error {
	c.initiateShutdown(audiostream.ErrClosed)
	return nil
}

// Wait blocks until the session ends and returns the terminal error. The
// common causes are audiostream.ErrClosed after Close, ctx.Err() if ctx
// cancels first, ErrServerTeardown on a server TEARDOWN,
// audiostream.ErrReadTimeout on watchdog expiry, ErrRequestTimeout when a
// request went unanswered, and ErrConnectionClosed (wrapping the cause) on
// connection loss. The list is not exhaustive: a framing or marshalling
// failure surfaces the wire layer's own error. After Wait returns, OnFrame
// will not be called again.
func (c *Client) Wait(ctx context.Context) error {
	select {
	case <-c.done:
		return c.termError()
	case <-ctx.Done():
		c.initiateShutdown(ctx.Err())
		<-c.done
		return c.termError()
	}
}

// Stats returns a snapshot of per-track receive counters in a freshly
// allocated map that never aliases internal state, keyed by track ID. It is
// safe from any goroutine, including from inside OnFrame: the reader is the
// only writer and the counters are atomic, so it takes mu only to read the
// track slice.
//
// The counters are read one at a time rather than under a single barrier, so a
// snapshot taken mid-packet can show a packet counted and its bytes not yet.
// They are cumulative diagnostics, never a ledger to reconcile.
func (c *Client) Stats() audiostream.Stats {
	c.mu.Lock()
	tracks := c.tracks
	c.mu.Unlock()
	m := make(map[int]audiostream.TrackStats, len(tracks))
	for _, tr := range tracks {
		m[tr.id] = audiostream.TrackStats{
			Packets:    tr.packets.Load(),
			Bytes:      tr.bytes.Load(),
			SeqGaps:    tr.seqGaps.Load(),
			Malformed:  tr.malformed.Load(),
			SSRCResets: tr.ssrcResets.Load(),
		}
	}
	return audiostream.Stats{Tracks: m}
}

// SessionInfo returns a snapshot of the negotiated session details known so
// far. It is safe from any goroutine, reads only mu-guarded fields (never
// credentials), and never aliases internal state.
func (c *Client) SessionInfo() SessionInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	var chans []ChannelPair
	if len(c.channelPairs) > 0 {
		chans = make([]ChannelPair, len(c.channelPairs))
		copy(chans, c.channelPairs)
	}
	return SessionInfo{
		SessionID:      c.sessionID,
		SessionTimeout: c.sessionTimeout,
		// The zero Challenge carries AuthNone, so an unauthenticated session
		// reports AuthNone without a separate "is auth active" test.
		AuthScheme:      c.auth.challenge.Scheme,
		KeepaliveMethod: c.keepaliveMethod,
		Server:          c.serverHeader,
		Channels:        chans,
	}
}

// requireState returns a *StateError (matching ErrInvalidState) when method is
// not legal in the current state, and nil when it is. The caller must hold mu.
//
// Validation and transition are two calls rather than one because every verb
// validates, releases mu for its network round trip, then re-acquires mu to
// commit: a combined advance would transition on the request rather than on
// the response, and a Describe that failed while parsing would leave a client
// that can never retry. Each verb pairs this with commitState on success.
func (c *Client) requireState(method string) error {
	if !legalIn(method, c.state) {
		return &StateError{Method: method, State: c.state.String()}
	}
	return nil
}

// commitState moves the client into the state method transitions into. The
// caller must hold mu, must have passed requireState, and must have confirmed
// no terminal error was recorded while mu was released. Close and fatal
// shutdown move to closed through setTermErr, never through here.
func (c *Client) commitState(method string) {
	dest, ok := destState(method)
	if !ok {
		// Unreachable while legalIn and destState enumerate the same methods,
		// which is the invariant destState's second return exists to keep.
		// Leaving the state untouched makes a future divergence a stuck
		// lifecycle (the next verb reports a *StateError naming the state it
		// was stuck in) rather than a silent jump to idle.
		return
	}
	c.state = dest
}

// writeMessage serializes one socket write under writeMu with a Timeout-bound
// write deadline.
func (c *Client) writeMessage(raw []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.armWriteDeadline(c.cfg.Timeout); err != nil {
		return err
	}
	_, err := c.conn.Write(raw)
	return err
}

// armWriteDeadline sets the socket write deadline d from now, under deadlineMu
// so it can never race past a shutdown. When shutdown has begun it sets an
// immediate deadline instead.
//
// initiateShutdown's SetDeadline(now) does interrupt a write already blocked in
// the syscall. What it could not stop was a write that reached its own deadline
// call just AFTER the interrupt and re-armed the write side forward to
// now+Timeout, undoing it. Close could then leave the reader parked in Write for
// the full 10s default while sendTeardownBestEffort queued behind it on writeMu
// and Wait blocked for the same window.
func (c *Client) armWriteDeadline(d time.Duration) error {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	deadline := time.Now().Add(d)
	if c.shuttingDown {
		deadline = time.Now()
	}
	return c.conn.SetWriteDeadline(deadline)
}

// armTeardownDeadlines bounds the best-effort TEARDOWN write and the discard
// read that follows it. Both go through deadlineMu for the same reason
// armWriteDeadline does, but unlike armWriteDeadline neither collapses to an
// immediate deadline during shutdown.
//
// That exemption is the whole point. sendTeardownBestEffort runs only from the
// terminal sequence, which runs only after initiateShutdown, so shuttingDown is
// ALWAYS set by then. Routing its write through armWriteDeadline therefore
// armed an already-expired deadline and the poller refused the write outright,
// so the TEARDOWN was never transmitted and the discarded error hid it. The
// server would keep the session allocated until its own timeout, and on a
// camera with a one-session limit the next Dial is refused.
func (c *Client) armTeardownDeadlines() {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	deadline := time.Now().Add(teardownDeadline)
	_ = c.conn.SetWriteDeadline(deadline)
	_ = c.conn.SetReadDeadline(deadline)
}
