package httpsource

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
)

// readBufSize is the reader's per-read buffer and the buffered-body size. The
// widest sample-frame this source accepts (2*maxChannels bytes) divides it, so
// a single read never straddles a frame by more than a partial one, and the
// per-read byte-swap scratch of the same size always fits a full delivery.
const readBufSize = 4096

// maxPTSSeconds is the largest whole-second count a time.Duration holds. A
// duration is an int64 nanosecond count, so the seconds term of a PTS must stay
// below this or the multiply that scales it wraps negative. It mirrors the rtsp
// pipeline's overflow-safe PTS math.
const maxPTSSeconds = math.MaxInt64 / int64(time.Second)

// Client is a single HTTP progressive audio source. It opens one GET, resolves
// the audio format from the response, and delivers s16le PCM frames to
// Config.OnFrame on its reader goroutine until the stream ends, Close is
// called, or the read-idle watchdog fires.
//
// Close, Wait, Stats, Info and Codec are safe from any goroutine; of those, all
// but Wait may be called from inside OnFrame, because Wait blocks until the
// reader goroutine has finished and would deadlock it.
type Client struct {
	cfg   Config
	httpc *http.Client

	// url is the credential-stripped request target, backing Info().URL. server
	// is the response Server header. Both are set in Open before the reader
	// goroutine spawns and never mutated after, so they need no lock.
	url    string
	server string

	// Resolved audio geometry, set by resolveFormat during Open before the
	// reader spawns and immutable after: no lock needed. swap is true when the
	// source is big-endian and each sample must be swapped to little-endian
	// s16le on delivery. frameBytes is 2*channels.
	rate       int
	channels   int
	frameBytes int
	swap       bool

	// Data budget, set during Open and owned by the reader afterward. When
	// bounded, remaining counts down the declared WAV data-chunk bytes and the
	// stream ends when it reaches zero; an unbounded source streams until EOF.
	bounded   bool
	remaining int64

	// cancel aborts the streaming request. reqCtx is divorced from the caller's
	// context (context.WithoutCancel), so nothing else ever cancels it: storing
	// cancel here and calling it from initiateShutdown is the only way to
	// unblock a parked Body.Read on Close or a watchdog timeout.
	cancel context.CancelFunc

	// body and br are the response body and its buffered reader, owned by the
	// reader goroutine (and closed by its terminal sequence).
	body io.ReadCloser
	br   *bufio.Reader

	// lastReadAt is the watchdog clock (UnixNano): the reader stamps it on every
	// read and the watchdog reads it to derive its deadline. Open stamps it once
	// before spawning the watchdog so the first timer tick does not evaluate a
	// 1970 deadline and kill a healthy stream.
	lastReadAt atomic.Int64

	closeOnce sync.Once
	closing   chan struct{}
	done      chan struct{}
	// wg joins the watchdog goroutine, so Wait never returns while it is still
	// running.
	wg sync.WaitGroup

	// mu guards termErr.
	mu      sync.Mutex
	termErr error

	// Reader-owned PTS counter, in delivered sample-frames.
	samples uint64

	// Per-source receive counters: the reader writes, Stats reads.
	packets   atomic.Uint64
	payload   atomic.Uint64
	malformed atomic.Uint64

	// Reader-owned buffers, fixed arrays on the struct so the hot read/deliver
	// path allocates nothing. rbuf accumulates reads across a partial trailing
	// sample-frame; swapBuf holds the byte-swapped image of a big-endian frame.
	rbuf    [readBufSize]byte
	swapBuf [readBufSize]byte
}

// Client satisfies the root package's source-agnostic capture contract.
var _ audiostream.Source = (*Client)(nil)

