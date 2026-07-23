package rtsp

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
)

// Sentinel errors from Transport and Session header parsing.
var (
	// ErrMalformedTransport means the Transport header value has no
	// transport-spec token.
	ErrMalformedTransport = errors.New("rtsp: malformed transport header")
	// ErrNoInterleaved means the parsed transport carries no interleaved
	// channel pair.
	ErrNoInterleaved = errors.New("rtsp: no interleaved channel pair")
	// ErrBadChannelPair means the interleaved channel pair is not a valid
	// consecutive pair within 0..255.
	ErrBadChannelPair = errors.New("rtsp: invalid interleaved channel pair")
	// ErrChannelConflict means an interleaved channel is already claimed by
	// an earlier track on the connection.
	ErrChannelConflict = errors.New("rtsp: interleaved channel already claimed")
)

// TransportHeader is a parsed RTSP Transport header for the TCP-interleaved
// profile. UDP-specific fields are out of scope for phase 1 and are added
// in phase 2.
type TransportHeader struct {
	// Protocol is the transport-spec token, for example "RTP/AVP/TCP".
	Protocol string
	// Interleaved is true when an interleaved=a-b pair was present and both
	// sides were numeric. It reports only that a pair was parsed, NOT that
	// the pair is usable: the range and consecutiveness rules are enforced
	// by InterleavedChannels, which is what callers must use before trusting
	// the channel numbers.
	Interleaved bool
	// RTPChannel and RTCPChannel are the interleaved channel numbers as
	// received, set only when Interleaved is true and still unvalidated.
	RTPChannel  int
	RTCPChannel int
	// Unicast is true when unicast delivery was indicated.
	Unicast bool
	// Mode is the mode parameter uppercased ("PLAY"), "" when absent.
	Mode string
	// SSRC is the ssrc parameter as received (hex text), "" when absent.
	SSRC string
	// Source and Destination are the optional address parameters, "" absent.
	Source      string
	Destination string
}

// ParseTransport parses one Transport header field value (the server's
// accepted transport-spec) into a TransportHeader. Parameters are
// case-insensitive; unknown parameters are ignored; missing fields keep
// their zero value. Any numeric interleaved=a-b pair sets Interleaved,
// including a non-consecutive or out-of-range one; validation is deferred
// to InterleavedChannels so a bad pair is reported as ErrBadChannelPair
// rather than being silently dropped at parse time. It returns
// ErrMalformedTransport when the value has no transport-spec token, and
// never panics.
func ParseTransport(value string) (TransportHeader, error) {
	parts := strings.Split(value, ";")
	protocol := strings.TrimSpace(parts[0])
	if protocol == "" {
		return TransportHeader{}, ErrMalformedTransport
	}

	t := TransportHeader{Protocol: protocol}
	for _, raw := range parts[1:] {
		applyTransportParam(&t, strings.TrimSpace(raw))
	}
	return t, nil
}

// applyTransportParam parses one ";"-separated Transport parameter (already
// trimmed) and applies it to t. Unknown parameters are ignored.
func applyTransportParam(t *TransportHeader, param string) {
	if param == "" {
		return
	}
	name, val, hasEq := strings.Cut(param, "=")
	name = strings.ToLower(strings.TrimSpace(name))
	val = strings.TrimSpace(val)

	switch {
	case !hasEq && name == "unicast":
		t.Unicast = true
	case !hasEq && name == "multicast":
		// Recognized; no dedicated field, Unicast stays false.
	case hasEq && name == "interleaved":
		parseInterleavedParam(t, val)
	case hasEq && name == "mode":
		t.Mode = strings.ToUpper(strings.Trim(val, `"`))
	case hasEq && name == "ssrc":
		t.SSRC = val
	case hasEq && name == "source":
		t.Source = val
	case hasEq && name == "destination":
		t.Destination = val
	}
}

// parseInterleavedParam parses an interleaved=a[-b] value into t. Any pair
// with both sides numeric sets RTPChannel, RTCPChannel, and Interleaved,
// leaving the range and consecutiveness checks to InterleavedChannels; a
// lone channel number sets only RTPChannel and leaves Interleaved false;
// anything else is ignored.
func parseInterleavedParam(t *TransportHeader, val string) {
	rtpStr, rtcpStr, hasDash := strings.Cut(val, "-")
	if hasDash {
		rtp, rerr := strconv.Atoi(strings.TrimSpace(rtpStr))
		rtcp, cerr := strconv.Atoi(strings.TrimSpace(rtcpStr))
		if rerr == nil && cerr == nil {
			t.RTPChannel = rtp
			t.RTCPChannel = rtcp
			t.Interleaved = true
		}
		return
	}
	if rtp, err := strconv.Atoi(strings.TrimSpace(val)); err == nil {
		t.RTPChannel = rtp
	}
}

