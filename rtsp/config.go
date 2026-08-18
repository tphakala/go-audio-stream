package rtsp

import (
	"crypto/tls"
	"errors"
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

// TransportPreference selects the media transport the client negotiates at
// Setup. The zero value PreferTCP reproduces the phase 1 TCP-interleaved
// behavior exactly, so existing callers are unaffected.
type TransportPreference uint8

const (
	// PreferTCP negotiates only RTP/AVP/TCP interleaved transport. It is the
	// zero value and the phase 1 default.
	PreferTCP TransportPreference = iota
	// PreferUDP negotiates only RTP/AVP unicast UDP. A server that rejects
	// the UDP SETUP yields ErrUDPSetupRejected with no fallback.
	PreferUDP
	// PreferUDPThenTCP tries UDP first and, if the first track's UDP SETUP is
	// rejected, transparently re-issues it over TCP interleaved and pins TCP
	// for the session.
	PreferUDPThenTCP
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
	// negative disables it. Any interleaved frame counts, including one this
	// client then drops: the watchdog answers "is the peer still sending", not
	// "is the audio still usable".
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
	// not block and must not call Describe, Setup, Play, or Wait (Close, Stats,
	// SessionInfo and Info are the callback-safe ones). Frame.Data is valid only
	// for the duration of the call.
	//
	// Nil is allowed: packets are still parsed and counted in Stats, they are
	// simply not delivered. That is the shape a caller wants for a track it
	// only wants statistics from; SetupOptions.Discard is the cheaper one for a
	// track it wants nothing from at all, since a discard track is never
	// parsed.
	OnFrame func(audiostream.Frame)
	// Logger receives diagnostics for conditions this package handles rather
	// than fails on: a track degraded to raw delivery by an unsupported codec
	// or a quirky fmtp, and a SETUP response whose Session header is missing or
	// inconsistent. Credentials are never logged.
	Logger *slog.Logger
	// Transport selects the media transport the client negotiates at Setup.
	// The zero value, PreferTCP, reproduces the phase 1 TCP-interleaved
	// behavior exactly.
	Transport TransportPreference
	// OnCodecUpdate, when non-nil, is called on the delivery goroutine (the
	// reader goroutine in TCP mode, or the per-track UDP receive goroutine
	// when the M6 UDP transport is in use) the first time a track's codec
	// configuration is resolved from the media stream, and again if it
	// changes mid-stream. Its purpose is to hand the consumer a codec value
	// whose AudioSpecificConfig is now known when it was not known at
	// Describe time, as happens for in-band MP4A-LATM (cpresent=1). It does
	// NOT fire for a config already known at Describe (out-of-band LATM or
	// MPEG4-GENERIC AAC), where the ASC is already on the Track.
	//
	// Ordering guarantee: for a given track, OnCodecUpdate fires BEFORE the
	// OnFrame call for the first access unit decoded under the newly
	// resolved config, so a consumer can initialize its decoder before the
	// first frame reaches it. Like OnFrame, it runs on the delivery
	// goroutine, must not block, and must not call Describe, Setup, Play, or
	// Wait (Close and Stats are callback-safe). The CodecUpdate's Codec and
	// any slices it carries are owned by the callee only for the duration of
	// the call; copy AudioSpecificConfig to retain it.
	OnCodecUpdate func(audiostream.CodecUpdate)
}

// applyDefaults fills a zero or negative Timeout and a zero UserAgent with
// their defaults, and normalizes a negative ReadIdle to zero (disabled).
func (c *Config) applyDefaults() {
	if c.ReadIdle < 0 {
		// Normalized to the documented "disabled" value so that every later
		// reader can test it with a single > 0, rather than each having to
		// decide for itself what a negative window means.
		c.ReadIdle = 0
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.UserAgent == "" {
		c.UserAgent = DefaultUserAgent
	}
}

// Track is one media track discovered by Describe and passed to Setup. The ID
// selects the retained descriptor that builds the depacketizer while Control
// names the stream the SETUP addresses, so Setup checks that the two still
// agree with what Describe resolved and returns ErrUnknownTrack when they do
// not. Pass Tracks through unmodified; pairing one track's ID with another's
// Control would otherwise set up one stream and decode it as another.
type Track struct {
	// ID is a stable per-session id (the SDP media index).
	ID int
	// Media is the track media kind (audio, video, other).
	Media audiostream.MediaKind
	// Codec is the resolved payload codec.
	Codec audiostream.Codec
	// ClockRate is the RTP clock rate in Hz from the rtpmap.
	ClockRate int
	// Channels is the channel count from the rtpmap. It is 1 rather than 0
	// whenever the rtpmap names an encoding but omits the channel segment,
	// including for an encoding this package does not decode and for a video
	// track, because the SDP layer defaults it from the presence of the
	// encoding name alone. It is 0 only when there was no rtpmap to read.
	// Do not use it to detect an unsupported codec; compare Codec instead.
	Channels int
	// PayloadType is the RTP payload type from the m= line (0-127), or -1 when
	// the media section listed no format, matching sdp.DescribedTrack. It
	// identifies the track on the wire, and it is the one wire identity left
	// for a track resolved from a static payload type with no a=rtpmap, where
	// Codec is CodecUnknown and ClockRate and Channels are 0.
	PayloadType int
	// Control is the resolved absolute control URL for this track.
	Control string
	// FMTP is the raw a=fmtp parameter string for this track's payload type
	// (everything after the payload type), "" when the SDP carried no fmtp for
	// it. It is a diagnostic aid: the depacketizer uses the parsed Codec, not
	// this string, but the raw parameters (for example the AAC sizelength and
	// mode) are what a maintainer needs to reproduce a codec issue.
	FMTP string
}

// SetupOptions controls one Setup. Discard sets up a track whose frames are
// dropped inside the reader without per-packet allocation or delivery, which is
// how a caller keeps a video track's channels bound (so the server streams the
// session it negotiated) without paying to parse it.
type SetupOptions struct {
	// Discard drops this track's frames in the reader, counting them in Stats
	// but never depacketizing or delivering them.
	Discard bool
}

// ErrUDPSetupRejected is returned by Setup under PreferUDP when the server
// declines the UDP transport (non-2xx, 461 Unsupported Transport, or a
// Transport response with no usable server_port). It is also returned under
// PreferUDPThenTCP for a 2xx response carrying an unusable server_port: the
// server has already allocated the session, so this is not a clean rejection
// the client may silently retry over TCP.
var ErrUDPSetupRejected = errors.New("rtsp: server rejected UDP transport")

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