// Open performs the whole HTTP handshake and returns an already-delivering
// source (os.Open semantics): it issues the GET, checks the status, resolves
// the audio format (parsing the WAV header when present), and spawns the reader
// before returning. From then on OnFrame is called on the reader goroutine.
//
// ctx bounds only the open phase. It is deliberately divorced from the
// streaming request (context.WithoutCancel), so cancelling ctx after Open
// returns does not end the stream; use Close for that. Config.Timeout bounds
// the open phase independently and classifies to ErrRequestTimeout.
//
// On failure it returns, with the connection torn down, one of: ErrInvalidURL
// (bad Config.URL), ctx.Err() (ctx cancelled during open), ErrRequestTimeout
// (open timed out), ErrConnectionClosed (other transport failure),
// *audiostream.RedirectError (a 3xx, matching audiostream.ErrRedirect),
// *StatusError (any other non-200, matching ErrBadStatus), ErrUnsupportedFormat
// (a media type or WAV shape this source will not decode), ErrFormatUnknown
// (raw audio whose rate and channels could not be resolved), or ErrMalformedWAV
// (a broken WAV header).
//
//nolint:gocyclo,gocritic // The open handshake is a linear sequence of guarded steps; splitting it would scatter the shared teardown defer. Config is the documented public constructor signature, so hugeParam does not apply to a per-source entry point.
func Open(ctx context.Context, cfg Config) (*Client, error) {
	cfg.applyDefaults()
	tgt, err := parseTarget(&cfg)
	if err != nil {
		return nil, err
	}
	// An already-cancelled caller context fails deterministically, without
	// depending on the AfterFunc winning a race against the dial below.
	if cerr := ctx.Err(); cerr != nil {
		return nil, cerr
	}
	if aerr := checkInsecureAuth(&cfg, &tgt); aerr != nil {
		return nil, aerr
	}

	c := &Client{
		cfg:     cfg,
		url:     tgt.requestURL,
		closing: make(chan struct{}),
		done:    make(chan struct{}),
	}
	c.httpc = newHTTPClient(&cfg, &tgt)

	// reqCtx carries the caller's values but not its cancellation, so the
	// streaming request outlives ctx. cancel is stored for initiateShutdown.
	reqCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c.cancel = cancel

	// cancelOpen aborts the request during the open phase but becomes a no-op
	// once the open has finished and the reader owns the stream. Both the
	// caller-context bridge and the open timeout drive it, so a late timeout tick
	// racing the deferred timer.Stop cannot cancel a healthy, already-published
	// stream (whose first Body.Read would otherwise fail context.Canceled and
	// surface as a spurious ErrConnectionClosed).
	var openMu sync.Mutex
	openFinished := false
	cancelOpen := func() {
		openMu.Lock()
		defer openMu.Unlock()
		if openFinished {
			return
		}
		cancel()
	}

	// Tear down the half-open request on any failure path, and cancel on all of
	// them. On success ok flips and the reader owns the body instead.
	ok := false
	defer func() {
		if !ok {
			cancel()
			if c.body != nil {
				_ = c.body.Close()
			}
		}
	}()

	// Bridge a caller-context cancellation onto the request for the open phase
	// only; stop removes the hook when Open returns, so ctx never touches the
	// stream afterward. Deferred on every exit path per the leak fix.
	stop := context.AfterFunc(ctx, cancelOpen)
	defer stop()

	// A separate open-phase deadline, classified to ErrRequestTimeout. Deferred
	// Stop on every exit path so a failed Open never leaves the timer (and its
	// pinned cancel closure) armed.
	var timedOut atomic.Bool
	timer := time.AfterFunc(cfg.Timeout, func() {
		timedOut.Store(true)
		cancelOpen()
	})
	defer timer.Stop()

	req, err := buildRequest(reqCtx, &cfg, &tgt)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpc.Do(req) //nolint:bodyclose // On success the reader owns c.body and closes it in teardown; on failure the defer above closes it.
	if err != nil {
		return nil, classifyOpenErr(ctx, err, &timedOut)
	}
	c.body = resp.Body
	c.server = resp.Header.Get("Server")

	if serr := responseStatusError(resp); serr != nil {
		return nil, serr
	}
	c.br = bufio.NewReaderSize(resp.Body, readBufSize)
	if ferr := c.resolveFormat(resp); ferr != nil {
		return nil, ferr
	}

	// The open has succeeded; neutralize the open-phase cancels before publishing
	// the stream, so a late timeout tick cannot abort a healthy reader.
	openMu.Lock()
	openFinished = true
	openMu.Unlock()

	// Stamp the watchdog clock before spawning the watchdog, so its first tick
	// measures from now rather than from the zero time.
	c.lastReadAt.Store(time.Now().UnixNano())
	if cfg.ReadIdle > 0 {
		c.wg.Add(1)
		go c.watchdog()
	}
	go c.reader()
	ok = true
	return c, nil
}

