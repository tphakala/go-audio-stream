package hlssource

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/internal/urltarget"
)

// Client defaults. A zero Config field falls back to these.
const (
	// DefaultTimeout bounds each individual HTTP request (a playlist fetch or a
	// segment download): the connect, TLS handshake, and the whole body transfer.
	DefaultTimeout = 10 * time.Second
	// DefaultUserAgent is sent when Config.UserAgent is empty. It matches the
	// value the other sources use, so they identify the library the same way.
	DefaultUserAgent = "go-audio-stream/0.1"
	// DefaultMaxPlaylistBytes caps a playlist body. A media playlist is small
	// text; 4 MiB is generous and bounds an untrusted or hostile response.
	DefaultMaxPlaylistBytes = 4 << 20
	// DefaultMaxSegmentBytes caps a segment body. A few seconds of AAC is well
	// under a megabyte; 16 MiB bounds a mislabelled or hostile segment.
	DefaultMaxSegmentBytes = 16 << 20
)

// Config configures an Open. OnFrame is registered here, before the stream is
// opened, so frame delivery is race-free no matter how early segments arrive. A
// Config value carries credentials; do not log it.
type Config struct {
	// URL is the http:// or https:// target of an m3u8 playlist (media or
	// master). Credentials may be embedded in the userinfo. This package never
	// logs it, but Config has no String method, so printing a Config with %v
	// exposes the userinfo and Password verbatim.
	URL string
	// Username and Password supply HTTP Basic credentials when the URL carries no
	// non-empty userinfo; a non-empty URL userinfo overrides them. They are sent
	// preemptively (see AllowInsecureAuth) only on requests to the same host as
	// URL, so a cross-host CDN redirect does not leak them. Digest authentication
	// is not supported by this source yet. Never logged.
	Username string
	Password string
	// Timeout bounds each HTTP request: the connect, TLS handshake, and body
	// transfer of one playlist fetch or segment download. Zero or negative uses
	// DefaultTimeout. It does not bound the whole live session; ReadIdle does.
	Timeout time.Duration
	// ReadIdle is the read-idle watchdog window: once streaming, no successful
	// playlist or segment body read within ReadIdle ends Wait with
	// audiostream.ErrReadTimeout. Zero or negative disables it. For a live
	// stream it must exceed the playlist target duration, since the client is
	// intentionally idle between reloads; a value shorter than a segment would
	// trip during that healthy wait.
	ReadIdle time.Duration
	// TLSConfig is used for https. Nil means verified TLS with the per-request
	// host as the server name (segments may live on a different CDN host than the
	// playlist), unless InsecureTLS is set. A non-nil config is cloned and an
	// unset MinVersion is raised to TLS 1.2; its ServerName, if empty, is left
	// empty so net/http derives it per request host.
	TLSConfig *tls.Config
	// InsecureTLS opts into skipping certificate verification for https. Ignored
	// when TLSConfig is non-nil.
	InsecureTLS bool
	// AllowInsecureAuth permits sending Basic credentials over a plaintext http
	// connection. False by default (secure): over plaintext the credentials are
	// withheld and the request is sent bare, so the password is never put on the
	// wire; set it only for a trusted network. Credentials over https are always
	// allowed. This source sends Basic preemptively only; it does not answer a
	// Digest or Basic challenge, so Digest authentication is not supported.
	AllowInsecureAuth bool
	// UserAgent is sent on every request. Empty uses DefaultUserAgent.
	UserAgent string
	// MaxPlaylistBytes caps a playlist body; zero or negative uses
	// DefaultMaxPlaylistBytes. A larger body ends the stream with
	// ErrPlaylistTooLarge.
	MaxPlaylistBytes int
	// MaxSegmentBytes caps a segment body; zero or negative uses
	// DefaultMaxSegmentBytes. A larger body ends the stream with
	// ErrSegmentTooLarge.
	MaxSegmentBytes int
	// OnFrame receives every delivered AAC access unit on the reader goroutine.
	// It must not block and must not call Wait (Close, Stats, Info and Format are
	// the callback-safe ones). Frame.Data is valid only for the duration of the
	// call; consumers that retain audio must copy. Nil is allowed: access units
	// are still counted in Stats, they are simply not delivered.
	OnFrame func(audiostream.Frame)
	// OnCodecUpdate, when non-nil, is called on the reader goroutine when a live
	// playlist scrolls in a new EXT-X-MAP initialization segment whose
	// AudioSpecificConfig differs from the one in effect, so a consumer can
	// re-initialize its decoder for the new configuration. It does NOT fire for
	// the configuration resolved at Open (that one is on Format().Codec), and it
	// does not fire when a replacement init segment carries the same
	// AudioSpecificConfig, which is a re-publish rather than a codec change.
	//
	// Ordering guarantee: OnCodecUpdate fires BEFORE the OnFrame call for the
	// first access unit demuxed under the new configuration, and after the last
	// one demuxed under the old. Like OnFrame it runs on the reader goroutine,
	// must not block, and must not call Wait (Close, Stats, Info and Format are
	// the callback-safe ones). The CodecUpdate's Codec and any slices it carries
	// are owned by the callee only for the duration of the call; copy
	// AudioSpecificConfig to retain it. TrackID is always 0, this source's only
	// track.
	//
	// Format().Codec is fixed at Open and does NOT follow these updates, so a
	// consumer that needs the live configuration must read it here rather than
	// poll Format.
	//
	// Registering it is what opts the source into playing through a mid-stream
	// configuration change. Leaving it nil is safe rather than lossy: because
	// this callback is then the only channel the new configuration could have
	// used, a replacement initialization segment that changes the
	// AudioSpecificConfig ends the stream with ErrUnsupportedPlaylist instead of
	// decoding on with a stale configuration. A replacement carrying the SAME
	// configuration is played either way.
	OnCodecUpdate func(audiostream.CodecUpdate)
	// Logger receives diagnostics for conditions this source handles rather than
	// fails on (a Basic credential sent over plaintext when permitted, a live
	// window the client fell behind, an initialization segment whose replacement
	// changed the audio configuration). The credential-stripped URL is logged,
	// never the credentials.
	Logger *slog.Logger
}

