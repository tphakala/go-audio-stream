package rtsp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/depacket/latm"
	"github.com/tphakala/go-audio-stream/rtsp/sdp"
)

// ErrNotSDP is returned by Describe when the DESCRIBE response lacks an
// application/sdp Content-Type.
var ErrNotSDP = errors.New("rtsp: DESCRIBE response is not application/sdp")

// sdpContentType is the media type a DESCRIBE body must carry, checked after
// stripping any ";charset=" style parameter.
const sdpContentType = "application/sdp"

// describedTrack is the internal per-track descriptor Describe resolves from
// the SDP, indexed by track ID, so Setup can build the pipeline without
// re-parsing. It carries the AAC fmtp parameters the public Track omits.
type describedTrack struct {
	control string
	codec   audiostream.Codec
	// payloadType is the RTP payload type the codec was resolved from, or
	// payloadTypeUnknown when the m= line named no format. It is the FIRST
	// format the m= line lists, recognized or not, which is why the pipeline
	// treats it as the expected payload type rather than an enforced one: see
	// track.acceptPayloadType.
	payloadType int
	clockRate   int
	media       audiostream.MediaKind
	aac         *sdp.AACParams
	// latmDepack is the depacketizer resolveLATMASC built once from an
	// out-of-band MP4A-LATM StreamMuxConfig at Describe, reused verbatim by
	// configureLATM at Setup so the config is parsed once, not twice. It is nil
	// for every non-LATM or in-band track and nil when the out-of-band parse
	// failed. latmResolved records that Describe ran that parse (success or
	// failure) so Setup neither re-parses nor re-logs; when false, Setup builds
	// the depacketizer itself as before: the in-band path, and a
	// direct-construction unit test that leaves latmResolved unset.
	latmDepack   *latm.Depacketizer
	latmResolved bool
}

// payloadTypeUnknown is describedTrack.payloadType when the SDP named no usable
// format, matching the sentinel sdp.DescribedTrack.PayloadType uses. A track
// with no declared payload type simply has nothing to compare an observed one
// against, so no mismatch is ever reported for it.
const payloadTypeUnknown = -1

// Describe issues a DESCRIBE, parses the SDP body, resolves the session base
// and per-track control URLs, and returns the discovered tracks. It is legal
// only in the idle state, else it returns a *StateError (matching
// ErrInvalidState).
//
// The transition happens only on success: a Describe that fails while parsing
// the response leaves the client idle, so it may be retried. Once one has
// succeeded, a second call returns a *StateError.
//
// Errors it can return: *audiostream.RedirectError for a 3xx, which is never
// followed; an error matching ErrAuthFailed when a 401 could not be answered or
// survived the retries, which wraps the *UnauthorizedError so the challenge is
// still readable with errors.As;
// *ResponseError for any other non-2xx; ErrNotSDP when the response body is
// not application/sdp. All of them leave the session open so the caller may
// Close or retry.
//
// A 2xx whose body parses but declares no media sections yields an empty track
// slice and a nil error, and still transitions to the described state.
func (c *Client) Describe(ctx context.Context) ([]Track, error) {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()

	c.mu.Lock()
	if serr := c.requireState(methodDescribe); serr != nil {
		c.mu.Unlock()
		return nil, serr
	}
	describeURL := c.baseURL
	c.mu.Unlock()

	req := &Request{Method: methodDescribe, URL: describeURL, Header: Header{}}
	req.Header.Set("Accept", sdpContentType)
	resp, err := c.do(ctx, req)
	if err != nil {
		return nil, err
	}
	if cerr := checkSDPContentType(resp.Header.Get("Content-Type")); cerr != nil {
		return nil, cerr
	}

	dr, rerr := resolveTracks(describeURL, resp, c.cfg.Logger)
	if rerr != nil {
		return nil, rerr
	}

	// Test-only seam, nil in production: it exists solely to make the guard
	// below reachable deterministically. A test installs a hook here to trip
	// termErr in the exact window between the round trip resolving and the
	// commit lock being taken, which is where a reader-driven shutdown can win.
	if c.afterDescribeRoundTrip != nil {
		c.afterDescribeRoundTrip()
	}

	c.mu.Lock()
	if c.termErr != nil {
		terr := c.termErr
		c.mu.Unlock()
		return nil, terr
	}
	c.baseURL = dr.base
	c.described = dr.described
	c.sdpSessionName = dr.name
	c.sdpTool = dr.tool
	c.commitState(methodDescribe)
	c.mu.Unlock()
	return dr.tracks, nil
}

// checkSDPContentType validates a DESCRIBE Content-Type: the base media type
// (everything before the first ";") must be application/sdp, tolerating a
// trailing parameter such as ";charset=utf-8". A missing or mismatched type is
// ErrNotSDP.
func checkSDPContentType(value string) error {
	if value == "" {
		return fmt.Errorf("%w: response carried no Content-Type", ErrNotSDP)
	}
	base := value
	if i := strings.IndexByte(base, ';'); i >= 0 {
		base = base[:i]
	}
	if !strings.EqualFold(strings.TrimSpace(base), sdpContentType) {
		return fmt.Errorf("%w: Content-Type is %q", ErrNotSDP, value)
	}
	return nil
}

