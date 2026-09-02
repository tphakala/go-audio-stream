package rtsp

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/internal/urltarget"
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
	// any slices it carries are read-only and are owned by the callee only for
	// the duration of the call; copy AudioSpecificConfig to retain it, and do
	// not modify it in place.
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
// session it negotiated) without paying to parse it. G726Packing overrides the
// codeword bit order a CodecG726 track decodes with, for a device whose rtpmap
// encoding name does not match how it actually packs; see G726PackingOverride.
type SetupOptions struct {
	// Discard drops this track's frames in the reader, counting them in Stats
	// but never depacketizing or delivering them.
	Discard bool
	// G726Packing overrides the codeword packing this track's rtpmap encoding
	// name resolved to. The zero value trusts the SDP. See G726PackingOverride.
	G726Packing G726PackingOverride
}

// G726PackingOverride selects the codeword bit order used to unpack a
// CodecG726 track, overriding what the SDP rtpmap encoding name resolved to.
// The zero value G726PackingFromSDP trusts the SDP, so existing callers are
// unaffected.
//
// It exists because nothing in the stream distinguishes the two orders: they
// carry the same codewords in reversed bit numbering, so decoding with the
// wrong one does not fail, it just yields plausible but wrong audio. The rtpmap
// encoding name is the only out-of-band signal (G726-NN for the RFC 3551 order,
// AAL2-G726-NN for the AAL2 order), and a track resolved from the static RTP
// payload type 2 carries no signal at all, so the plain RFC 3551 order is merely
// assumed there. This override is the only fix for two failure modes: a device
// advertising AAL2-G726-NN while actually packing the RFC 3551 order (a real
// vendor bug class), and a payload-type-2 device that in fact packs AAL2. The
// two differ in mechanism: RFC 3551 section 6 deprecates payload type 2 for
// exactly this absence of a signal, whereas the vendor-bug case is a signal that
// is present and wrong. Either way only the caller can correct it.
//
// Diagnosing it is the first step, and the bundled stream-doctor names the
// resolved packing in its report. A track that sounds like distorted noise but
// decodes without error is the signature; setting the other packing here is the
// fix.
//
// The override applies only to a track the SDP already resolved to CodecG726.
// It does not rescue a CodecUnknown track, because the bit rate comes from the
// same encoding name, and a G.726 track advertising a non-8 kHz clock or more
// than one channel is left unresolved deliberately (the decoder carries a
// single adaptive state at a fixed 8 kHz clock, so it could not be decoded or
// timed correctly whatever the packing).
//
// Track.Format reports the codec Describe resolved, so it keeps reporting the
// packing the SDP advertised even when this overrides it. That asymmetry is
// harmless in practice: the library decodes G.726 itself and delivers s16le
// PCM, so a consumer reads SampleRate and Channels from the format and has no
// reason to inspect the packing, which is a decoder detail once the audio is
// already PCM.
type G726PackingOverride uint8

const (
	// G726PackingFromSDP uses the packing the rtpmap encoding name resolved to:
	// RFC 3551 order for the plain G726-NN names, AAL2 order for the
	// AAL2-G726-NN ones. It is the zero value and the default.
	G726PackingFromSDP G726PackingOverride = iota
	// G726PackingForceRFC3551 forces least-significant-bit-first codewords
	// (RFC 3551 section 4.5.4), whatever the encoding name said.
	G726PackingForceRFC3551
	// G726PackingForceAAL2 forces most-significant-bit-first codewords (the
	// AAL2-G726 form of ITU-T I.366.2 Annex E), whatever the encoding name said.
	G726PackingForceAAL2
)

// resolve maps the override against the packing the SDP reported. It returns the
// packing the decoder should use and whether o was one of the defined override
// constants. Unifying the mapping and the validity check in one switch is
// deliberate: a fourth constant added to this method cannot be registered for
// resolution but missed for validation (or the reverse), the drift a separate
// packing/valid pair invited. An out-of-range value resolves to the SDP packing
// (fail-open, so a bad config value never fails Setup) and reports ok=false,
// which configureG726 turns into an out-of-range warning so the value is not
// silently inert; the zero value G726PackingFromSDP also defers to the SDP but
// reports ok=true, so it draws no warning.
func (o G726PackingOverride) resolve(fromSDP audiostream.G726Packing) (packing audiostream.G726Packing, ok bool) {
	switch o {
	case G726PackingFromSDP:
		return fromSDP, true
	case G726PackingForceRFC3551:
		return audiostream.G726PackingRFC3551, true
	case G726PackingForceAAL2:
		return audiostream.G726PackingAAL2, true
	default:
		return fromSDP, false
	}
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
// URL. It returns ErrInvalidURL (wrapped) on any malformed input. The
// security-relevant scheme-independent core, leak-safe URL parsing and
// credential resolution, is shared with the other network sources via
// internal/urltarget so a fix to it cannot land in one copy only; only the
// rtsp-specific scheme set, default-port supply, and dial-address building stay
// here.
func parseTarget(cfg *Config) (target, error) {
	u, err := urltarget.ParseURL(cfg.URL)
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

	username, password, err := urltarget.ResolveCredentials(u, cfg.Username, cfg.Password)
	if err != nil {
		return target{}, fmt.Errorf("%w: %w", ErrInvalidURL, err)
	}

	return target{
		tls:        tlsOn,
		address:    net.JoinHostPort(host, port),
		requestURL: urltarget.StripUserinfoFragment(u),
		serverName: host,
		username:   username,
		password:   password,
	}, nil
}
