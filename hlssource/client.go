package hlssource

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
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
	// goroutine afterward. demux is the long-lived segment demuxer (TS or fMP4);
	// media is the
	// current media playlist; mediaURL is its absolute URL for live reloads;
	// pending holds the first segment's access units, demuxed during Open to
	// resolve the codec and delivered first by the reader. lastSeq is the highest
	// media sequence number delivered; mediaPTS is the running presentation time;
	// pendingSeqGap accumulates segments lost to a gap or a window drop, attached
	// to the next delivered frame.
	demux    segmentDemuxer
	media    *mediaPlaylist
	mediaURL *url.URL
	// initURI is the EXT-X-MAP initialization-segment URL the fMP4 demuxer was
	// built from, or "" for an MPEG-TS stream. A later fMP4 segment naming a
	// different init URI rebuilds the demuxer from it (see reinitFMP4); a change
	// that crosses the container boundary in either direction (one side "") ends
	// the stream with ErrUnsupportedPlaylist.
	initURI string
	// asc is the AudioSpecificConfig currently in effect, the comparison snapshot
	// for firing OnCodecUpdate after an init-segment change. It is seeded at Open
	// from the first init and is reader-owned thereafter; c.codec keeps the
	// Open-time value so Format stays lock-free.
	asc []byte
	// malformedBase carries the accumulated gap count of every retired demuxer.
	// syncMalformed publishes it plus the live demuxer's own count, so swapping
	// demuxers on an init change cannot make the source's malformed counter jump
	// backwards.
	malformedBase uint64
	pending       []pendingAU
	lastSeq       uint64
	mediaPTS      time.Duration
	pendingSeqGap int
	// pendingDisc carries a continuity break (a live-window drop, an explicit
	// EXT-X-DISCONTINUITY, or a skipped EXT-X-GAP) to the next real segment, so
	// the reset is not lost when the break lands on a gap segment the reader does
	// not demux. A real segment consumes and clears it.
	pendingDisc bool

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

// segmentDemuxer is the boundary the reader loop depends on, abstracting the
// per-container demuxer so client.go never names a concrete type. Both the
// MPEG-TS demuxer (tsDemux) and the fMP4/CMAF demuxer (fmp4Demux) satisfy it. A
// call to demux processes one whole segment, delivering each AAC access unit to
// onAU in order with its duration; discontinuity requests a continuity-domain
// reset before the segment (a no-op for the self-contained fMP4 fragments). end
// flushes any buffered trailing frame at true stream end, or when this demuxer is
// retired mid-stream on an initialization-segment change. audioSpecificConfig
// returns the resolved AAC configuration once known, by reference: it is the
// demuxer's own slice, so a caller that hands it outside the package copies it
// first. gapCount is the running count of lost or unparsable audio for THIS
// demuxer; the source's malformed counter is that plus every retired demuxer's
// final count (see syncMalformed).
type segmentDemuxer interface {
	demux(seg []byte, discontinuity bool, onAU func(au []byte, dur time.Duration)) error
	end(onAU func(au []byte, dur time.Duration))
	audioSpecificConfig() []byte
	gapCount() uint64
}

