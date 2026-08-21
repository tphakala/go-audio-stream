package hlssource

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
)

// Client is a single HLS audio source. Open fetches the playlist, resolves a
// master to a media playlist, downloads and demuxes segments in media-sequence
// order, and delivers AAC access units to Config.OnFrame on its reader goroutine
// until a VOD playlist ends, Close is called, or the read-idle watchdog fires. A
// live playlist is reloaded on the RFC 8216 cadence.
//
// Close, Wait, Stats, Info and Format are safe from any goroutine; of those, all
// but Wait may be called from inside OnFrame, because Wait blocks until the
// reader goroutine has finished and would deadlock it.
type Client struct {
	cfg   Config
	httpc *http.Client
	tgt   target

	// url is the credential-stripped playlist target, backing Info().URL.
	url string

	// codec is the resolved AAC codec (with the AudioSpecificConfig), set in Open
	// before the reader spawns and immutable after, so Format needs no lock.
	codec audiostream.Codec

	// Reader-owned playback state, set up in Open and owned by the reader
	// goroutine afterward. demux is the long-lived TS demuxer; media is the
	// current media playlist; mediaURL is its absolute URL for live reloads;
	// pending holds the first segment's access units, demuxed during Open to
	// resolve the codec and delivered first by the reader. lastSeq is the highest
	// media sequence number delivered; mediaPTS is the running presentation time;
	// pendingSeqGap accumulates segments lost to a gap or a window drop, attached
	// to the next delivered frame.
	demux         *tsDemux
	media         *mediaPlaylist
	mediaURL      *url.URL
	pending       []pendingAU
	lastSeq       uint64
	mediaPTS      time.Duration
	pendingSeqGap int

	// baseCtx is the base context for every streaming request, divorced from the
	// caller's Open context (context.WithoutCancel); cancel aborts it. Only Close
	// or the watchdog, via initiateShutdown, ends the stream after Open returns.
	baseCtx context.Context
	cancel  context.CancelFunc

	// lastReadAt is the watchdog clock (UnixNano): the reader stamps it on every
	// successful playlist or segment body read and the watchdog derives its
	// deadline from it.
	lastReadAt atomic.Int64

	closeOnce sync.Once
	closing   chan struct{}
	done      chan struct{}
	wg        sync.WaitGroup

	mu      sync.Mutex
	termErr error

	// Per-source receive counters: the reader writes, Stats reads.
	packets   atomic.Uint64
	payload   atomic.Uint64
	malformed atomic.Uint64
}

// schemeHTTPS is the URL scheme that permits sending Basic credentials on the
// wire, and marks an https request for per-host TLS.
const schemeHTTPS = "https"

// pendingAU is one access unit demuxed during Open, held until the reader
// delivers it. Data is a private copy, since the framer buffer it aliased is
// reused as the reader continues.
type pendingAU struct {
	data []byte
	dur  time.Duration
}

// Client satisfies the root package's source-agnostic capture contract.
var _ audiostream.Source = (*Client)(nil)

// Open performs the whole HLS handshake and returns an already-delivering
// source: it fetches the playlist, resolves a master playlist to a media
// playlist, downloads the first segment and demuxes it to resolve the AAC
// AudioSpecificConfig, then spawns the reader. From then on OnFrame is called on
// the reader goroutine.
//
// ctx bounds only the open phase; it is divorced from the streaming requests, so
// cancelling it after Open returns does not end the stream (use Close). Each
// request is bounded by Config.Timeout.
//
//nolint:gocritic // Config is the documented public constructor signature, so hugeParam does not apply to this per-source entry point.
func Open(ctx context.Context, cfg Config) (*Client, error) {
	cfg.applyDefaults()
	tgt, err := parseTarget(&cfg)
	if err != nil {
		return nil, err
	}
	if cerr := ctx.Err(); cerr != nil {
		return nil, cerr
	}

	c := &Client{
		cfg:     cfg,
		tgt:     tgt,
		url:     tgt.requestURL,
		closing: make(chan struct{}),
		done:    make(chan struct{}),
	}
	c.httpc = newHTTPClient(&cfg, &tgt)
	reqCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c.baseCtx = reqCtx
	c.cancel = cancel

	ok := false
	defer func() {
		if !ok {
			cancel()
		}
	}()

	if err := c.openHandshake(ctx); err != nil {
		return nil, err
	}

	c.lastReadAt.Store(time.Now().UnixNano())
	if cfg.ReadIdle > 0 {
		c.wg.Add(1)
		go c.watchdog()
	}
	go c.reader()
	ok = true
	return c, nil
}

