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
	// the userinfo. Redact it with RedactURL before logging it: this package
	// never logs the URL itself, but Config has no String method, so printing
	// a Config with %v exposes both the userinfo and Password verbatim.
	URL string
	// Username and Password supply credentials when URL has no userinfo.
	// Ignored when the URL carries userinfo. Never logged.
	Username string
	Password string
	// Timeout bounds the dial and, separately, each request/response
	// round-trip, so an rtsps Dial can take up to three of them (connect, TLS
	// handshake, OPTIONS probe). Zero or negative uses DefaultTimeout.
	Timeout time.Duration
	// ReadIdle is the watchdog window: once playing, no interleaved frame
	// within ReadIdle ends Wait with audiostream.ErrReadTimeout. Zero or
	// negative disables it.
	ReadIdle time.Duration
	// TLSConfig is used for rtsps. Nil means verified TLS with the URL host as
	// the server name, unless InsecureTLS is set. A non-nil config is cloned;
	// an empty ServerName is filled in from the URL host and an unset
	// MinVersion is raised to TLS 1.2.
	TLSConfig *tls.Config
	// InsecureTLS opts into skipping certificate verification for rtsps
	// (self-signed cameras). Ignored when TLSConfig is non-nil.
	InsecureTLS bool
	// UserAgent is sent on every request. Zero uses DefaultUserAgent.
	UserAgent string
	// OnFrame receives every delivered frame on the reader goroutine. It must
	// not block and must not call Describe, Setup, Play, or Wait (Close, Stats
	// and SessionInfo are the callback-safe ones). Frame.Data is valid only
	// for the duration of the call.
	//
	// Frame delivery arrives with track setup in a later change; today no
	// frames are delivered. Once it lands, nil will be allowed and frames will
	// then be counted in Stats without being delivered.
	OnFrame func(audiostream.Frame)
	// Logger is reserved for diagnostics and is not read yet; the package
	// currently emits no log output. When it is wired, URLs will be passed
	// through RedactURL and credentials will never be logged.
	Logger *slog.Logger
}

// applyDefaults fills a zero or negative Timeout and a zero UserAgent with
// their defaults, and normalizes a negative ReadIdle to zero (disabled).
func (c *Config) applyDefaults() {
	if c.ReadIdle < 0 {
		// Normalized to the documented "disabled" value so a negative can
		// never reach a timer. time.NewTicker panics on a non-positive
		// interval, and the keepalive task will build one from this.
		c.ReadIdle = 0
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.UserAgent == "" {
		c.UserAgent = DefaultUserAgent
	}
}

// Track is one media track discovered by Describe and passed to Setup. Both
// verbs arrive in a later change; the type is declared here so the shape is
// visible, and nothing in this package produces or consumes it yet.
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

// SetupOptions controls one Setup. Setup arrives in a later change; nothing in
// this package consumes this type yet. Discard sets up a track whose frames are
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

// parseTarget resolves cfg.URL into a target. It validates the scheme (rtsp or
// rtsps) and the port range, supplies the default port when absent, extracts
// credentials (a non-empty URL userinfo overrides Config) and rejects CR, LF
// and NUL in them, and strips the userinfo and fragment from the request
// URL. It returns ErrInvalidURL (wrapped) on any malformed input.
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
	} else {
		// url.Parse only guarantees the port is digits, so an out-of-range
		// value reaches DialContext and surfaces as a connection error rather
		// than the documented ErrInvalidURL.
		n, perr := strconv.Atoi(port)
		if perr != nil || n < 1 || n > 65535 {
			return target{}, fmt.Errorf("%w: port %q out of range", ErrInvalidURL, port)
		}
	}

	username, password := cfg.Username, cfg.Password
	// A wholly empty userinfo ("rtsp://@host" or "rtsp://:@host", what a URL
	// template produces when its substitution variables are unset) is treated
	// as absent rather than as an override, so it cannot silently discard the
	// credentials the caller supplied in Config. Note url.User is non-nil for
	// both of those and User.String() is ":" for the second, so neither a nil
	// check nor an empty-string check catches them.
	//
	// Gating on the username alone would be too broad: "rtsp://:secret@host"
	// carries a real password-only credential, which some cameras accept, and
	// dropping it would surface as an unexplainable 401.
	if u.User != nil {
		urlUser := u.User.Username()
		urlPass, _ := u.User.Password()
		if urlUser != "" || urlPass != "" {
			username, password = urlUser, urlPass
		}
	}
	// Userinfo is percent-decoded, so url.Parse's rejection of raw control
	// characters does not cover "%0D%0A". These values flow into an
	// Authorization header; MarshalRequest would catch CRLF one hop later, but
	// the boundary that extracts them from an untrusted URL is where they
	// should be rejected.
	if strings.ContainsAny(username, "\r\n\x00") || strings.ContainsAny(password, "\r\n\x00") {
		return target{}, fmt.Errorf("%w: CR, LF or NUL in credentials", ErrInvalidURL)
	}

	reqURL := *u
	reqURL.User = nil
	// A fragment is a client-side construct and has no meaning on an RTSP
	// request line; leaving it on would send it to the server verbatim.
	reqURL.Fragment = ""
	reqURL.RawFragment = ""
	return target{
		tls:        tlsOn,
		address:    net.JoinHostPort(host, port),
		requestURL: reqURL.String(),
		serverName: host,
		username:   username,
		password:   password,
	}, nil
}