// InterleavedChannels validates and returns the server-assigned interleaved
// channel pair, enforcing the observed rules: the pair must be present,
// consecutive (rtcp == rtp+1), each in 0..255, and not overlap a channel in
// claimed (any overlap, not just an exact match). claimed is the set of
// channel numbers already assigned to earlier tracks on the connection; it
// may be nil. It returns ErrNoInterleaved, ErrBadChannelPair, or
// ErrChannelConflict on failure. It never panics.
//
//nolint:gocritic // value receiver is the documented M4a API contract that M4b consumes; TransportHeader is a small stateless header value, not a hot-path allocation.
func (t TransportHeader) InterleavedChannels(claimed map[int]bool) (rtp, rtcp int, err error) {
	if !t.Interleaved {
		return 0, 0, ErrNoInterleaved
	}
	if t.RTCPChannel != t.RTPChannel+1 || t.RTPChannel < 0 || t.RTPChannel > 255 || t.RTCPChannel < 0 || t.RTCPChannel > 255 {
		return 0, 0, ErrBadChannelPair
	}
	if claimed[t.RTPChannel] || claimed[t.RTCPChannel] {
		return 0, 0, ErrChannelConflict
	}
	return t.RTPChannel, t.RTCPChannel, nil
}

// BuildTransport builds the client SETUP Transport proposal for the
// TCP-interleaved profile with the given channel pair (RTP then RTCP):
// "RTP/AVP/TCP;unicast;interleaved=<rtp>-<rtcp>". The server may renumber;
// the response pair, validated by InterleavedChannels, is authoritative.
//
// The caller is responsible for passing a pair this package would itself
// accept: 0 <= rtpChannel <= 254 and rtcpChannel == rtpChannel+1. This is a
// pure serializer with no error return, so an out-of-range pair yields a
// header the server will reject rather than a local error. The pair is
// chosen by the client from its own per-session counter, never from remote
// input, which is why the check belongs at the allocation site rather than
// here.
func BuildTransport(rtpChannel, rtcpChannel int) string {
	return "RTP/AVP/TCP;unicast;interleaved=" + strconv.Itoa(rtpChannel) + "-" + strconv.Itoa(rtcpChannel)
}

// DefaultSessionTimeout is the assumed session timeout when the Session
// header omits a timeout parameter (RFC 2326 section 12.37).
const DefaultSessionTimeout = 60 * time.Second

// SessionHeader is a parsed RTSP Session header.
type SessionHeader struct {
	// ID is the bare session identifier with any parameters stripped. Only
	// this is echoed on later requests; strict servers reject the full
	// parameterized string.
	ID string
	// Timeout is the advertised timeout=<seconds>, or DefaultSessionTimeout
	// when the parameter was absent.
	Timeout time.Duration
}

// maxTimeoutSeconds is the largest timeout=<seconds> that still fits in a
// time.Duration. A larger value would overflow the nanosecond multiply and
// wrap to a negative or nonsensical duration, so it is rejected outright.
const maxTimeoutSeconds = math.MaxInt64 / int64(time.Second)

// ParseSession parses a Session header field value: the bare session ID and
// an optional ";timeout=<seconds>" parameter. Other parameters are
// tolerated and ignored. A missing timeout yields DefaultSessionTimeout. An
// empty value yields a zero ID and the default timeout. It never panics.
//
// Only a strictly positive timeout that fits in a time.Duration is accepted.
// A zero, negative, or overflowing value is ignored in favour of
// DefaultSessionTimeout: the keepalive timer derives its interval from this
// duration, and a non-positive interval would make it fire continuously
// (time.NewTicker panics outright), so a malformed or hostile Session header
// must not be able to reach it.
func ParseSession(value string) SessionHeader {
	parts := strings.Split(value, ";")
	h := SessionHeader{
		ID:      strings.TrimSpace(parts[0]),
		Timeout: DefaultSessionTimeout,
	}
	for _, raw := range parts[1:] {
		name, val, hasEq := strings.Cut(strings.TrimSpace(raw), "=")
		if !hasEq || !strings.EqualFold(strings.TrimSpace(name), "timeout") {
			continue
		}
		secs, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64)
		if err != nil || secs <= 0 || secs > maxTimeoutSeconds {
			continue
		}
		h.Timeout = time.Duration(secs) * time.Second
	}
	return h
}

// ParsePublic parses an OPTIONS Public header value into the list of method
// tokens it advertises (uppercased, in order). It never panics.
func ParsePublic(value string) []string {
	var methods []string
	for _, tok := range strings.Split(value, ",") {
		tok = strings.ToUpper(strings.TrimSpace(tok))
		if tok == "" {
			continue
		}
		methods = append(methods, tok)
	}
	return methods
}

// methodGetParameter is the GET_PARAMETER method token KeepaliveMethod
// checks for and returns.
const methodGetParameter = "GET_PARAMETER"

// KeepaliveMethod returns the keepalive method to use given the advertised
// Public methods: "GET_PARAMETER" when it is advertised, otherwise
// "OPTIONS" (research: a well-known media player's server needs
// GET_PARAMETER and will not stay alive on OPTIONS). It never panics.
func KeepaliveMethod(publicMethods []string) string {
	for _, m := range publicMethods {
		if strings.EqualFold(m, methodGetParameter) {
			return methodGetParameter
		}
	}
	return "OPTIONS"
}
