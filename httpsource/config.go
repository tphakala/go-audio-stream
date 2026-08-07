package httpsource

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
)

// Client defaults. A zero Config field falls back to these.
const (
	// DefaultTimeout bounds the whole open phase (connect, TLS handshake, and
	// the response headers) when Config.Timeout is zero.
	DefaultTimeout = 10 * time.Second
	// DefaultUserAgent is sent on the request when Config.UserAgent is empty. It
	// matches the value the rtsp client uses, so both sources identify the
	// library the same way.
	DefaultUserAgent = "go-audio-stream/0.1"
)

// Endianness selects the byte order of a raw PCM source. It applies only to raw
// audio (audio/L16 and unlabeled embedded PCM); a WAV stream is little-endian
// by definition and ignores it.
type Endianness uint8

const (
	// EndianUnspecified takes the raw default, which is little-endian for every
	// raw PCM stream this source carries. RFC 3551 defines audio/L16 as
	// big-endian, but real HTTP embedded microphones (for example
	// esp32-audio-streamer's /stream.pcm) send native little-endian while labeling
	// the stream audio/L16, so defaulting audio/L16 to little-endian matches the
	// devices in the field; unlabeled embedded PCM is native little-endian for the
	// same reason. Set EndianBig for a spec-strict big-endian audio/L16 source.
	// This applies only to the HTTP source; the RTP/RTSP L16 path (rtsp package)
	// stays big-endian per RFC 3551.
	EndianUnspecified Endianness = iota
	// EndianLittle forces little-endian interpretation of the source samples.
	EndianLittle
	// EndianBig forces big-endian interpretation of the source samples, for a
	// spec-strict RFC 3551 audio/L16 stream.
	EndianBig
)

// String returns a stable label for logs and tests.
func (e Endianness) String() string {
	switch e {
	case EndianUnspecified:
		return "unspecified"
	case EndianLittle:
		return "little"
	case EndianBig:
		return "big"
	default:
		return "unknown"
	}
}

// PCMFormat describes a raw PCM source that carries no self-describing header.
// It supplies the sample rate and channel count for an audio/L16 response whose
// MIME parameters omit them, and the whole shape for an unlabeled
// application/octet-stream, audio/pcm, or headerless stream. It is ignored for
// a WAV stream, whose fmt chunk is authoritative, and for an audio/L16 response
// whose rate and channels parameters are present.
type PCMFormat struct {
	// SampleRate is the sample rate in Hz. Zero means "not supplied".
	SampleRate int
	// Channels is the channel count. Zero means "not supplied".
	Channels int
	// Endian overrides the byte order implied by the stream. EndianUnspecified
	// follows the implication; see Endianness.
	Endian Endianness
}

// Config configures an Open. OnFrame is registered here, before the stream is
// opened, so frame delivery is race-free no matter how early the server starts
// sending. A Config value carries credentials; do not log it.
type Config struct {
	// URL is the http:// or https:// target. Credentials may be embedded in the
	// userinfo. This package never logs it, but Config has no String method, so
	// printing a Config with %v exposes both the userinfo and Password verbatim.
	URL string
	// Username and Password supply HTTP credentials when the URL carries no
	// non-empty userinfo; a non-empty URL userinfo overrides them (a wholly
	// empty userinfo, "http://@host", is treated as absent and does not).
	// Never logged. They answer either a Basic (RFC 7617) or a Digest (RFC 7616,
	// with RFC 2069 legacy) challenge: Basic is sent preemptively when the
	// connection permits it (see AllowInsecureAuth), Digest is computed in
	// response to the server's WWW-Authenticate challenge.
	Username string
	Password string
	// Timeout bounds the whole open phase: the TCP connect, the TLS handshake
	// for https, and the wait for the response headers. Zero or negative uses
	// DefaultTimeout. It does not bound the streaming body; ReadIdle does.
	Timeout time.Duration
	// ReadIdle is the read-idle watchdog window: once streaming, no body data
	// within ReadIdle ends Wait with audiostream.ErrReadTimeout. Zero or
	// negative disables it. Any read counts, including one this source then
	// drops for a nil OnFrame: the watchdog answers "is the peer still sending",
	// not "is the audio still usable".
	ReadIdle time.Duration
	// TLSConfig is used for https. Nil means verified TLS with the URL host as
	// the server name, unless InsecureTLS is set. A non-nil config is cloned; an
	// empty ServerName is filled in from the URL host and an unset MinVersion is
	// raised to TLS 1.2.
	TLSConfig *tls.Config
	// InsecureTLS opts into skipping certificate verification for https
	// (self-signed endpoints). Ignored when TLSConfig is non-nil.
	InsecureTLS bool
	// AllowInsecureAuth permits sending Basic credentials over a plaintext http
	// connection. It is false by default, which is secure: over plaintext http
	// the first request is sent bare, and if the server answers with a Basic
	// challenge, Open returns ErrInsecureAuth rather than transmitting the
	// password in the clear. Setting it lets Basic go out (sent preemptively,
	// with a warning logged). Credentials over https are always allowed and
	// unaffected by this flag. This gate covers only Basic: a Digest challenge
	// is always answered, over plaintext included, because Digest never puts the
	// password on the wire. Set it only for a trusted network where plaintext
	// Basic is acceptable.
	AllowInsecureAuth bool
	// UserAgent is sent on the request. Empty uses DefaultUserAgent.
	UserAgent string
	// Format describes a raw PCM source. It is consulted only when the stream
	// does not fully describe itself: an audio/L16 response missing its rate or
	// channels parameters, or an unlabeled raw stream. See PCMFormat.
	Format PCMFormat
	// OnFrame receives every delivered frame on the reader goroutine. It must
	// not block and must not call Wait (Close, Stats, Info and Codec are the
	// callback-safe ones). Frame.Data is valid only for the duration of the
	// call; consumers that retain audio must copy.
	//
	// Nil is allowed: bytes are still counted in Stats, they are simply not
	// delivered. That is the shape a caller wants for a source it only wants
	// statistics or liveness from.
	OnFrame func(audiostream.Frame)
	// Logger receives diagnostics for conditions this source handles rather than
	// fails on. Today that is a single warning: sending Basic credentials over a
	// plaintext http connection when AllowInsecureAuth permits it. The
	// credential-stripped URL is logged, never the credentials themselves.
	Logger *slog.Logger
}