// describeResult is everything Describe commits from one DESCRIBE: the public
// tracks, the internal per-track descriptors, the resolved aggregate control
// base, and the session identity (SDP s= name and a=tool) surfaced through
// SessionInfo for diagnostics. Grouping them keeps resolveTracks to a single
// value return as the fields grew.
type describeResult struct {
	tracks    []Track
	base      string
	described []describedTrack
	name      string // SDP session name (s=), "" if absent; untrusted free text
	tool      string // SDP a=tool, "" if absent; untrusted free text
}

// resolveTracks resolves the session base URL and builds the public Track slice
// plus the internal descriptor slice from a DESCRIBE response. The session base
// is Content-Base (or Content-Location, or the request URL) further resolved
// against a non-"*" session-level a=control. Each track's control is resolved
// against that base. logger receives a warning when an out-of-band LATM
// track's StreamMuxConfig cannot be parsed; see resolveLATMASC.
func resolveTracks(describeURL string, resp *Response, logger *slog.Logger) (describeResult, error) {
	headerBase, err := ResolveBaseURL(describeURL, resp.Header.Get("Content-Base"), resp.Header.Get("Content-Location"))
	if err != nil {
		return describeResult{}, err
	}

	session, err := sdp.Parse(resp.Body)
	if err != nil {
		return describeResult{}, err
	}

	base := headerBase
	if session.Control != "" && session.Control != "*" {
		base, err = ResolveControlURL(headerBase, session.Control)
		if err != nil {
			return describeResult{}, err
		}
	}

	dts := session.Codecs()
	tracks := make([]Track, 0, len(dts))
	described := make([]describedTrack, 0, len(dts))
	for i := range dts {
		dt := &dts[i]
		control, cerr := ResolveControlURL(base, dt.Control)
		if cerr != nil {
			return describeResult{}, cerr
		}
		codec, latmDepack, latmResolved := resolveLATMASC(dt.Codec, logger)
		tracks = append(tracks, Track{
			ID:          i,
			Media:       dt.Media,
			Codec:       codec,
			ClockRate:   dt.ClockRate,
			Channels:    dt.Channels,
			PayloadType: dt.PayloadType,
			Control:     control,
			FMTP:        dt.FMTP,
		})
		described = append(described, describedTrack{
			control:      control,
			codec:        codec,
			payloadType:  dt.PayloadType,
			clockRate:    dt.ClockRate,
			media:        dt.Media,
			aac:          dt.AAC,
			latmDepack:   latmDepack,
			latmResolved: latmResolved,
		})
	}
	return describeResult{tracks: tracks, base: base, described: described, name: session.Name, tool: session.Tool}, nil
}

// resolveLATMASC extracts the out-of-band AudioSpecificConfig for a
// CodecMP4ALATM track whose config is carried in the SDP config= rather than
// in-band, so the Track Describe returns already carries the ASC a consumer
// needs to initialize its decoder, without waiting for OnCodecUpdate. Every
// other codec, and an in-band LATM track (MuxConfigPresent true), is returned
// unchanged: an in-band ASC is not known until the stream carries it.
//
// This is the only place rtsp/describe.go touches depacket/latm: rtsp/sdp
// does not parse the StreamMuxConfig itself, to keep the SDP parser free of
// that dependency.
//
// It returns the (possibly ASC-augmented) codec, the depacketizer it built,
// and whether it ran the out-of-band parse. The depacketizer is stashed on the
// describedTrack and adopted verbatim by configureLATM at Setup, so an
// out-of-band StreamMuxConfig is parsed once, not once here and again at Setup.
// A StreamMuxConfig latm.New cannot parse (an unsupported, malformed, or empty
// out-of-band config) logs a warning ONCE here and returns a nil depacketizer
// with resolved true, so Setup falls back to raw delivery without re-parsing or
// re-logging; AudioSpecificConfig is left nil. A non-LATM or in-band track
// (MuxConfigPresent true) is returned unchanged with resolved false, so Setup
// builds the depacketizer itself: an in-band ASC is not known until the stream
// carries it.
func resolveLATMASC(codec audiostream.Codec, logger *slog.Logger) (audiostream.Codec, *latm.Depacketizer, bool) {
	lc, ok := codec.(audiostream.CodecMP4ALATM)
	if !ok || lc.MuxConfigPresent {
		return codec, nil, false
	}
	dp, err := latm.New(latm.Config{MuxConfigPresent: false, StreamMuxConfig: lc.StreamMuxConfig})
	if err != nil {
		logWarn(logger, "latm out-of-band StreamMuxConfig invalid; AudioSpecificConfig unavailable at Describe", "error", err)
		return codec, nil, true
	}
	lc.AudioSpecificConfig = append([]byte(nil), dp.AudioSpecificConfig()...)
	return lc, dp, true
}