// openHandshake fetches the playlist (resolving a master to a media playlist),
// downloads the first playable segment, and demuxes it to resolve the codec,
// leaving the reader everything it needs to continue. It uses ctx so a caller
// cancellation or the per-request Timeout aborts the open.
func (c *Client) openHandshake(ctx context.Context) error {
	media, mediaURL, err := c.fetchMediaPlaylist(ctx, c.tgt.requestURL)
	if err != nil {
		return err
	}
	seg, ok := firstPlayable(media)
	if !ok {
		return fmt.Errorf("%w: playlist has no playable segment", ErrMalformedPlaylist)
	}
	body, err := c.get(ctx, seg.uri, c.cfg.MaxSegmentBytes, ErrSegmentTooLarge)
	if err != nil {
		return err
	}
	d := newTSDemux()
	if derr := d.demux(body, false, func(au []byte, dur time.Duration) {
		c.pending = append(c.pending, pendingAU{data: append([]byte(nil), au...), dur: dur})
	}); derr != nil {
		return derr
	}
	asc := d.audioSpecificConfig()
	if asc == nil {
		return fmt.Errorf("%w: first segment carried no AAC access unit", ErrMalformedSegment)
	}
	c.codec = audiostream.CodecAAC{AudioSpecificConfig: asc}
	c.demux = d
	c.media = media
	c.mediaURL = mediaURL
	c.lastSeq = seg.seq
	return nil
}

// fetchMediaPlaylist fetches and parses the playlist at rawURL, following one
// master-playlist indirection to the selected media playlist. It returns the
// media playlist and its absolute URL (for live reloads).
func (c *Client) fetchMediaPlaylist(ctx context.Context, rawURL string) (*mediaPlaylist, *url.URL, error) {
	body, err := c.get(ctx, rawURL, c.cfg.MaxPlaylistBytes, ErrPlaylistTooLarge)
	if err != nil {
		return nil, nil, err
	}
	base, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrInvalidURL, err)
	}
	media, master, err := parsePlaylist(body, base)
	if err != nil {
		return nil, nil, err
	}
	if master != nil {
		mediaURLStr, serr := master.selectMediaURL()
		if serr != nil {
			return nil, nil, serr
		}
		mBody, merr := c.get(ctx, mediaURLStr, c.cfg.MaxPlaylistBytes, ErrPlaylistTooLarge)
		if merr != nil {
			return nil, nil, merr
		}
		mBase, _ := url.Parse(mediaURLStr)
		media, master, err = parsePlaylist(mBody, mBase)
		if err != nil {
			return nil, nil, err
		}
		if master != nil {
			return nil, nil, fmt.Errorf("%w: master playlist referenced another master", ErrMalformedPlaylist)
		}
		return media, mBase, nil
	}
	return media, base, nil
}

// firstPlayable returns the first non-gap segment of a media playlist.
func firstPlayable(m *mediaPlaylist) (mediaSegment, bool) {
	for _, s := range m.segments {
		if !s.gap {
			return s, true
		}
	}
	return mediaSegment{}, false
}

// newHTTPClient builds the per-source HTTP client. Unlike the httpsource client,
// it follows redirects (HLS relies on CDN 3xx for both playlists and segments,
// bounded by net/http's default of 10), leaves compression enabled (playlists
// are text, often gzipped), and keeps connections alive (a session fetches many
// small segments).
func newHTTPClient(cfg *Config, tgt *target) *http.Client {
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: cfg.Timeout}).DialContext,
		TLSClientConfig:     tlsConfigFor(cfg, tgt),
		TLSHandshakeTimeout: cfg.Timeout,
		ForceAttemptHTTP2:   true,
	}
	return &http.Client{Transport: tr}
}

