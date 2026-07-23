package rtsp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	audiostream "github.com/tphakala/go-audio-stream"
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
	control   string
	codec     audiostream.Codec
	clockRate int
	media     audiostream.MediaKind
	aac       *sdp.AACParams
}

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
// followed; *UnauthorizedError for a 401, which the caller must currently
// answer itself because no authentication retry is wired into this verb yet;
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
	if c.state != stateIdle {
		st := c.state.String()
		c.mu.Unlock()
		return nil, &StateError{Method: methodDescribe, State: st}
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

	tracks, base, described, rerr := resolveTracks(describeURL, resp)
	if rerr != nil {
		return nil, rerr
	}

	c.mu.Lock()
	if c.termErr != nil {
		terr := c.termErr
		c.mu.Unlock()
		return nil, terr
	}
	c.baseURL = base
	c.described = described
	c.state = stateDescribed
	c.mu.Unlock()
	return tracks, nil
}

// do performs one request round-trip and classifies the response status into
// the error the caller acts on: nil on 2xx, *audiostream.RedirectError on a
// 3xx, *UnauthorizedError on a 401, or *ResponseError otherwise. A transport
// failure (already funneled into shutdown by roundTrip) is returned with a nil
// response. The 401 auth-retry wrapper is layered over this in a later task.
func (c *Client) do(ctx context.Context, req *Request) (*Response, error) {
	resp, err := c.roundTrip(ctx, req)
	if err != nil {
		return nil, err
	}
	return resp, ClassifyStatus(resp)
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

// resolveTracks resolves the session base URL and builds the public Track slice
// plus the internal descriptor slice from a DESCRIBE response. The session base
// is Content-Base (or Content-Location, or the request URL) further resolved
// against a non-"*" session-level a=control. Each track's control is resolved
// against that base.
func resolveTracks(describeURL string, resp *Response) ([]Track, string, []describedTrack, error) {
	headerBase, err := ResolveBaseURL(describeURL, resp.Header.Get("Content-Base"), resp.Header.Get("Content-Location"))
	if err != nil {
		return nil, "", nil, err
	}

	session, err := sdp.Parse(resp.Body)
	if err != nil {
		return nil, "", nil, err
	}

	base := headerBase
	if session.Control != "" && session.Control != "*" {
		base, err = ResolveControlURL(headerBase, session.Control)
		if err != nil {
			return nil, "", nil, err
		}
	}

	dts := session.Codecs()
	tracks := make([]Track, 0, len(dts))
	described := make([]describedTrack, 0, len(dts))
	for i := range dts {
		dt := &dts[i]
		control, cerr := ResolveControlURL(base, dt.Control)
		if cerr != nil {
			return nil, "", nil, cerr
		}
		tracks = append(tracks, Track{
			ID:        i,
			Media:     dt.Media,
			Codec:     dt.Codec,
			ClockRate: dt.ClockRate,
			Channels:  dt.Channels,
			Control:   control,
		})
		described = append(described, describedTrack{
			control:   control,
			codec:     dt.Codec,
			clockRate: dt.ClockRate,
			media:     dt.Media,
			aac:       dt.AAC,
		})
	}
	return tracks, base, described, nil
}