// newHTTPClient builds the private per-source HTTP client. It never follows
// redirects (CheckRedirect returns http.ErrUseLastResponse, so Do hands back
// the 3xx response for Open to surface as a *audiostream.RedirectError),
// disables transparent compression and keep-alives (a source is one request),
// and bounds the connect, TLS handshake, and response-header waits by
// Config.Timeout.
func newHTTPClient(cfg *Config, tgt *target) *http.Client {
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: cfg.Timeout}).DialContext,
		TLSClientConfig:       tlsConfigFor(cfg, tgt),
		TLSHandshakeTimeout:   cfg.Timeout,
		ResponseHeaderTimeout: cfg.Timeout,
		DisableCompression:    true,
		DisableKeepAlives:     true,
	}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// tlsConfigFor returns the tls.Config for an https request: the caller's
// TLSConfig (cloned, with the server name filled in when empty and the minimum
// version raised to TLS 1.2 when unset), or a verified default keyed on the URL
// host. It mirrors the rtsp client's rule so both sources treat TLS the same.
func tlsConfigFor(cfg *Config, tgt *target) *tls.Config {
	if cfg.TLSConfig != nil {
		tc := cfg.TLSConfig.Clone()
		if tc.ServerName == "" {
			tc.ServerName = tgt.serverName
		}
		if tc.MinVersion == 0 {
			tc.MinVersion = tls.VersionTLS12
		}
		return tc
	}
	return &tls.Config{
		ServerName:         tgt.serverName,
		InsecureSkipVerify: cfg.InsecureTLS, //nolint:gosec // Opt-in via Config.InsecureTLS for self-signed endpoints, mirroring the rtsp client.
		MinVersion:         tls.VersionTLS12,
	}
}

// checkInsecureAuth enforces the secure-by-default credential policy: Basic
// credentials over a plaintext http connection are refused with ErrInsecureAuth
// unless the caller opted in with Config.AllowInsecureAuth, in which case a
// warning is logged (the URL is already credential-stripped). Credentials over
// https, and requests without credentials, are always allowed.
func checkInsecureAuth(cfg *Config, tgt *target) error {
	if tgt.tls || (tgt.username == "" && tgt.password == "") {
		return nil
	}
	if !cfg.AllowInsecureAuth {
		return ErrInsecureAuth
	}
	if cfg.Logger != nil {
		cfg.Logger.Warn("httpsource: sending Basic credentials over a plaintext http connection", "url", tgt.requestURL)
	}
	return nil
}

// buildRequest constructs the GET, sets the User-Agent, and attaches Basic auth
// when a credential was resolved (URL userinfo or Config).
func buildRequest(ctx context.Context, cfg *Config, tgt *target) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tgt.requestURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidURL, err)
	}
	req.Header.Set("User-Agent", cfg.UserAgent)
	if tgt.username != "" || tgt.password != "" {
		req.SetBasicAuth(tgt.username, tgt.password)
	}
	return req, nil
}

// responseStatusError maps the response status to the error Open should return,
// or nil for 200. A 3xx is a *audiostream.RedirectError (Do surfaced it because
// CheckRedirect refuses to follow); any other non-200 is a *StatusError.
func responseStatusError(resp *http.Response) error {
	switch {
	case resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return &audiostream.RedirectError{Location: resp.Header.Get("Location")}
	default:
		return &StatusError{Code: resp.StatusCode, Status: resp.Status}
	}
}