// tlsConfigFor returns the tls.Config for https requests. Unlike httpsource, it
// does not pin ServerName, because segments may live on a different host than
// the playlist and net/http must derive the SNI/verification name per request.
func tlsConfigFor(cfg *Config, _ *target) *tls.Config {
	if cfg.TLSConfig != nil {
		tc := cfg.TLSConfig.Clone()
		if tc.MinVersion == 0 {
			tc.MinVersion = tls.VersionTLS12
		}
		return tc
	}
	return &tls.Config{
		InsecureSkipVerify: cfg.InsecureTLS, //nolint:gosec // Opt-in via Config.InsecureTLS for self-signed endpoints, mirroring the other sources.
		MinVersion:         tls.VersionTLS12,
	}
}

// get issues a bounded GET for rawURL and returns its body, enforcing the byte
// cap with an over-read of one byte. Each request is bounded by Config.Timeout.
// Basic credentials are attached preemptively only for a same-host request on a
// safe connection (TLS, or AllowInsecureAuth for plaintext), so a cross-host CDN
// redirect never carries them. It classifies a transport failure to
// ErrRequestTimeout, the caller context error, or ErrConnectionClosed, a
// non-success status to a *StatusError (ErrBadStatus), and an over-cap body to
// tooLarge.
func (c *Client) get(ctx context.Context, rawURL string, limit int, tooLarge error) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidURL, err)
	}
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	c.maybeSetBasicAuth(req)

	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, c.classifyRequestErr(ctx, reqCtx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{Code: resp.StatusCode, Status: resp.Status}
	}

	// Read up to limit+1 bytes: exceeding limit means the body is over the cap.
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if len(body) > limit {
		return nil, fmt.Errorf("%w: body exceeds the %d-byte limit", tooLarge, limit)
	}
	if rerr != nil {
		return nil, c.classifyRequestErr(ctx, reqCtx, rerr)
	}
	return body, nil
}

// maybeSetBasicAuth attaches preemptive Basic credentials when the request is to
// the playlist host on a safe connection. A cross-host redirect target or a
// plaintext connection without the opt-in is left bare.
func (c *Client) maybeSetBasicAuth(req *http.Request) {
	if c.tgt.username == "" && c.tgt.password == "" {
		return
	}
	if req.URL.Host != c.tgt.host {
		return // do not leak credentials to a different host
	}
	if req.URL.Scheme == schemeHTTPS || c.cfg.AllowInsecureAuth {
		req.SetBasicAuth(c.tgt.username, c.tgt.password)
		if req.URL.Scheme != schemeHTTPS && c.cfg.Logger != nil {
			c.cfg.Logger.Warn("hlssource: sending Basic credentials over a plaintext http connection", "url", c.url)
		}
	}
}

// classifyRequestErr maps a transport error to a terminal cause: ErrRequestTimeout
// when the per-request deadline fired or the transport timed out, the caller's
// ctx error when Open's context cancelled, context.Canceled when the source's own
// shutdown cancelled it, and ErrConnectionClosed otherwise.
func (c *Client) classifyRequestErr(base, reqCtx context.Context, err error) error {
	if errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", ErrRequestTimeout, err)
	}
	if cerr := base.Err(); cerr != nil {
		return cerr
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return fmt.Errorf("%w: %w", ErrRequestTimeout, err)
	}
	return fmt.Errorf("%w: %w", ErrConnectionClosed, err)
}

// Format returns the source's audio format descriptor: the resolved AAC codec
// with KindCompressed and, per the AudioFormat contract for compressed audio,
// SampleRate and Channels 0 (the decoder determines the true geometry). It is
// immutable after Open and safe from any goroutine, including from inside OnFrame.
func (c *Client) Format() audiostream.AudioFormat {
	return audiostream.AudioFormat{
		Codec: c.codec,
		Kind:  audiostream.PayloadKindFor(c.codec),
	}
}

// Wait blocks until the stream ends and returns the terminal cause: ErrStreamEnded
// on an orderly VOD end, audiostream.ErrClosed after Close, ctx.Err() if ctx
// cancels first, audiostream.ErrReadTimeout on watchdog expiry, or a playlist,
// segment, or connection error. The first cause wins. After Wait returns, OnFrame
// will not be called again. Do not call Wait from inside OnFrame; it deadlocks.
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