// applyDefaults fills a zero or negative Timeout and an empty UserAgent with
// their defaults, and normalizes a negative ReadIdle to zero (disabled).
func (c *Config) applyDefaults() {
	if c.ReadIdle < 0 {
		// Normalized to the documented "disabled" value so a single > 0 test
		// covers it everywhere, rather than each reader deciding for itself what
		// a negative window means.
		c.ReadIdle = 0
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.UserAgent == "" {
		c.UserAgent = DefaultUserAgent
	}
}

// target is the resolved request destination parsed from a Config: whether it
// is https, the request URL with userinfo and fragment stripped, the TLS server
// name, and the credentials Basic auth will use.
type target struct {
	tls        bool
	requestURL string
	serverName string
	username   string
	password   string
}

// parseTarget resolves cfg.URL into a target. It validates the scheme (http or
// https) and the port range, extracts credentials (a non-empty URL userinfo
// overrides Config) and rejects CR, LF and NUL in them, and strips the userinfo
// and fragment from the request URL so neither reaches the server or Info. It
// returns ErrInvalidURL (wrapped) on any malformed input.
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
	switch strings.ToLower(u.Scheme) {
	case "http":
		tlsOn = false
	case "https":
		tlsOn = true
	default:
		return target{}, fmt.Errorf("%w: unsupported scheme %q", ErrInvalidURL, u.Scheme)
	}

	host := u.Hostname()
	if host == "" {
		return target{}, fmt.Errorf("%w: missing host", ErrInvalidURL)
	}
	if port := u.Port(); port != "" {
		// url.Parse only guarantees the port is digits, so an out-of-range value
		// would otherwise reach the dialer and surface as a connection error
		// rather than the documented ErrInvalidURL.
		n, perr := strconv.Atoi(port)
		if perr != nil || n < 1 || n > 65535 {
			return target{}, fmt.Errorf("%w: port %q out of range", ErrInvalidURL, port)
		}
	}

	username, password := cfg.Username, cfg.Password
	// A wholly empty userinfo ("http://@host" or "http://:@host", what a URL
	// template produces when its substitution variables are unset) is treated as
	// absent rather than as an override, so it cannot silently discard the
	// credentials the caller supplied in Config. url.User is non-nil for both of
	// those, so neither a nil check nor an empty-string check on User.String()
	// catches them. Gating on the username alone would be too broad:
	// "http://:secret@host" carries a real password-only credential.
	if u.User != nil {
		urlUser := u.User.Username()
		urlPass, _ := u.User.Password()
		if urlUser != "" || urlPass != "" {
			username, password = urlUser, urlPass
		}
	}
	// Userinfo is percent-decoded, so url.Parse's rejection of raw control
	// characters does not cover "%0D%0A". These values flow into an
	// Authorization header via SetBasicAuth; rejecting them at the boundary that
	// extracts them from an untrusted URL is where it belongs.
	if strings.ContainsAny(username, "\r\n\x00") || strings.ContainsAny(password, "\r\n\x00") {
		return target{}, fmt.Errorf("%w: CR, LF or NUL in credentials", ErrInvalidURL)
	}

	reqURL := *u
	reqURL.User = nil
	// A fragment is a client-side construct with no meaning on the wire; leaving
	// it on would send it to the server verbatim and expose it through Info.
	reqURL.Fragment = ""
	reqURL.RawFragment = ""
	return target{
		tls:        tlsOn,
		requestURL: reqURL.String(),
		serverName: host,
		username:   username,
		password:   password,
	}, nil
}
