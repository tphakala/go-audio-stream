package rtsp

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
)

// Client defaults. A zero Config field falls back to these.
const (
	// DefaultTimeout bounds the dial and each request/response round-trip
	// when Config.Timeout is zero.
	DefaultTimeout = 10 * time.Second
	// DefaultUserAgent is sent on every request when Config.UserAgent is
	// empty.
	DefaultUserAgent = "go-audio-stream/0.1"
)

// defaultRTSPPort and defaultRTSPSPort are the IANA-assigned control ports
// used when the URL omits an explicit port.
const (
	defaultRTSPPort  = 554
	defaultRTSPSPort = 322
)

// Config configures a Client. OnFrame is registered here, before any Setup
// or Play, so frame delivery is race-free no matter how early the server
// starts sending. A Config value carries credentials; do not log it.
type Config struct {
	// URL is the rtsp:// or rtsps:// target. Credentials may be embedded in
	// the userinfo; they are never logged and are redacted in any
	// stringification.
	URL string
	// Username and Password supply credentials when URL has no userinfo.
	// Ignored when the URL carries userinfo. Never logged.
	Username string
	Password string
	// Timeout bounds the dial and each request/response round-trip. Zero uses
	// DefaultTimeout.
	Timeout time.Duration
	// ReadIdle is the watchdog window: once playing, no interleaved frame
	// within ReadIdle ends Wait with ErrReadTimeout. Zero disables it.
	ReadIdle time.Duration
	// TLSConfig is used for rtsps. Nil means verified TLS with the URL host
	// as the server name.
	TLSConfig *tls.Config
	// InsecureTLS opts into skipping certificate verification for rtsps
	// (self-signed cameras). Ignored when TLSConfig is non-nil.
	InsecureTLS bool
	// UserAgent is sent on every request. Zero uses DefaultUserAgent.
	UserAgent string
	// OnFrame receives every delivered frame on the reader goroutine. It must
	// not block and must not call Describe, Setup, Play, or Wait (only Close
	// and Stats are callback-safe). Frame.Data is valid only for the duration
	// of the call. Nil is allowed: frames are then counted in Stats but not
	// delivered.
	OnFrame func(audiostream.Frame)
	// Logger receives diagnostics; nil disables logging. URLs are passed
	// through RedactURL before logging; credentials are never logged.
	Logger *slog.Logger
}

// applyDefaults fills zero-valued Timeout and UserAgent with their defaults.
func (c *Config) applyDefaults() {
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.UserAgent == "" {
		c.UserAgent = DefaultUserAgent
	}
}

// Track is one media track discovered by Describe and passed to Setup.
type Track struct {
	// ID is a stable per-session id (the SDP media index).
	ID int
	// Media is the track media kind (audio, video, other).
	Media audiostream.MediaKind
	// Codec is the resolved payload codec.
	Codec audiostream.Codec
	// ClockRate is the RTP clock rate in Hz from the rtpmap.
	ClockRate int
	// Channels is the audio channel count from the rtpmap, 0 when absent.
	Channels int
	// Control is the resolved absolute control URL for this track.
	Control string
}

// SetupOptions controls one Setup. Discard sets up a track whose frames are
// dropped inside the reader without per-packet allocation or delivery.
type SetupOptions struct {
	// Discard drops this track's frames in the reader, counting them in Stats
	// but never depacketizing or delivering them.
	Discard bool
}

// target is the resolved dial destination parsed from a Config: the address
// to connect, the request-URI to use on the wire (userinfo stripped), the
// TLS server name, and the credentials the auth flow will use.
type target struct {
	tls        bool
	address    string
	requestURL string
	serverName string
	username   string
	password   string
}

// parseTarget resolves cfg.URL into a target. It validates the scheme
// (rtsp or rtsps), supplies the default port when absent, extracts
// credentials (URL userinfo overrides Config), and strips userinfo from the
// request URL. It returns ErrInvalidURL (wrapped) on any malformed input.
func parseTarget(cfg *Config) (target, error) {
	raw := strings.TrimSpace(cfg.URL)
	if raw == "" {
		return target{}, fmt.Errorf("%w: empty URL", ErrInvalidURL)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return target{}, fmt.Errorf("%w: %w", ErrInvalidURL, err)
	}

	var tlsOn bool
	var defPort int
	switch strings.ToLower(u.Scheme) {
	case "rtsp":
		tlsOn, defPort = false, defaultRTSPPort
	case "rtsps":
		tlsOn, defPort = true, defaultRTSPSPort
	default:
		return target{}, fmt.Errorf("%w: unsupported scheme %q", ErrInvalidURL, u.Scheme)
	}

	host := u.Hostname()
	if host == "" {
		return target{}, fmt.Errorf("%w: missing host", ErrInvalidURL)
	}
	port := u.Port()
	if port == "" {
		port = strconv.Itoa(defPort)
	}

	username, password := cfg.Username, cfg.Password
	if u.User != nil {
		username = u.User.Username()
		if p, ok := u.User.Password(); ok {
			password = p
		} else {
			password = ""
		}
	}

	reqURL := *u
	reqURL.User = nil
	return target{
		tls:        tlsOn,
		address:    net.JoinHostPort(host, port),
		requestURL: reqURL.String(),
		serverName: host,
		username:   username,
		password:   password,
	}, nil
}