// classifyOpenErr maps a transport error from the open GET to a terminal cause:
// ErrRequestTimeout when the open-phase deadline fired or the transport
// reported a timeout, the caller's ctx.Err() when ctx cancelled the open, and
// ErrConnectionClosed (wrapping the cause) otherwise.
func classifyOpenErr(ctx context.Context, err error, timedOut *atomic.Bool) error {
	if timedOut.Load() {
		return fmt.Errorf("%w: %w", ErrRequestTimeout, err)
	}
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return fmt.Errorf("%w: %w", ErrRequestTimeout, err)
	}
	return fmt.Errorf("%w: %w", ErrConnectionClosed, err)
}

// Codec returns the source's codec identity: 16-bit linear PCM at the resolved
// clock rate and channel count. Frames are always delivered as little-endian
// s16le regardless of the source byte order. It is immutable after Open and
// safe from any goroutine, including from inside OnFrame.
func (c *Client) Codec() audiostream.CodecL16 {
	return audiostream.CodecL16{ClockRate: c.rate, Channels: c.channels}
}

// Wait blocks until the stream ends and returns the terminal cause: ErrStreamEnded
// on an orderly end, audiostream.ErrClosed after Close, ctx.Err() if ctx
// cancels first, audiostream.ErrReadTimeout on watchdog expiry, or
// ErrConnectionClosed (wrapping the cause) on connection loss. The first cause
// wins. After Wait returns, OnFrame will not be called again. Do not call Wait
// from inside OnFrame; it deadlocks.
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

// Close ends the stream. It is idempotent and safe from any goroutine,
// including from inside OnFrame. It cancels the streaming request, which
// unblocks the reader; Wait afterwards returns audiostream.ErrClosed unless an
// earlier cause had already ended the stream. Close returns nil.
func (c *Client) Close() error {
	c.initiateShutdown(audiostream.ErrClosed)
	return nil
}

// Stats returns a snapshot of the source's receive counters in a freshly
// allocated map keyed by the single track ID 0. PayloadBytes is the PCM
// delivered and Malformed the count of dropped partial sample-frames;
// LastFrameAt is the last read time and Stats.CapturedAt the snapshot time.
// WireBytes stays zero, since this source does not meter transport framing
// apart from the payload, as do the other counters it has no equivalent for.
// Safe from any goroutine, including from inside OnFrame.
func (c *Client) Stats() audiostream.Stats {
	ts := audiostream.TrackStats{
		Packets:      c.packets.Load(),
		PayloadBytes: c.payload.Load(),
		Malformed:    c.malformed.Load(),
	}
	// lastReadAt is stamped at Open and on every read, so it is non-zero for the
	// life of the source and maps directly to the media-liveness clock.
	if nanos := c.lastReadAt.Load(); nanos != 0 {
		ts.LastFrameAt = time.Unix(0, nanos)
	}
	// Stamp CapturedAt after loading every counter, so a read landing mid-snapshot
	// cannot make LastFrameAt outrank it and yield a negative age when a consumer
	// computes CapturedAt.Sub(LastFrameAt). It also carries a monotonic reading
	// for snapshot-to-snapshot elapsed time.
	capturedAt := time.Now()
	return audiostream.Stats{
		CapturedAt: capturedAt,
		Tracks:     map[int]audiostream.TrackStats{0: ts},
	}
}

// Info returns the source-neutral identity snapshot: the request URL with
// credentials stripped, and the response Server header ("" when the server sent
// none). Safe from any goroutine, including from inside OnFrame.
func (c *Client) Info() audiostream.SourceInfo {
	return audiostream.SourceInfo{URL: c.url, Server: c.server}
}

// initiateShutdown funnels every terminal trigger through one place, exactly
// once; the first cause wins. It records the cause, signals the watchdog and
// reader by closing closing, and cancels the streaming request to unblock a
// parked Body.Read.
func (c *Client) initiateShutdown(cause error) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		if c.termErr == nil {
			c.termErr = cause
		}
		c.mu.Unlock()
		close(c.closing)
		if c.cancel != nil {
			c.cancel()
		}
	})
}