// Both demuxers satisfy the boundary verbatim; tsDemux keeps its exact prior
// method set, so this extraction is behavior-preserving.
var (
	_ segmentDemuxer = (*tsDemux)(nil)
	_ segmentDemuxer = (*fmp4Demux)(nil)
)

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
			// openHandshake may have left an idle keep-alive connection in the pool
			// (the playlist and any first-segment fetch succeeded before a later step
			// failed). Reap it here so a failed Open does not strand a socket, the same
			// leak teardown closes on the reader path. Under supervisor's
			// reconnect-per-attempt Factory an Open that fails after connecting would
			// otherwise accumulate one idle socket per attempt until IdleConnTimeout.
			c.httpc.CloseIdleConnections()
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
		// fetchMediaPlaylist has already validated the body's structure, so a
		// missing playable segment here is never a malformed playlist. For a LIVE
		// playlist (no EXT-X-ENDLIST) it is a well-formed playlist whose present
		// segments are all EXT-X-GAP, or one with no segments yet; RFC 8216 permits
		// that and a later reload can bring playable segments, so surface the
		// retryable ErrNoPlayableSegment. A VOD playlist (EXT-X-ENDLIST) is final:
		// no reload will ever add a playable segment, so returning a retryable cause
		// would spin a supervisor forever against a dead playlist. Keep the
		// permanent-shaped ErrMalformedPlaylist for that terminal case.
		if media.endList {
			return fmt.Errorf("%w: VOD playlist has no playable segment", ErrMalformedPlaylist)
		}
		return ErrNoPlayableSegment
	}
	d, err := c.buildDemuxer(ctx, seg)
	if err != nil {
		return err
	}
	body, err := c.get(ctx, seg.uri, c.cfg.MaxSegmentBytes, ErrSegmentTooLarge)
	if err != nil {
		return err
	}
	if derr := d.demux(body, false, func(au []byte, dur time.Duration) {
		c.pending = append(c.pending, pendingAU{data: bytes.Clone(au), dur: dur})
	}); derr != nil {
		return derr
	}
	asc := d.audioSpecificConfig()
	if asc == nil {
		return fmt.Errorf("%w: first segment carried no AAC access unit", ErrMalformedSegment)
	}
	// audioSpecificConfig returns the demuxer's own slice by reference, so both of
	// these take a copy. c.codec is the one that makes it load-bearing: Format
	// hands it to arbitrary callers, including from inside OnFrame, and an alias
	// would let any of them reach into the running demuxer's resolved
	// configuration. c.asc is the change-comparison snapshot; its copy is
	// defensive at the segmentDemuxer boundary rather than against any current
	// implementation, since neither demuxer mutates its ASC after resolving it.
	c.codec = audiostream.CodecAAC{AudioSpecificConfig: bytes.Clone(asc)}
	c.asc = bytes.Clone(asc)
	c.demux = d
	c.initURI = seg.initURI
	c.media = media
	c.mediaURL = mediaURL
	c.lastSeq = seg.seq
	return nil
}

// buildDemuxer selects and constructs the demuxer for the first playable segment:
// an fMP4 demuxer when the segment carries an EXT-X-MAP init URI (its
// initialization segment is read through fetchInitSegment, which owns the size
// bound, so the AudioSpecificConfig is resolved before any fragment is demuxed),
// or a plain MPEG-TS demuxer otherwise.
func (c *Client) buildDemuxer(ctx context.Context, seg mediaSegment) (segmentDemuxer, error) {
	if seg.initURI == "" {
		return newTSDemux(), nil
	}
	initBody, err := c.fetchInitSegment(ctx, seg.initURI)
	if err != nil {
		return nil, err
	}
	return newFMP4Demux(initBody)
}