// Close ends the stream. It is idempotent and safe from any goroutine, including
// from inside OnFrame. It cancels the in-flight request and signals the reader;
// Wait afterwards returns audiostream.ErrClosed unless an earlier cause won.
func (c *Client) Close() error {
	c.initiateShutdown(audiostream.ErrClosed)
	return nil
}

// Stats returns a snapshot of the source's receive counters, keyed by track 0.
// PayloadBytes is the delivered access-unit bytes; Malformed counts framer
// resync discards. LastFrameAt is the last successful read time. Safe from any
// goroutine, including from inside OnFrame.
func (c *Client) Stats() audiostream.Stats {
	ts := audiostream.TrackStats{
		Packets:      c.packets.Load(),
		PayloadBytes: c.payload.Load(),
		Malformed:    c.malformed.Load(),
	}
	if nanos := c.lastReadAt.Load(); nanos != 0 {
		ts.LastFrameAt = time.Unix(0, nanos)
	}
	capturedAt := time.Now()
	return audiostream.Stats{
		CapturedAt: capturedAt,
		Tracks:     map[int]audiostream.TrackStats{0: ts},
	}
}

// Info returns the source-neutral identity snapshot: the playlist URL with
// credentials stripped. Server is left empty; HLS has no single server identity.
// Safe from any goroutine, including from inside OnFrame.
func (c *Client) Info() audiostream.SourceInfo {
	return audiostream.SourceInfo{URL: c.url}
}

// initiateShutdown funnels every terminal trigger through one place, exactly
// once; the first cause wins. It records the cause, signals the reader and
// watchdog by closing closing, and cancels the in-flight request.
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

// reader is the single reader goroutine. A deferred recover funnels a panic (an
// OnFrame callback above all) into shutdown; when the loop ends it joins the
// watchdog and, as its final act, closes done.
func (c *Client) reader() {
	defer close(c.done)
	defer c.teardown()
	defer c.recoverReader()
	c.deliverPending()
	c.runSegments()
}

// recoverReader turns a panic in the reader into a fatal shutdown cause.
func (c *Client) recoverReader() {
	if r := recover(); r != nil {
		c.initiateShutdown(fmt.Errorf("hlssource: reader panic: %v", r))
	}
}

// deliverPending delivers the first segment's access units, demuxed during Open.
func (c *Client) deliverPending() {
	now := time.Now()
	for _, au := range c.pending {
		c.deliverAU(au.data, au.dur, now)
	}
	c.pending = nil
}

// runSegments processes the media playlist's remaining segments in order,
// reloading for a live playlist and ending on a VOD ENDLIST. It funnels its
// terminal cause and returns.
func (c *Client) runSegments() {
	for {
		delivered := c.processNewSegments()
		if c.closed() {
			return
		}
		if c.media.endList {
			c.demux.end(func(au []byte, dur time.Duration) { c.deliverAU(au, dur, time.Now()) })
			c.initiateShutdown(ErrStreamEnded)
			return
		}
		if !c.reloadWait(delivered) {
			return
		}
		media, mediaURL, err := c.fetchMediaPlaylist(c.reqBaseCtx(), c.mediaURL.String())
		if err != nil {
			c.initiateShutdown(err)
			return
		}
		c.stampRead()
		c.media, c.mediaURL = media, mediaURL
	}
}

// processNewSegments fetches, demuxes and delivers every segment with a media
// sequence past lastSeq, in order. It returns whether any new segment was
// delivered, which drives the reload backoff. A shutdown or a fatal segment
// error stops it early (the caller checks closed()).
func (c *Client) processNewSegments() bool {
	delivered := false
	forceDisc := false
	for _, seg := range c.media.segments {
		if seg.seq <= c.lastSeq {
			continue
		}
		if !delivered && seg.seq > c.lastSeq+1 {
			// The live window advanced past the next expected segment: the
			// intervening segments were dropped. Signal the loss and reset the
			// demux domain for the resumed stream.
			missed := int(seg.seq - c.lastSeq - 1)
			c.pendingSeqGap += missed
			forceDisc = true
			if c.cfg.Logger != nil {
				c.cfg.Logger.Warn("hlssource: fell behind the live window", "missed_segments", missed)
			}
		}
		if err := c.processSegment(seg, forceDisc); err != nil {
			c.initiateShutdown(err)
			return delivered
		}
		c.lastSeq = seg.seq
		delivered = true
		forceDisc = false
		if c.closed() {
			return delivered
		}
	}
	return delivered
}