// termError returns the recorded terminal cause, or nil if none yet.
func (c *Client) termError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.termErr
}

// reader is the single reader goroutine. It owns every read of the response
// body for the source's life. A deferred recover funnels a panic raised inside
// the loop (an OnFrame callback, above all) into shutdown, so such a panic
// becomes a clean stream end rather than a process crash. When the loop ends it
// performs the terminal teardown and, as its final act, closes done.
func (c *Client) reader() {
	defer close(c.done)
	defer c.teardown()
	defer c.recoverReader()
	c.readLoop()
}

// recoverReader turns a panic in the reader into a fatal shutdown cause.
func (c *Client) recoverReader() {
	if r := recover(); r != nil {
		c.initiateShutdown(fmt.Errorf("httpsource: reader panic: %v", r))
	}
}

// readLoop accumulates body bytes into rbuf and delivers each whole sample-frame
// prefix, carrying the sub-frame remainder across reads so a frame split by a
// read boundary is never delivered half. It runs until a terminal condition,
// whose shutdown it funnels before returning.
func (c *Client) readLoop() {
	fill := 0
	for {
		select {
		case <-c.closing:
			return
		default:
		}
		if c.bounded && c.remaining == 0 {
			// The declared data chunk is fully consumed. Any sub-frame tail is a
			// truncated final frame; count it and drop it, as deliverL16 does.
			c.dropPartial(fill)
			c.initiateShutdown(ErrStreamEnded)
			return
		}
		limit := len(c.rbuf) - fill
		if c.bounded && int64(limit) > c.remaining {
			limit = int(c.remaining)
		}
		n, err := c.br.Read(c.rbuf[fill : fill+limit])
		if n > 0 {
			fill = c.absorb(fill, n)
		}
		if err != nil {
			cause := c.classifyReadErr(err)
			// An orderly end with an undelivered sub-frame tail drops and counts
			// it. A connection loss or a local Close does not: the tail is lost
			// data, not a malformed frame.
			if errors.Is(cause, ErrStreamEnded) {
				c.dropPartial(fill)
			}
			c.initiateShutdown(cause)
			return
		}
	}
}

// absorb stamps the watchdog clock, charges the read against a bounded budget,
// delivers the whole-frame prefix of the accumulated bytes, compacts the
// sub-frame remainder to the front of rbuf, and returns the new fill.
func (c *Client) absorb(fill, n int) int {
	now := time.Now()
	c.lastReadAt.Store(now.UnixNano())
	if c.bounded {
		c.remaining -= int64(n)
	}
	usable := fill + n
	deliver := usable - usable%c.frameBytes
	if deliver > 0 {
		c.deliverFrame(c.rbuf[:deliver], now)
	}
	rem := usable - deliver
	if rem > 0 {
		copy(c.rbuf[:rem], c.rbuf[deliver:usable])
	}
	return rem
}

// deliverFrame counts one PCM delivery and, when OnFrame is set, hands it over.
// A little-endian source (WAV or raw LE) is delivered zero-copy; a big-endian
// source is byte-swapped into swapBuf first. The frame's PTS is the presentation
// time of its first sample, computed before this delivery advances the sample
// counter, so the first frame is at PTS 0 and successive PTSs are strictly
// increasing. pcm is a whole number of sample-frames.
func (c *Client) deliverFrame(pcm []byte, now time.Time) {
	c.packets.Add(1)
	c.payload.Add(uint64(len(pcm)))
	if c.cfg.OnFrame == nil {
		return
	}
	out := pcm
	if c.swap {
		out = c.byteSwap(pcm)
	}
	pts := c.ptsOf()
	c.cfg.OnFrame(audiostream.Frame{
		TrackID:    0,
		Data:       out,
		RTPTime:    0,
		PTS:        pts,
		ReceivedAt: now,
		SeqGap:     0,
	})
	c.samples += uint64(len(pcm) / c.frameBytes)
}

