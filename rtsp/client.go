package rtsp

import (
	"context"
	"crypto/tls"
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
	// The M4a per-unit caps make it unreachable for well-formed units.
	maxReadBuffer = MaxHeaderBytes + MaxBodySize
	// maxResyncBytes bounds byte-at-a-time resynchronization before the
	// reader declares a fatal framing error.
	maxResyncBytes = 4096
)

// Exported errors, in addition to the reused audiostream and M4a errors.
var (
	// ErrServerTeardown ends Wait when the server sent a TEARDOWN.
	ErrServerTeardown = errors.New("rtsp: server sent TEARDOWN")
	// ErrConnectionClosed wraps the underlying cause when the control
	// connection was lost unexpectedly.
	ErrConnectionClosed = errors.New("rtsp: connection closed")
	// ErrAuthFailed ends a request when authentication was rejected after the
	// permitted retries.
	ErrAuthFailed = errors.New("rtsp: authentication failed")
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
// diagnostics such as the M5 stream-doctor handshake walkthrough. It carries
// no credentials and no counters.
type SessionInfo struct {
	// SessionID is the negotiated RTSP session identifier, "" before Setup.
	SessionID string
	// SessionTimeout is the advertised (or defaulted) session timeout.
	SessionTimeout time.Duration
	// AuthScheme names the authentication scheme in use ("none", "Basic", or
	// "Digest").
	AuthScheme string
	// KeepaliveMethod is the negotiated keepalive method ("OPTIONS" or
	// "GET_PARAMETER"), "" before Dial's OPTIONS completes.
	KeepaliveMethod string
	// Channels lists the assigned interleaved channel pairs, one per set-up
	// track, in Setup order. Freshly allocated; never internal state.
	Channels []ChannelPair
}

// Client is a single RTSP session. Methods other than Close, Wait, and Stats
// are not safe for concurrent use and must be called in lifecycle order
// (Describe, Setup, Play) from one goroutine. Close, Wait, and Stats are safe
// from any goroutine.
type Client struct {
	cfg  Config
	conn net.Conn

	// writeMu serializes every socket write.
	writeMu sync.Mutex

	// pendMu guards pending, the CSeq rendezvous table.
	pendMu  sync.Mutex
	pending map[int]chan *Response
	cseq    atomic.Uint32

	// mu guards the state machine and negotiated session fields below.
	mu              sync.Mutex
	state           state
	sessionID       string
	sessionTimeout  time.Duration
	keepaliveMethod string
	authScheme      string
	baseURL         string
	channelPairs    []ChannelPair
	termErr         error

	// lastFrameAt is the watchdog clock (UnixNano). It is written by Play on
	// the caller goroutine and by the reader on every frame, so it is atomic.
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

	// Reader-owned accumulation buffer and parse offset.
	rbuf   []byte
	start  int
	resync int
}

// Dial connects to cfg.URL, starts the reader goroutine, and sends an OPTIONS
// probe to learn the keepalive method from the Public header. For rtsps it
// performs the TLS handshake first. It returns a Client in the idle state
// ready for Describe, or an error (with the connection torn down) on failure.
// A non-2xx OPTIONS response is tolerated (the Public header is simply
// unknown); a connection-level failure is fatal.
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
		cfg:     *cfg,
		conn:    conn,
		pending: make(map[int]chan *Response),
		baseURL: tgt.requestURL,
		closing: make(chan struct{}),
		done:    make(chan struct{}),
		state:   stateIdle,
	}
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
	c.mu.Lock()
	c.keepaliveMethod = km
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
// best-effort TEARDOWN and closes the connection; Wait then returns
// ErrClosed. Close returns nil.
func (c *Client) Close() error {
	c.initiateShutdown(audiostream.ErrClosed)
	return nil
}

// Wait blocks until the session ends and returns the terminal error:
// ErrClosed after Close, ctx.Err() if ctx cancels first, ErrServerTeardown on
// a server TEARDOWN, ErrReadTimeout on watchdog expiry, or ErrConnectionClosed
// (wrapping the cause) on connection loss. After Wait returns, OnFrame will
// not be called again.
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

// Stats returns a deep-copied snapshot of per-track receive counters. The
// returned map is freshly allocated and never aliases internal state. In this
// milestone no track is set up, so the map is empty.
func (c *Client) Stats() audiostream.Stats {
	return audiostream.Stats{Tracks: make(map[int]audiostream.TrackStats)}
}

// SessionInfo returns a snapshot of the negotiated session details known so
// far. It is safe from any goroutine, reads only mu-guarded fields (never
// credentials), and never aliases internal state.
func (c *Client) SessionInfo() SessionInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	scheme := c.authScheme
	if scheme == "" {
		scheme = "none"
	}
	var chans []ChannelPair
	if len(c.channelPairs) > 0 {
		chans = make([]ChannelPair, len(c.channelPairs))
		copy(chans, c.channelPairs)
	}
	return SessionInfo{
		SessionID:       c.sessionID,
		SessionTimeout:  c.sessionTimeout,
		AuthScheme:      scheme,
		KeepaliveMethod: c.keepaliveMethod,
		Channels:        chans,
	}
}

// advance validates that method is legal in the current state and, when it
// is, transitions to the destination state. It returns a *StateError
// (matching ErrInvalidState) without changing state when the call is illegal.
// Close and fatal shutdown move to closed through setTermErr, not advance.
func (c *Client) advance(method string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !legalIn(method, c.state) {
		return &StateError{Method: method, State: c.state.String()}
	}
	c.state = destState(method)
	return nil
}

// writeMessage serializes one socket write under writeMu with a Timeout-bound
// write deadline.
func (c *Client) writeMessage(raw []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.conn.SetWriteDeadline(time.Now().Add(c.cfg.Timeout)); err != nil {
		return err
	}
	_, err := c.conn.Write(raw)
	return err
}