// applyDefaults fills zero or negative fields with their documented defaults.
func (c *Config) applyDefaults() {
	if c.ReadIdle < 0 {
		c.ReadIdle = 0
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.UserAgent == "" {
		c.UserAgent = DefaultUserAgent
	}
	if c.MaxPlaylistBytes <= 0 {
		c.MaxPlaylistBytes = DefaultMaxPlaylistBytes
	}
	if c.MaxSegmentBytes <= 0 {
		c.MaxSegmentBytes = DefaultMaxSegmentBytes
	}
}

// target is the resolved request destination parsed from a Config: the request
// URL with userinfo and fragment stripped, the host (for same-host credential
// gating), and the credentials Basic auth uses. Whether a request is https is
// read per request from its URL scheme, so it is not carried here.
type target struct {
	requestURL string
	host       string
	username   string
	password   string
}

// parseTarget resolves cfg.URL into a target. It validates the scheme (http or
// https) and the port range, extracts credentials (a non-empty URL userinfo
// overrides Config) and rejects CR, LF and NUL in them, and strips the userinfo
// and fragment from the request URL so neither reaches the server. It returns
// ErrInvalidURL (wrapped) on any malformed input. The parsing itself is shared
// with the other network sources via internal/urltarget so a fix to the
// credential handling cannot land in one copy only.
func parseTarget(cfg *Config) (target, error) {
	t, err := urltarget.ParseHTTP(cfg.URL, cfg.Username, cfg.Password)
	if err != nil {
		return target{}, fmt.Errorf("%w: %w", ErrInvalidURL, err)
	}
	return target{
		requestURL: t.RequestURL,
		host:       t.Host, // host:port, for same-origin credential gating
		username:   t.Username,
		password:   t.Password,
	}, nil
}