// byteSwap writes the little-endian image of a big-endian s16 buffer into
// swapBuf and returns the written prefix. The manual pairwise loop is the same
// shape the rtsp L16 path uses: a benchmark there found encoding/binary about
// 40% slower on this per-frame path, and the manual form is equally
// alloc-free. len(pcm) is even (a whole number of 2-byte samples) and never
// exceeds len(swapBuf).
func (c *Client) byteSwap(pcm []byte) []byte {
	n := len(pcm)
	dst := c.swapBuf[:n]
	for i := 0; i+1 < n; i += 2 {
		dst[i] = pcm[i+1]
		dst[i+1] = pcm[i]
	}
	return dst
}

// ptsOf computes the presentation time of the next sample to be delivered, from
// the running sample-frame count and the clock rate. The division is split into
// whole seconds and a remainder so the nanosecond scaling cannot overflow a
// uint64 on a long stream, and the seconds term is clamped to what a
// time.Duration can express, mirroring the rtsp pipeline's overflow-safe math.
func (c *Client) ptsOf() time.Duration {
	rate := uint64(c.rate)
	if rate == 0 {
		return 0
	}
	s := c.samples
	sec := s / rate
	if sec >= uint64(maxPTSSeconds) {
		return time.Duration(maxPTSSeconds) * time.Second
	}
	frac := (s % rate) * uint64(time.Second) / rate
	return time.Duration(sec)*time.Second + time.Duration(frac)
}

// dropPartial counts an undelivered sub-frame tail as malformed, mirroring the
// rtsp L16 whole-frame rule so a consumer concatenating frames never sees a
// half-frame that shifts channel interleaving.
func (c *Client) dropPartial(fill int) {
	if fill > 0 {
		c.malformed.Add(1)
	}
}

// classifyReadErr maps a body read error to the terminal cause: the recorded
// cause when shutdown is already in progress (a Close or watchdog interrupt we
// induced), ErrStreamEnded on a clean EOF, and ErrConnectionClosed (wrapping the
// cause) otherwise. It checks the shutdown state first, so the cancellation this
// source itself triggered surfaces as the first recorded cause rather than as a
// spurious connection error.
func (c *Client) classifyReadErr(err error) error {
	select {
	case <-c.closing:
		if te := c.termError(); te != nil {
			return te
		}
	default:
	}
	if errors.Is(err, io.EOF) {
		return ErrStreamEnded
	}
	return fmt.Errorf("%w: %w", ErrConnectionClosed, err)
}

// teardown is the reader's terminal sequence: close the body (unblocking
// nothing, since the reader is the only reader, but releasing the connection)
// and join the watchdog goroutine. It runs after the loop has funneled its
// cause, so closing is already closed and the watchdog will observe it.
func (c *Client) teardown() {
	if c.body != nil {
		_ = c.body.Close()
	}
	c.wg.Wait()
}

// watchdog ends the stream with audiostream.ErrReadTimeout when no body byte
// arrives within Config.ReadIdle. It arms a timer for the window; on each fire
// it recomputes the remaining time from the last read and re-arms if the stream
// spoke since the timer was set, else it funnels the timeout. It re-arms only
// from its own goroutine after draining the timer channel, so there is no
// Timer.Reset race and a healthy stream is never spuriously killed. It exits on
// closing.
func (c *Client) watchdog() {
	defer c.wg.Done()
	timer := time.NewTimer(c.cfg.ReadIdle)
	defer timer.Stop()
	for {
		select {
		case <-c.closing:
			return
		case <-timer.C:
			deadline := time.Unix(0, c.lastReadAt.Load()).Add(c.cfg.ReadIdle)
			if rem := time.Until(deadline); rem > 0 {
				timer.Reset(rem)
				continue
			}
			c.initiateShutdown(audiostream.ErrReadTimeout)
			return
		}
	}
}