// fetchInitSegment fetches an EXT-X-MAP initialization segment, bounded by the
// segment size cap. It is the single place an init is read, shared by Open (via
// buildDemuxer) and by the mid-stream replacement path (reinitFMP4), so a change
// to how an init is fetched (a byte-range check, a conditional GET, an init size
// cap distinct from the media one) lands once rather than in two places.
//
// It deliberately stops at the bytes and does not build the demuxer, because the
// two callers differ in what happens between the two steps: reinitFMP4 stamps
// the read-idle watchdog on a successful init read and Open does not, having no
// watchdog running yet. That asymmetry stays at the call sites.
func (c *Client) fetchInitSegment(ctx context.Context, initURI string) ([]byte, error) {
	return c.get(ctx, initURI, c.cfg.MaxSegmentBytes, ErrSegmentTooLarge)
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
		// Bound how long a pooled connection may sit idle. http.DefaultTransport
		// uses 90s; this transport is hand-rolled and otherwise inherits 0 (no
		// limit). teardown closes idle connections promptly on a clean shutdown, so
		// this is the belt-and-braces reap for a connection that goes idle while the
		// session is still live.
		IdleConnTimeout: 90 * time.Second,
	}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("hlssource: stopped after 10 redirects")
			}
			// net/http copies the Authorization header onto a same-domain or
			// subdomain redirect, including an https to http downgrade. Re-apply
			// the same-host, safe-scheme gate maybeSetBasicAuth uses, so a redirect
			// can never carry Basic credentials to a different host or onto a
			// plaintext connection without the opt-in.
			if req.URL.Host != tgt.host || (req.URL.Scheme != schemeHTTPS && !cfg.AllowInsecureAuth) {
				req.Header.Del("Authorization")
			}
			return nil
		},
	}
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
//
// Immutable means it does not follow a mid-stream EXT-X-MAP change either: a live
// fMP4 playlist can scroll in a new initialization segment whose configuration
// differs, and Format keeps reporting what Open resolved. A consumer that must
// track the live configuration registers Config.OnCodecUpdate, without which such
// a change ends the stream rather than going unreported.
//
// The returned CodecAAC.AudioSpecificConfig is read-only and is the same slice on
// every call. It is the source's own copy, taken at Open, and not the running
// demuxer's, so it cannot be used to reach into the demuxer's resolved
// configuration; a caller that mutates it corrupts only what Format reports.
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
	c.syncMalformed() // publish any discards from the Open-phase demux
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
			c.syncMalformed()
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
	for _, seg := range c.media.segments {
		if seg.seq <= c.lastSeq {
			continue
		}
		if !delivered && seg.seq > c.lastSeq+1 {
			// The live window advanced past the next expected segment: the
			// intervening segments were dropped. Signal the loss and mark a
			// continuity break for the resumed stream. Clamp before the int
			// conversion so a huge (or hostile) media-sequence jump cannot report
			// a negative SeqGap.
			gap := seg.seq - c.lastSeq - 1
			if gap > uint64(math.MaxInt) {
				gap = math.MaxInt
			}
			missed := int(gap)
			c.pendingSeqGap += missed
			c.pendingDisc = true
			if c.cfg.Logger != nil {
				c.cfg.Logger.Warn("hlssource: fell behind the live window", "missed_segments", missed)
			}
		}
		disc := c.pendingDisc || seg.discontinuity
		if err := c.processSegment(seg, disc); err != nil {
			c.initiateShutdown(err)
			return delivered
		}
		// A gap segment is not demuxed, so it cannot consume the discontinuity;
		// carry it (and force one, since a gap breaks audio continuity) to the
		// next real segment. A real segment resets the demux domain and clears it.
		c.pendingDisc = seg.gap
		c.lastSeq = seg.seq
		delivered = true
		if c.closed() {
			return delivered
		}
	}
	return delivered
}

// processSegment downloads and demuxes one segment, delivering its access units.
// disc resets the demux continuity domain before this segment. A gap segment
// (EXT-X-GAP) is not fetched: the media clock advances by its declared duration
// and the loss is signalled on the next delivered frame.
func (c *Client) processSegment(seg mediaSegment, disc bool) error {
	if seg.gap {
		c.mediaPTS += seg.duration
		c.pendingSeqGap++
		return nil
	}
	// A segment naming a different EXT-X-MAP init URI needs a demuxer rebuilt from
	// that init. Within fMP4 that is supported; across the container boundary it
	// is not, so a stream that switches between TS and fMP4 (one side "") still
	// ends here rather than swapping demuxer families mid-stream.
	if seg.initURI != c.initURI {
		if seg.initURI == "" {
			return fmt.Errorf("%w: stream switched container mid-stream (EXT-X-MAP dropped: fMP4 to MPEG-TS)",
				ErrUnsupportedPlaylist)
		}
		if c.initURI == "" {
			return fmt.Errorf("%w: stream switched container mid-stream (EXT-X-MAP added: MPEG-TS to fMP4)",
				ErrUnsupportedPlaylist)
		}
		if err := c.reinitFMP4(seg.initURI); err != nil {
			return err
		}
	}
	body, err := c.get(c.reqBaseCtx(), seg.uri, c.cfg.MaxSegmentBytes, ErrSegmentTooLarge)
	if err != nil {
		return err
	}
	c.stampRead()
	now := time.Now()
	derr := c.demux.demux(body, disc, func(au []byte, dur time.Duration) {
		c.deliverAU(au, dur, now)
	})
	c.syncMalformed()
	return derr
}