// processSegment downloads and demuxes one segment, delivering its access units.
// A gap segment (EXT-X-GAP) is not fetched: the media clock advances by its
// declared duration and the loss is signalled on the next delivered frame.
func (c *Client) processSegment(seg mediaSegment, forceDisc bool) error {
	if seg.gap {
		c.mediaPTS += seg.duration
		c.pendingSeqGap++
		return nil
	}
	body, err := c.get(c.reqBaseCtx(), seg.uri, c.cfg.MaxSegmentBytes, ErrSegmentTooLarge)
	if err != nil {
		return err
	}
	c.stampRead()
	now := time.Now()
	return c.demux.demux(body, seg.discontinuity || forceDisc, func(au []byte, dur time.Duration) {
		c.deliverAU(au, dur, now)
	})
}

// deliverAU counts one access-unit delivery and hands it to OnFrame. PTS is the
// running media time before this frame advances it, so the first frame is at PTS
// 0 and successive PTSs increase by each frame's duration. Any accumulated loss
// (a gap segment or a window drop) is reported as SeqGap on this frame, then
// cleared. data aliases reader-owned memory and is valid only during the callback.
func (c *Client) deliverAU(data []byte, dur time.Duration, now time.Time) {
	c.packets.Add(1)
	c.payload.Add(uint64(len(data)))
	pts := c.mediaPTS
	if dur > 0 {
		c.mediaPTS += dur
	}
	seqGap := c.pendingSeqGap
	c.pendingSeqGap = 0
	if c.cfg.OnFrame == nil {
		return
	}
	c.cfg.OnFrame(audiostream.Frame{
		TrackID:    0,
		Data:       data,
		RTPTime:    0,
		PTS:        pts,
		ReceivedAt: now,
		SeqGap:     seqGap,
	})
}

// reloadDelayFor computes the RFC 8216 reload cadence: one target duration after
// a reload that delivered new segments, half that after one that did not. It is a
// package var so a test can substitute a fast, deterministic delay; production
// keeps the spec cadence.
var reloadDelayFor = func(target time.Duration, delivered bool) time.Duration {
	if target <= 0 {
		target = DefaultTimeout
	}
	if !delivered {
		target /= 2
	}
	return target
}

// reloadWait sleeps the reload cadence before the next playlist fetch. The wait
// is cancelable: it returns false if the source is shutting down.
func (c *Client) reloadWait(delivered bool) bool {
	d := reloadDelayFor(c.media.targetDuration, delivered)
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-c.closing:
		return false
	case <-timer.C:
		return true
	}
}

// reqBaseCtx returns the base context for a streaming request: our own cancel
// (via initiateShutdown) aborts it, and it is divorced from the caller's Open
// context, so only Close or the watchdog ends the stream.
func (c *Client) reqBaseCtx() context.Context { return c.baseCtx }

// stampRead records a successful playlist or segment body read for the watchdog.
func (c *Client) stampRead() { c.lastReadAt.Store(time.Now().UnixNano()) }

// closed reports whether shutdown has begun.
func (c *Client) closed() bool {
	select {
	case <-c.closing:
		return true
	default:
		return false
	}
}

// teardown is the reader's terminal sequence: join the watchdog goroutine. It
// runs after the loop funneled its cause, so closing is already closed.
func (c *Client) teardown() { c.wg.Wait() }

// watchdog ends the stream with audiostream.ErrReadTimeout when no successful
// playlist or segment read arrives within Config.ReadIdle. It re-arms from its
// own goroutine, so a healthy stream is never spuriously killed, and exits on
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