// reinitFMP4 rebuilds the fMP4 demuxer from a replacement EXT-X-MAP
// initialization segment that a live playlist scrolled in, so a stream that
// legitimately changes its init mid-session keeps playing instead of ending.
//
// The retired demuxer is flushed through the segmentDemuxer boundary before it
// is dropped (a no-op for fMP4, whose fragments carry whole samples, but the
// boundary is what client.go depends on, not the concrete type), and its gap
// count is folded into malformedBase so the source's malformed counter cannot
// jump backwards across the swap.
//
// A new init whose AudioSpecificConfig differs is a real codec change: it fires
// Config.OnCodecUpdate here, before the replacement segment is fetched and
// demuxed, which is what gives the callback its documented ordering guarantee
// relative to the first frame under the new configuration. An init carrying the
// same config is a re-publish and fires nothing. Format().Codec keeps reporting
// what Open resolved either way, so it stays lock-free for a concurrent caller.
//
// A changed configuration is never delivered silently. Format().Codec is fixed
// at Open, so Config.OnCodecUpdate is the ONLY way the new AudioSpecificConfig
// can reach a consumer, and a supervisor-wrapped source cannot even reach
// Format (audiostream.Source does not include it). When the configuration
// changed and no callback is registered, this therefore keeps the pre-existing
// terminal behaviour and ends the stream with ErrUnsupportedPlaylist, which is
// retryable: a supervisor reconnects and Open re-resolves the configuration from
// the new init, exactly as it did before this path existed. Registering the
// callback is what opts a consumer into playing through the change. The
// alternative, continuing with a stale ASC, would feed access units encoded
// under the new configuration to a decoder configured for the old one and
// produce plausible but wrong audio, which this library refuses to do anywhere.
//
// N segments in a playlist cost at most 2N bounded GETs (one init, one segment),
// so a hostile or broken playlist that names a new init URI on every segment
// doubles the fetch count it already commanded; it cannot loop or allocate
// without bound.
func (c *Client) reinitFMP4(initURI string) error {
	initBody, err := c.fetchInitSegment(c.reqBaseCtx(), initURI)
	if err != nil {
		return err
	}
	// A successful init read is new bytes off the network: stamp the read-idle
	// watchdog so a slow init fetch cannot starve it before the segment that
	// follows stamps its own read. This is the caller-side half of the asymmetry
	// fetchInitSegment's doc describes (Open has no watchdog to stamp yet).
	c.stampRead()
	d, err := newFMP4Demux(initBody)
	if err != nil {
		return err
	}
	// Everything that can fail happens BEFORE any state is mutated, so a rejected
	// replacement leaves the client exactly as it was rather than half-swapped.
	// The nil check defends the segmentDemuxer boundary rather than any current
	// implementation: newFMP4Demux resolves the config or fails, so it does not
	// fire today.
	asc := d.audioSpecificConfig()
	if asc == nil {
		return fmt.Errorf("%w: replacement initialization segment carried no AudioSpecificConfig", ErrMalformedSegment)
	}
	changed := !bytes.Equal(asc, c.asc)
	if changed && c.cfg.OnCodecUpdate == nil {
		if c.cfg.Logger != nil {
			c.cfg.Logger.Warn("hlssource: initialization segment changed the audio configuration "+
				"and no OnCodecUpdate is registered; ending the stream rather than decoding on with a stale configuration",
				"url", c.url)
		}
		return fmt.Errorf("%w: the initialization segment changed the audio configuration mid-stream "+
			"and no Config.OnCodecUpdate is registered to receive it", ErrUnsupportedPlaylist)
	}

	// Validation is complete; from here nothing can fail, so the swap happens in
	// full or not at all.
	now := time.Now()
	c.demux.end(func(au []byte, dur time.Duration) { c.deliverAU(au, dur, now) })
	c.malformedBase += c.demux.gapCount()
	c.demux = d
	c.initURI = initURI
	c.syncMalformed()
	if !changed {
		return nil // a re-published init with the same configuration, not a codec change
	}
	// Two copies of one slice, because audioSpecificConfig returns the live
	// demuxer's own by reference and Config.OnCodecUpdate documents the value it
	// hands out as read-only. The snapshot keeps the next comparison independent
	// of anything a consumer does; the callback's copy keeps a consumer that
	// ignores the contract away from the demuxer's resolved configuration. They
	// are redundant barriers against the same hazard, so no single current code
	// path distinguishes dropping one from keeping both, and each is defensive at
	// the segmentDemuxer boundary rather than against any implementation in tree.
	//
	// That redundancy is specific to THIS source and does not carry over to the
	// structurally identical block in rtsp's deliverLATM, where the two copies are
	// NOT interchangeable. The difference is that a replacement init builds a
	// whole new demuxer here (newFMP4Demux resolves the config once at
	// construction), so nothing recycles a buffer across the comparison, whereas
	// the LATM depacketizer keeps one long-lived pair of ASC arrays and parses
	// each new config into the one it is about to promote. Dropping rtsp's
	// snapshot there silences real mid-stream config changes; see the comment on
	// that block before applying this one by analogy.
	c.asc = bytes.Clone(asc)
	if c.cfg.Logger != nil {
		c.cfg.Logger.Info("hlssource: initialization segment changed; audio configuration updated",
			"url", c.url, "asc", hex.EncodeToString(asc))
	}
	c.cfg.OnCodecUpdate(audiostream.CodecUpdate{
		TrackID: 0,
		Codec:   audiostream.CodecAAC{AudioSpecificConfig: bytes.Clone(asc)},
	})
	return nil
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

// stampRead records a successful playlist, initialization-segment, or segment
// body read for the watchdog.
func (c *Client) stampRead() { c.lastReadAt.Store(time.Now().UnixNano()) }

// syncMalformed publishes the demuxer's running framer-discard count as the
// source's malformed counter, mirroring how httpsource surfaces GapCount. The
// live demuxer counts only its own discards, so the retired demuxers' totals are
// carried in malformedBase: the published counter is therefore monotonic across
// an initialization-segment swap rather than restarting from the new demuxer's
// zero.
func (c *Client) syncMalformed() { c.malformed.Store(c.malformedBase + c.demux.gapCount()) }

// closed reports whether shutdown has begun.
func (c *Client) closed() bool {
	select {
	case <-c.closing:
		return true
	default:
		return false
	}
}

// teardown closes the transport's idle keep-alive connections, then waits for the
// client's goroutines to finish. The transport keeps connections alive across a
// session's many small segment fetches, so without this a retired Client strands
// up to MaxIdleConnsPerHost idle sockets per host, each pinned against GC by its
// readLoop/writeLoop pair. That matters under supervisor, which builds a fresh
// Client per reconnect attempt: a flapping source would otherwise accumulate one
// abandoned pool per attempt. teardown runs deferred in reader after the segment
// loop has returned, so no request is in flight when the idle conns are closed.
func (c *Client) teardown() {
	if c.httpc != nil {
		c.httpc.CloseIdleConnections()
	}
	c.wg.Wait()
}

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
